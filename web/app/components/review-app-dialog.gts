import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from './async-view';
import Badge from './badge';
import Banner from './banner';
import ConfirmDialog from './confirm-dialog';
import ErrorMessage from './error-message';
import RelativeTime from './relative-time';
import type ApiService from 'koroku-console/services/api';
import { ApiError } from 'koroku-console/utils/errors';
import Loadable from 'koroku-console/utils/loadable';
import { parentCanCopy } from 'koroku-console/utils/review';
import type {
  App,
  CreatedReviewApp,
  PullRequest,
} from 'koroku-console/utils/types';

interface Signature {
  Args: {
    parent: App;
    onClose: () => unknown;
    /** Called with the new review app; the dialog is then closed. */
    onCreated: (app: CreatedReviewApp) => unknown;
  };
}

function selectable(pr: PullRequest): boolean {
  return !pr.from_fork && pr.review_app === null;
}

function notSelectable(pr: PullRequest): boolean {
  return !selectable(pr);
}

/**
 * Creating a review app (ARCHITECTURE 9.9): one open pull request of the
 * parent's repository, and whether the parent's data is copied.
 */
export default class ReviewAppDialog extends Component<Signature> {
  @service declare api: ApiService;

  pulls = new Loadable<PullRequest[]>(() =>
    this.api.listPullRequests(this.args.parent.slug),
  );

  @tracked chosen: number | null = null;
  @tracked copyData: boolean;
  @tracked busy = false;
  @tracked error: unknown = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.copyData = parentCanCopy(args.parent) === null;
    void this.pulls.load();
  }

  /** Why the parent's data cannot be copied, or null. */
  get copyProblem(): string | null {
    return parentCanCopy(this.args.parent);
  }

  get copyDisabled(): boolean {
    return this.copyProblem !== null || this.busy;
  }

  /** The App lacks Pull requests: Read-only. */
  get permissionMissing(): boolean {
    return (
      this.pulls.error instanceof ApiError &&
      this.pulls.error.code === 'github_permission_missing'
    );
  }

  get confirmLabel(): string {
    return this.chosen === null
      ? 'レビューアプリを作成'
      : `PR #${this.chosen} のレビューアプリを作成`;
  }

  get cannotCreate(): boolean {
    return this.chosen === null;
  }

  choose = (number: number) => {
    this.chosen = number;
  };

  toggleCopy = (event: Event) => {
    this.copyData = (event.target as HTMLInputElement).checked;
  };

  create = async () => {
    if (this.chosen === null) return;
    this.busy = true;
    this.error = null;
    try {
      const app = await this.api.createReviewApp(this.args.parent.slug, {
        pr_number: this.chosen,
        copy_data: this.copyData && this.copyProblem === null,
      });
      await this.args.onCreated(app);
    } catch (error) {
      this.error = error;
    } finally {
      this.busy = false;
    }
  };

  <template>
    <ConfirmDialog
      @title="レビューアプリを作成"
      @confirmLabel={{this.confirmLabel}}
      @busy={{this.busy}}
      @disabled={{this.cannotCreate}}
      @onConfirm={{this.create}}
      @onCancel={{@onClose}}
    >
      <p>
        pull request
        のブランチを、このアプリと同じ設定・環境変数で別のアプリとして動かします。push
        すると自動でデプロイし直します。不要になったら削除してください（自動では消えません）。
      </p>

      {{#if this.permissionMissing}}
        <Banner
          @tone="warning"
          @title="pull request を読めません"
          data-test-review-permission
        >
          GitHub App の Repository permissions に「Pull requests:
          Read-only」を足し、インストール先の画面で新しい権限を承認してください（docs/SETUP.md）。
        </Banner>
      {{else}}
        <AsyncView
          @data={{this.pulls}}
          @errorTitle="pull request の一覧を取得できませんでした"
        >
          <:default as |pulls|>
            {{#if pulls.length}}
              <fieldset
                class="option-group review-pulls"
                data-test-review-pulls
              >
                <legend class="visually-hidden">pull request</legend>
                {{#each pulls key="number" as |pr|}}
                  <label class="option" data-test-review-pull={{pr.number}}>
                    <input
                      type="radio"
                      name="review-pull"
                      value={{pr.number}}
                      disabled={{notSelectable pr}}
                      checked={{eqNumber this.chosen pr.number}}
                      {{on "change" (fn this.choose pr.number)}}
                    />
                    <span class="option-body">
                      <span class="option-label">
                        <span class="review-pull-number">#{{pr.number}}</span>
                        {{pr.title}}
                      </span>
                      <span class="option-hint">
                        <code>{{pr.head_branch}}</code>
                        ·
                        {{pr.author}}
                        ·
                        <RelativeTime @time={{pr.updated_at}} />
                        {{#if pr.from_fork}}
                          <Badge data-test-review-fork>fork からは作れません</Badge>
                        {{else if pr.review_app}}
                          <Badge @tone="info" data-test-review-exists>作成済み:
                            {{pr.review_app}}</Badge>
                        {{/if}}
                      </span>
                    </span>
                  </label>
                {{/each}}
              </fieldset>
            {{else}}
              <p class="review-empty" data-test-review-no-pulls>
                開いている pull request はありません。
              </p>
            {{/if}}
          </:default>
        </AsyncView>
      {{/if}}

      <fieldset class="option-group dialog-options">
        <legend class="visually-hidden">データ</legend>
        <label class="option">
          <input
            type="checkbox"
            checked={{this.copyData}}
            disabled={{this.copyDisabled}}
            data-test-review-copy
            {{on "change" this.toggleCopy}}
          />
          <span class="option-body">
            <span class="option-label">親アプリのデータをコピーする</span>
            <span class="option-hint" data-test-review-copy-hint>
              {{#if this.copyProblem}}
                {{this.copyProblem}}
                データはコピーせず、空の DB で作ります。
              {{else}}
                最初のデプロイが成功したあと、{{@parent.slug}}
                の DB を流し込み、コピー後のコマンドを実行します。
              {{/if}}
            </span>
          </span>
        </label>
      </fieldset>

      <ErrorMessage @error={{this.error}} />
    </ConfirmDialog>
  </template>
}

function eqNumber(a: number | null, b: number): boolean {
  return a === b;
}
