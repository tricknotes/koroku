import Component from '@glimmer/component';
import { service } from '@ember/service';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import Badge from './badge';
import Icon from './icon';
import StatusIndicator from './status-indicator';
import TabSection from './tab-section';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import Loadable from 'koroku-console/utils/loadable';
import {
  copyState,
  type CopyState,
  PR_STATE_LABELS,
  PR_STATE_TONES,
  prFinished,
} from 'koroku-console/utils/review';
import { can } from 'koroku-console/utils/roles';
import type { App, DbCopy, ReviewInfo } from 'koroku-console/utils/types';

const INTERVAL = 10000;

interface Signature {
  Element: HTMLElement;
  Args: { app: App; review: ReviewInfo };
}

/**
 * What a review app is: its parent, its pull request and the copy of the
 * parent's data (ARCHITECTURE 9.9). Shown on the review app's overview.
 */
export default class ReviewAppSummary extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  copies = new Loadable<DbCopy[]>(() =>
    this.api.listDbCopies(this.args.app.slug),
  );

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.copies.load();
    this.poller.every(this, INTERVAL, () => this.copies.load({ quiet: true }));
  }

  get prLabel(): string {
    return PR_STATE_LABELS[this.args.review.pr_state];
  }

  get prTone() {
    return PR_STATE_TONES[this.args.review.pr_state];
  }

  get finished(): boolean {
    return prFinished(this.args.review);
  }

  get canManage(): boolean {
    return can(this.args.app, 'manage');
  }

  /** The newest copy from the parent, which the state follows once started. */
  get latestCopy(): DbCopy | null {
    const parent = this.args.review.parent;
    return (
      (this.copies.value ?? []).find((c) => c.source_app === parent) ?? null
    );
  }

  get copyState(): CopyState {
    const latest = this.latestCopy;
    return copyState(
      this.args.review,
      latest ? { status: latest.status, error: latest.error } : undefined,
    );
  }

  <template>
    <TabSection @title="レビューアプリ" data-test-review-summary ...attributes>
      <div class="box overview-panel">
        <dl class="facts">
          <div class="fact">
            <dt>pull request</dt>
            <dd class="review-summary-pr">
              <a
                href={{@review.pr_url}}
                target="_blank"
                rel="noopener noreferrer"
                data-test-review-pr-link
              >#{{@review.pr_number}}
                {{@review.pr_title}}<Icon @name="external" /></a>
              <Badge
                @tone={{this.prTone}}
                data-test-review-pr-state
              >{{this.prLabel}}</Badge>
            </dd>
          </div>
          <div class="fact">
            <dt>ブランチ</dt>
            <dd><code>{{@review.head_branch}}</code></dd>
          </div>
          <div class="fact">
            <dt>親アプリ</dt>
            <dd>
              <LinkTo
                @route="app"
                @model={{@review.parent}}
                data-test-review-parent
              >{{@review.parent}}</LinkTo>
            </dd>
          </div>
          <div class="fact" data-test-review-copy-state>
            <dt>データ</dt>
            <dd>
              <StatusIndicator
                @kind={{this.copyState.kind}}
                @label={{this.copyState.label}}
              />
            </dd>
          </div>
        </dl>
        <p class="review-summary-hint">{{this.copyState.hint}}</p>
        {{#if this.canManage}}
          <p class="review-summary-hint">
            <LinkTo
              @route="app.backups"
              @model={{@app.slug}}
              data-test-review-recopy
            >バックアップタブ</LinkTo>の「DB のコピー」で、コピー元に
            {{@review.parent}}
            を選ぶと、データをコピーし直せます。
          </p>
        {{/if}}
        {{#if this.finished}}
          <p class="overview-warning" data-test-review-finished>
            <Icon @name="info" />
            <span>pull request は{{this.prLabel}}です。不要なら設定タブからこのアプリを削除してください（自動では消えません）。</span>
          </p>
        {{/if}}
      </div>
    </TabSection>
  </template>
}
