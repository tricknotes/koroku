import Component from '@glimmer/component';
import { service } from '@ember/service';
import { hash } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import AsyncView from 'koroku-console/components/async-view';
import AttentionBanner from 'koroku-console/components/attention-banner';
import Banner from 'koroku-console/components/banner';
import { buttonClass } from 'koroku-console/components/button';
import EmptyState from 'koroku-console/components/empty-state';
import EnvironmentBadge from 'koroku-console/components/environment-badge';
import ProtectionBadge from 'koroku-console/components/protection-badge';
import RoleBadge from 'koroku-console/components/role-badge';
import HostStatus from 'koroku-console/components/host-status';
import Icon from 'koroku-console/components/icon';
import RelativeTime from 'koroku-console/components/relative-time';
import RowList from 'koroku-console/components/row-list';
import StatusIndicator from 'koroku-console/components/status-indicator';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type SessionService from 'koroku-console/services/session';
import {
  attentionItems,
  type AttentionItem,
} from 'koroku-console/utils/attention';
import { firstLine, hostOf, shortSha } from 'koroku-console/utils/format';
import {
  reviewCount,
  reviewDeploying,
  reviewFailed,
} from 'koroku-console/utils/review';
import Loadable from 'koroku-console/utils/loadable';
import {
  APP_STATE_HINTS,
  APP_STATE_KINDS,
  APP_STATE_LABELS,
  DEPLOYMENT_STATUS_KINDS,
  DEPLOYMENT_STATUS_LABELS,
  deriveAppState,
} from 'koroku-console/utils/status';
import type {
  App,
  BackupOverview,
  Deployment,
  SystemInfo,
} from 'koroku-console/utils/types';

/** How often the apps are re-read; a deployment changes them within seconds. */
const APPS_INTERVAL = 5000;
/** The host figures move slowly. */
const HOST_INTERVAL = 30000;

const SKELETON_ROWS = [1, 2, 3];

function stateKind(app: App) {
  return APP_STATE_KINDS[deriveAppState(app)];
}

function stateLabel(app: App): string {
  return APP_STATE_LABELS[deriveAppState(app)];
}

function stateHint(app: App): string {
  return APP_STATE_HINTS[deriveAppState(app)];
}

/** Where "open" leads: the primary domain, or the assigned one. */
function openUrl(app: App): string {
  return app.primary_url || app.url;
}

function deploymentKind(deployment: Deployment) {
  return DEPLOYMENT_STATUS_KINDS[deployment.status] ?? 'stopped';
}

function deploymentLabel(deployment: Deployment): string {
  return DEPLOYMENT_STATUS_LABELS[deployment.status] ?? deployment.status;
}

/** A successful deployment is the normal case and stays in the background. */
function isUneventful(deployment: Deployment): boolean {
  return deployment.status === 'succeeded';
}

/**
 * The latest deployment when it is not the one that is serving: one that
 * failed, was canceled or is still in progress. Its commit is then shown
 * next to the result, since it differs from the running one.
 */
function pendingOrFailed(app: App): Deployment | null {
  const latest = app.latest_deployment;
  if (!latest || latest.id === app.current_deployment?.id) return null;
  return latest;
}

/**
 * Review apps are not rows of their own: their parent's row counts them
 * (ARCHITECTURE 9.9).
 */
function withoutReviewApps(apps: App[]): App[] {
  return apps.filter((app) => !app.review);
}

/** The count has notes when some review apps failed or are deploying. */
function hasReviewNotes(app: App): boolean {
  return !!(reviewFailed(app.review_apps) || reviewDeploying(app.review_apps));
}

function when(deployment: Deployment): string | null {
  return (
    deployment.finished_at ?? deployment.started_at ?? deployment.queued_at
  );
}

export default class IndexTemplate extends Component {
  @service declare api: ApiService;
  @service declare poller: PollerService;
  @service declare session: SessionService;

