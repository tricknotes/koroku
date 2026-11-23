import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import AsyncView from './async-view';
import Badge from './badge';
import Banner from './banner';
import Button, { buttonClass } from './button';
import Icon from './icon';
import ReviewAppDialog from './review-app-dialog';
import RowList from './row-list';
import StatusIndicator from './status-indicator';
import TabSection from './tab-section';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import { hostOf } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import {
  copyState,
  PR_STATE_LABELS,
  PR_STATE_TONES,
} from 'koroku-console/utils/review';
import { can } from 'koroku-console/utils/roles';
import {
  APP_STATE_KINDS,
  APP_STATE_LABELS,
  deriveAppState,
} from 'koroku-console/utils/status';
import type {
  App,
  CreatedReviewApp,
  PullRequestState,
  ReviewInfo,
} from 'koroku-console/utils/types';

/** Review apps deploy and change state while the parent's page is open. */
const INTERVAL = 10000;

function stateKind(app: App) {
  return APP_STATE_KINDS[deriveAppState(app)];
}

function stateLabel(app: App): string {
  return APP_STATE_LABELS[deriveAppState(app)];
}

function prLabel(state: PullRequestState): string {
  return PR_STATE_LABELS[state];
}

function prTone(state: PullRequestState) {
  return PR_STATE_TONES[state];
}

function copyKind(review: ReviewInfo) {
  return copyState(review).kind;
}

function copyLabel(review: ReviewInfo): string {
  return copyState(review).label;
}

function copyHint(review: ReviewInfo): string {
  return copyState(review).hint;
}

interface Signature {
  Element: HTMLElement;
  Args: {
    parent: App;
    /** Called after a review app was created, to re-read the parent. */
    onCreated?: () => unknown;
  };
}

/**
 * The review apps of a staging app and the way to create one (ARCHITECTURE
 * 9.9), in the review apps section of the parent's deploy tab.
 */
export default class ReviewAppsPanel extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  reviewApps = new Loadable<App[]>(() =>
    this.api.listReviewApps(this.args.parent.slug),
  );

  @tracked creating = false;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.reviewApps.load();
    this.poller.every(this, INTERVAL, () =>
      this.reviewApps.load({ quiet: true }),
    );
  }

  /** Creating one copies the parent's variables and data: manage. */
  get canCreate(): boolean {
    return can(this.args.parent, 'manage');
  }

  /** Only where the owner allowed them; the ones there stay either way. */
  get allowed(): boolean {
    return this.args.parent.review_apps_allowed === true;
  }

  get notAllowed(): boolean {
    return !this.allowed;
  }

  open = () => {
    this.creating = true;
  };

  close = () => {
    this.creating = false;
  };

  /** The review app that was just created, with what to tell about it. */
  @tracked created: CreatedReviewApp | null = null;

  onCreated = async (app: CreatedReviewApp) => {
    this.creating = false;
    this.created = app;
    await Promise.all([
      this.reviewApps.load({ quiet: true }),
      this.args.onCreated?.(),
    ]);
  };

  dismiss = () => {
    this.created = null;
  };

  <template>
    <TabSection @title="レビューアプリ" data-test-review-apps ...attributes>
      <:actions>
        {{#if this.canCreate}}
          <Button
            @size="small"
            disabled={{this.notAllowed}}
            title={{if this.notAllowed "オーナーが許可すると作れます"}}
            data-test-review-create
            {{on "click" this.open}}
          >レビューアプリを作成</Button>
        {{/if}}
      </:actions>
      <:default>
        <p class="pane-description">
          pull request
          ごとに、このアプリの設定・環境変数・データを写したアプリを作ります。
        </p>
        {{#if this.created}}
          <Banner
            @tone={{if this.created.deploy_error "warning" "info"}}
            @title="{{this.created.slug}} を作成しました"
            class="review-created"
            data-test-review-created
          >
            <:default>
              {{#if this.created.deploy_error}}
                <p data-test-review-deploy-error>
                  最初のデプロイを始められませんでした:
                  {{this.created.deploy_error}}
                  デプロイタブから始め直してください。
                </p>
              {{else}}
                <p>pull request の head をデプロイしています。</p>
              {{/if}}
              {{#if this.created.dns_warning}}
                <p data-test-review-dns-warning>
                  <code>{{hostOf this.created.url}}</code>
                  がこのホストに解決されません。DNS
                  にワイルドカードのレコードがなければ、このホスト名を登録してください。
                </p>
              {{/if}}
            </:default>
            <:actions>
              <LinkTo
                @route="app"
                @model={{this.created.slug}}
                class={{buttonClass "default" "small"}}
              >開く</LinkTo>
              <Button
                @variant="ghost"
                @size="small"
                {{on "click" this.dismiss}}
              >閉じる</Button>
            </:actions>
          </Banner>
        {{/if}}
        <AsyncView
          @data={{this.reviewApps}}
          @errorTitle="レビューアプリの一覧を取得できませんでした"
        >
          <:default as |apps|>
            {{#if apps.length}}
              <RowList class="review-rows" @label="レビューアプリ">
                <:header>
                  <span>pull request</span>
                  <span>状態</span>
                  <span>データ</span>
                  <span></span>
                </:header>
                <:rows as |Row|>
                  {{#each apps key="slug" as |app|}}
                    <Row data-test-review-app={{app.slug}}>
                      <div class="review-cell-pr">
                        <LinkTo
                          @route="app"
                          @model={{app.slug}}
                          class="row-link review-title"
                        >
                          <span
                            class="review-pull-number"
                          >#{{app.review.pr_number}}</span>
                          {{app.review.pr_title}}
                        </LinkTo>
                        <span class="review-meta">
                          {{#if app.review}}
                            <Badge
                              @tone={{prTone app.review.pr_state}}
                              data-test-review-pr-state
                            >{{prLabel app.review.pr_state}}</Badge>
                          {{/if}}
                          <span class="review-host">{{hostOf app.url}}</span>
                        </span>
                      </div>
                      <div class="review-cell-state">
                        <span class="cell-label">状態</span>
                        <StatusIndicator
                          @kind={{stateKind app}}
                          @label={{stateLabel app}}
                          data-test-review-state
                        />
                      </div>
                      <div class="review-cell-copy">
                        <span class="cell-label">データ</span>
                        {{#if app.review}}
                          <StatusIndicator
                            @kind={{copyKind app.review}}
                            @label={{copyLabel app.review}}
                            title={{copyHint app.review}}
                            data-test-review-row-copy
                          />
                        {{/if}}
                      </div>
                      <div class="review-cell-open">
                        <a
                          class="{{buttonClass 'ghost' 'small'}} row-action"
                          href={{app.url}}
                          target="_blank"
                          rel="noopener noreferrer"
                          aria-label="{{app.slug}} を新しいタブで開く"
                        >開く<Icon @name="external" /></a>
                      </div>
                    </Row>
                  {{/each}}
                </:rows>
              </RowList>
            {{else}}
              <p
                class="box overview-panel overview-quiet"
                data-test-review-empty
              >
                {{#if this.allowed}}
                  レビューアプリはまだありません。
                {{else}}
                  レビューアプリはありません。
                {{/if}}
              </p>
            {{/if}}
          </:default>
        </AsyncView>
      </:default>
    </TabSection>

    {{#if this.creating}}
      <ReviewAppDialog
        @parent={{@parent}}
        @onClose={{this.close}}
        @onCreated={{this.onCreated}}
      />
    {{/if}}
  </template>
}
