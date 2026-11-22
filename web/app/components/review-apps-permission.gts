import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import ConfirmDialog from './confirm-dialog';
import ErrorMessage from './error-message';
import StatusIndicator from './status-indicator';
import TabSection from './tab-section';
import ToggleSwitch from './toggle-switch';
import type ApiService from 'koroku-console/services/api';
import type SessionService from 'koroku-console/services/session';
import type AppContext from 'koroku-console/utils/app-context';
import { ApiError } from 'koroku-console/utils/errors';
import { parentProblem } from 'koroku-console/utils/review';

interface Signature {
  Element: HTMLElement;
  Args: { context: AppContext };
}

/**
 * Whether review apps may be created from the app (ARCHITECTURE 9.9). Only
 * the owner switches it; turning it on asks first, since pull request code
 * then receives the app's variables and data.
 */
export default class ReviewAppsPermission extends Component<Signature> {
  @service declare api: ApiService;
  @service declare session: SessionService;

  @tracked confirming = false;
  @tracked busy = false;
  @tracked error: unknown = null;

  get allowed(): boolean {
    return this.args.context.app.review_apps_allowed === true;
  }

  get isOwner(): boolean {
    return this.session.isOwner;
  }

  /** An app that stopped being a possible parent cannot be allowed again. */
  get problem(): string | null {
    return parentProblem(this.args.context.app);
  }

  get cannotSwitch(): boolean {
    return this.busy || (!this.allowed && this.problem !== null);
  }

  onChange = (checked: boolean) => {
    if (this.cannotSwitch) return;
    this.error = null;
    if (checked) {
      this.confirming = true;
    } else {
      void this.save(false);
    }
  };

  cancel = () => {
    this.confirming = false;
  };

  confirm = () => this.save(true);

  private async save(allowed: boolean): Promise<void> {
    this.busy = true;
    try {
      const app = await this.api.updateApp(this.args.context.slug, {
        review_apps_allowed: allowed,
      });
      this.args.context.set(app);
      this.confirming = false;
    } catch (error) {
      // The re-login dialog takes a stale login over.
      if (!(error instanceof ApiError && error.code === 'reauth_required')) {
        this.error = error;
      }
      this.confirming = false;
    } finally {
      this.busy = false;
    }
  }

  <template>
    <TabSection @title="許可" data-test-review-permission-section ...attributes>
      <div class="box review-permission">
        {{#if this.isOwner}}
          <ToggleSwitch
            @checked={{this.allowed}}
            @onChange={{this.onChange}}
            disabled={{this.cannotSwitch}}
            data-test-review-allow
          >レビューアプリを許可する</ToggleSwitch>
        {{else}}
          <StatusIndicator
            @kind={{if this.allowed "running" "stopped"}}
            @label={{if this.allowed "許可されています" "許可されていません"}}
            data-test-review-allowed-state
          />
        {{/if}}
        <p class="review-permission-text">
          許可すると、pull request
          のコードがこのアプリの環境変数とデータを受け取ります。pull request
          は、このリポジトリのブランチに push できる人なら誰でも作れます。
        </p>
        {{#if this.allowed}}
          <p class="review-permission-text">
            許可を外しても、作ってあるレビューアプリはそのまま残り、デプロイも削除もできます。新しく作れなくなるだけです。
          </p>
        {{else if this.isOwner}}
          {{#if this.problem}}
            <p class="review-permission-text" data-test-review-allow-problem>
              {{this.problem}}
            </p>
          {{/if}}
        {{else}}
          <p class="review-permission-text" data-test-review-owner-needed>
            レビューアプリを作るには、オーナーがここで許可する必要があります。
          </p>
        {{/if}}
        <ErrorMessage @error={{this.error}} />
      </div>
    </TabSection>

    {{#if this.confirming}}
      <ConfirmDialog
        @title="レビューアプリを許可する"
        @confirmLabel="許可する"
        @busy={{this.busy}}
        @onConfirm={{this.confirm}}
        @onCancel={{this.cancel}}
      >
        <p data-test-review-allow-confirm>
          {{@context.slug}}
          の pull request
          ごとに、レビューアプリを作れるようになります。レビューアプリには、このアプリの環境変数（値を含む）とデータが写されます。
        </p>
        <p>
          マージ前の pull request
          のコードが、これらを読めるようになります。このリポジトリのブランチに
          push
          できる人を信頼できるときだけ許可してください。管理のロールを持つメンバーも作れるようになります。
        </p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