  apps = new Loadable<App[]>(() => this.api.listApps());
  system = new Loadable<SystemInfo>(() => this.api.getSystem());
  backups = new Loadable<BackupOverview>(() => this.api.getBackups());

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.apps.load();
    void this.system.load();
    void this.backups.load();
    // States change without anyone touching this page (a deployment
    // finishing, a domain getting verified), so they are polled for as long
    // as the list is on screen.
    this.poller.every(this, APPS_INTERVAL, () =>
      this.apps.load({ quiet: true }),
    );
    this.poller.every(this, HOST_INTERVAL, () =>
      Promise.all([
        this.system.load({ quiet: true }),
        this.backups.load({ quiet: true }),
      ]),
    );
  }

  get attention(): AttentionItem[] {
    return attentionItems(
      this.apps.value,
      this.system.value,
      this.backups.value,
    );
  }

  /** Only the owner creates apps (ARCHITECTURE 10.7). */
  get isOwner(): boolean {
    return this.session.isOwner;
  }

  /** Loaded and nothing there: the empty state then holds the one action. */
  get isEmpty(): boolean {
    return this.apps.loaded && this.apps.value?.length === 0;
  }

  <template>
    {{pageTitle "アプリ"}}

    <div class="app-list-top">
      <HostStatus @system={{this.system}} @backups={{this.backups}} />

      {{#if this.attention.length}}
        <AttentionBanner @items={{this.attention}} @owner={{this.isOwner}} />
      {{/if}}

      {{#if this.apps.stale}}
        <Banner @tone="warning" data-test-stale>
          最新の状態を取得できていません。表示は最後に取得できた内容です。
        </Banner>
      {{/if}}
    </div>

    <div class="page-header">
      <h1>アプリ</h1>
      {{#if this.isOwner}}
        {{#unless this.isEmpty}}
          <LinkTo
            @route="new-app"
            class={{buttonClass "primary"}}
            data-test-new-app
          >アプリを作成</LinkTo>
        {{/unless}}
      {{/if}}
    </div>

    <AsyncView
      @data={{this.apps}}
      @errorTitle="アプリの一覧を取得できませんでした"
    >
      <:loading>
        <div
          class="row-list app-rows"
          role="status"
          aria-label="読み込み中"
          data-test-loading
        >
          {{#each SKELETON_ROWS as |n|}}
            <div class="row" aria-hidden="true" data-skeleton={{n}}>
              <div class="app-cell-state">
                <span class="skeleton skeleton-fixed"></span>
              </div>
              <div class="app-cell-name">
                <span class="skeleton skeleton-narrow"></span>
              </div>
              <div class="app-cell-commit">
                <span class="skeleton skeleton-wide"></span>
              </div>
              <div class="app-cell-deploy">
                <span class="skeleton skeleton-narrow"></span>
              </div>
            </div>
          {{/each}}
        </div>
      </:loading>

      <:default as |apps|>
        {{#if apps.length}}
          <RowList class="app-rows" @label="アプリ">
            <:header>
              <span>状態</span>
              <span>アプリ</span>
              <span>稼働中のコミット</span>
              <span>直近のデプロイ</span>
            </:header>
            <:rows as |Row|>
              {{#each (withoutReviewApps apps) key="slug" as |app|}}
                <Row data-test-app={{app.slug}}>
                  <div class="app-cell-state">
                    <StatusIndicator
                      @kind={{stateKind app}}
                      @label={{stateLabel app}}
                      title={{stateHint app}}
                      data-test-state
                    />
                  </div>

                  <div class="app-cell-name">
                    <span class="app-name-line">
                      <LinkTo
                        @route="app"
                        @model={{app.slug}}
                        class="row-link app-name"
                        data-test-app-link
                      >{{app.slug}}</LinkTo>
                      <EnvironmentBadge
                        @environment={{app.environment}}
                        data-test-environment
                      />
                      {{#if app.access_protection}}
                        <ProtectionBadge data-test-protected />
                      {{/if}}
                      <RoleBadge @role={{app.role}} />
                    </span>
                    {{#if (reviewCount app.review_apps)}}
                      <span class="app-review-count" data-test-review-count>
                        <LinkTo
                          @route="app.deploy"
                          @model={{app.slug}}
                          @query={{hash section="review-apps"}}
                          class="app-review-link"
                          data-test-review-count-link
                        >レビューアプリ
                          {{reviewCount app.review_apps}}</LinkTo>
                        {{#if (hasReviewNotes app)}}
                          <span class="app-review-notes">
                            {{#if (reviewFailed app.review_apps)}}
                              <span
                                class="app-review-failed"
                                data-test-review-count-failed
                              >{{reviewFailed app.review_apps}}</span>
                            {{/if}}
                            {{#if (reviewDeploying app.review_apps)}}
                              <span data-test-review-count-deploying>
                                {{reviewDeploying app.review_apps}}
                              </span>
                            {{/if}}
                          </span>
                        {{/if}}
                      </span>
                    {{/if}}
                    {{#if (openUrl app)}}
                      <span class="app-url" data-test-url>
                        {{hostOf (openUrl app)}}
                      </span>
                    {{/if}}
                  </div>

                  <div class="app-cell-commit" data-test-current-deployment>
                    <span class="cell-label">稼働中のコミット</span>
                    {{#if app.current_deployment}}
                      <span class="app-commit-message">
                        {{firstLine app.current_deployment.commit_message}}
                      </span>
                      <span class="app-meta">
                        <span
                          class="app-sha"
                          title={{app.current_deployment.commit_sha}}
                        >{{shortSha app.current_deployment.commit_sha}}</span>
                        <RelativeTime @time={{when app.current_deployment}} />
                      </span>
                    {{else}}
                      <span class="app-none">まだありません</span>
                    {{/if}}
                  </div>

                  <div class="app-cell-deploy" data-test-last-deployment>
                    <span class="cell-label">直近のデプロイ</span>
                    {{#if app.latest_deployment}}
                      {{#let (pendingOrFailed app) as |deployment|}}
                        <span class="app-meta">
                          <StatusIndicator
                            @kind={{deploymentKind app.latest_deployment}}
                            @label={{deploymentLabel app.latest_deployment}}
                            @quiet={{isUneventful app.latest_deployment}}
                            data-test-deployment-status
                          />
                          {{#if deployment}}
                            <RelativeTime @time={{when deployment}} />
                          {{/if}}
                        </span>
                        {{#if deployment}}
                          <span class="app-meta">
                            <span
                              class="app-sha"
                              title={{deployment.commit_sha}}
                            >{{shortSha deployment.commit_sha}}</span>
                            <span class="app-commit-message">
                              {{firstLine deployment.commit_message}}
                            </span>
                          </span>
                        {{/if}}
                      {{/let}}
                    {{else}}
                      <span class="app-none">デプロイ履歴なし</span>
                    {{/if}}
                  </div>

                  <div class="app-cell-open">
                    {{#if (openUrl app)}}
                      <a
                        class="{{buttonClass 'ghost' 'small'}} row-action"
                        href={{openUrl app}}
                        target="_blank"
                        rel="noopener noreferrer"
                        aria-label="{{app.slug}} を新しいタブで開く"
                        data-test-open
                      >開く<Icon @name="external" /></a>
                    {{/if}}
                  </div>
                </Row>
              {{/each}}
            </:rows>
          </RowList>
        {{else}}
          <div class="row-list" data-test-empty>
            {{#if this.isOwner}}
              <EmptyState
                @title="アプリはまだありません"
                @description="GitHub のリポジトリを選んで、Docker Compose のアプリをデプロイします。"
              >
                <:action>
                  <LinkTo
                    @route="new-app"
                    class={{buttonClass "primary"}}
                    data-test-new-app
                  >アプリを作成</LinkTo>
                </:action>
              </EmptyState>
            {{else}}
              <EmptyState
                @title="扱えるアプリはありません"
                @description="オーナーがアプリのロールを与えると、ここに表示されます。"
              />
            {{/if}}
          </div>
        {{/if}}
      </:default>
    </AsyncView>
  </template>
}
