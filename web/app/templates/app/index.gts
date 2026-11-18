import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import AsyncView from 'paas-console/components/async-view';
import Badge from 'paas-console/components/badge';
import Banner from 'paas-console/components/banner';
import Button, { buttonClass } from 'paas-console/components/button';
import CopyButton from 'paas-console/components/copy-button';
import EmptyState from 'paas-console/components/empty-state';
import ErrorMessage from 'paas-console/components/error-message';
import Icon from 'paas-console/components/icon';
import RelativeTime from 'paas-console/components/relative-time';
import RowList from 'paas-console/components/row-list';
import StatusIndicator from 'paas-console/components/status-indicator';
import TabSection from 'paas-console/components/tab-section';
import type ApiService from 'paas-console/services/api';
import type PollerService from 'paas-console/services/poller';
import type AppContext from 'paas-console/utils/app-context';
import {
  firstLine,
  formatDateTimeJst,
  formatDuration,
  shortSha,
} from 'paas-console/utils/format';
import Loadable from 'paas-console/utils/loadable';
import {
  healthLabel,
  memoryUsage,
  serviceState,
} from 'paas-console/utils/services';
import {
  APP_STATE_HINTS,
  DEPLOYMENT_STATUS_KINDS,
  DEPLOYMENT_STATUS_LABELS,
  deriveAppState,
  type AppState,
} from 'paas-console/utils/status';
import type {
  App,
  BackupOverview,
  Deployment,
  Domain,
  DomainList,
  ServiceStatus,
  Snapshot,
} from 'paas-console/utils/types';

/** The app itself: cheap, and it is what tells that something changed. */
const APP_INTERVAL = 5000;
/** Everything else, which also changes without the app changing. */
const DETAILS_INTERVAL = 30000;

const RECENT_DEPLOYMENTS = 5;
const SKELETON_ROWS = [1, 2, 3];

/** One hostname of the app, as listed in the side column. */
interface Hostname {
  name: string;
  automatic: boolean;
  primary: boolean;
  pending: boolean;
}

function deploymentKind(deployment: Deployment) {
  return DEPLOYMENT_STATUS_KINDS[deployment.status] ?? 'stopped';
}

function deploymentLabel(deployment: Deployment): string {
  return DEPLOYMENT_STATUS_LABELS[deployment.status] ?? deployment.status;
}

function isUneventful(deployment: Deployment): boolean {
  return deployment.status === 'succeeded';
}

function when(deployment: Deployment): string | null {
  return (
    deployment.finished_at ?? deployment.started_at ?? deployment.queued_at
  );
}

function duration(deployment: Deployment): string {
  return formatDuration(deployment.started_at, deployment.finished_at);
}

function isUnhealthy(service: ServiceStatus): boolean {
  return service.health === 'unhealthy';
}

function isQuiet(service: ServiceStatus): boolean {
  return serviceState(service).kind === 'stopped';
}

/** Everything that moves an app from one state to another. */
function stateKey(app: App): string {
  const { latest_deployment: latest, current_deployment: current } = app;
  return [latest?.id, latest?.status, current?.id, app.desired_state].join(':');
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppOverviewTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  services = new Loadable<ServiceStatus[]>(() =>
    this.api.listServices(this.slug),
  );
  deployments = new Loadable<Deployment[]>(() =>
    this.api.listDeployments(this.slug, RECENT_DEPLOYMENTS),
  );
  domains = new Loadable<DomainList>(() => this.api.listDomains(this.slug));
  backups = new Loadable<BackupOverview>(() => this.api.getBackups());
  snapshots = new Loadable<Snapshot[]>(() => this.api.listSnapshots(this.slug));

  @tracked starting = false;
  @tracked error: unknown = null;
  @tracked notice = '';

  private lastState: string;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.lastState = stateKey(this.app);
    if (this.isDeployed) {
      void this.services.load();
      void this.deployments.load();
    }
    void this.domains.load();
    void this.backups.load();
    void this.snapshots.load();
    // A deployment finishing, a container turning unhealthy, a domain
    // getting verified: none of it needs a reload of the page.
    this.poller.every(this, APP_INTERVAL, () => this.refreshApp());
    this.poller.every(this, DETAILS_INTERVAL, () => this.refreshDetails());
  }

  get slug(): string {
    return this.args.model.slug;
  }

  get app(): App {
    return this.args.model.app;
  }

  get state(): AppState {
    return deriveAppState(this.app);
  }

  /** Something is, or was, serving: there is a release to describe. */
  get isDeployed(): boolean {
    return this.app.current_deployment !== null;
  }

  get isStopped(): boolean {
    return this.app.desired_state === 'stopped';
  }

  /**
   * The latest deployment while it is not the one serving: in progress,
   * failed or canceled. Only the first two are worth a banner.
   */
  get latestIfDifferent(): Deployment | null {
    const { latest_deployment: latest, current_deployment: current } = this.app;
    return latest && latest.id !== current?.id ? latest : null;
  }

  get failedDeployment(): Deployment | null {
    return this.state === 'failed' ? this.latestIfDifferent : null;
  }

  get activeDeployment(): Deployment | null {
    return this.state === 'deploying' ? this.latestIfDifferent : null;
  }

  get stoppedHint(): string {
    return APP_STATE_HINTS.stopped;
  }

  /** Shown instead of a release while the app has never been deployed. */
  get emptyState(): { title: string; description: string; action: string } {
    const latest = this.app.latest_deployment;
    if (this.state === 'deploying') {
      return {
        title: '最初のデプロイを実行中です',
        description:
          '完了すると、ここに稼働中のリリースとサービスの状態が表示されます。',
        action: '進行状況を見る',
      };
    }
    if (this.state === 'failed') {
      return {
        title: '最初のデプロイに失敗しました',
        description:
          latest?.error ||
          '原因はデプロイのログで確認できます。稼働中のリリースはありません。',
        action: 'デプロイを確認',
      };
    }
    return {
      title: 'まだデプロイされていません',
      description:
        'ブランチかコミットを選んでデプロイすると、ここに稼働中のリリースとサービスの状態が表示されます。',
      action: 'デプロイする',
    };
  }

  get isStale(): boolean {
    return this.services.stale || this.deployments.stale;
  }

  /** Custom domains first (the primary one on top), then the assigned one. */
  get hostnames(): Hostname[] {
    const list = this.domains.value;
    if (!list) return [];
    const custom = [...(list.domains ?? [])]
      .sort((a, b) => Number(b.is_primary) - Number(a.is_primary))
      .map((domain: Domain) => ({
        name: domain.hostname,
        automatic: false,
        primary: domain.is_primary,
        pending: domain.status !== 'active',
      }));
    return [
      ...custom,
      {
        name: list.default_hostname,
        automatic: true,
        primary: false,
        pending: false,
      },
    ];
  }

  /** Backups are switched off on this host (either endpoint says so). */
  get backupsUnconfigured(): boolean {
    return (
      this.backups.value?.configured === false ||
      this.snapshots.unavailable !== null
    );
  }

  get backupsLoading(): boolean {
    const pending = (data: Loadable<unknown>) => !data.loaded && !data.error;
    return pending(this.backups) || pending(this.snapshots);
  }

  /** Time of the newest snapshot of this app. */
  get lastSnapshotAt(): string | null {
    const times = (this.snapshots.value ?? []).map((s) => s.time);
    return times.sort().at(-1) ?? null;
  }

  get backupWarning(): boolean {
    return this.backups.value?.warning === true;
  }

  get nextBackup(): string {
    return formatDateTimeJst(this.backups.value?.next_run_at);
  }

  isCurrent = (deployment: Deployment): boolean =>
    deployment.id === this.app.current_deployment?.id;

  /** Follows the app; a change of state refreshes what hangs off it. */
  async refreshApp(): Promise<void> {
    await this.args.model.reload();
    const state = stateKey(this.app);
    const changed = state !== this.lastState;
    this.lastState = state;
    if (changed || this.state === 'deploying') await this.refreshRelease();
  }

  async refreshDetails(): Promise<void> {
    await Promise.all([
      this.refreshRelease(),
      this.domains.load({ quiet: true }),
      this.backups.load({ quiet: true }),
      this.snapshots.load({ quiet: true }),
    ]);
  }

  private async refreshRelease(): Promise<void> {
    if (!this.isDeployed) return;
    await Promise.all([
      this.services.load({ quiet: this.services.loaded }),
      this.deployments.load({ quiet: this.deployments.loaded }),
    ]);
  }

  /**
   * Starts a stopped app: the one thing to do next in that state. Stopping
   * and restarting live on the settings tab.
   */
  start = async () => {
    this.starting = true;
    this.error = null;
    this.notice = '';
    try {
      const app = await this.api.controlApp(this.slug, 'start');
      this.args.model.set(app);
      this.lastState = stateKey(app);
      this.notice = '起動しました。';
      await this.services.load({ quiet: this.services.loaded });
    } catch (error) {
      this.error = error;
    } finally {
      this.starting = false;
    }
  };

  <template>
    <div class="overview" data-test-overview>
      <div class="overview-top">
        {{#if this.isDeployed}}
          {{#if this.failedDeployment}}
            <Banner
              @tone="error"
              @title="直近のデプロイに失敗しました"
              data-test-latest-deployment
            >
              <:default>
                <p class="overview-banner-commit">
                  <span
                    class="overview-sha"
                    title={{this.failedDeployment.commit_sha}}
                  >{{shortSha this.failedDeployment.commit_sha}}</span>
                  <span>{{firstLine
                      this.failedDeployment.commit_message
                    }}</span>
                  <RelativeTime @time={{when this.failedDeployment}} />
                </p>
                {{#if this.failedDeployment.error}}
                  <p
                    class="overview-banner-error"
                    data-test-latest-deployment-error
                  >{{this.failedDeployment.error}}</p>
                {{/if}}
                <p>以前のリリースが応答し続けています。</p>
              </:default>
              <:actions>
                <LinkTo
                  @route="app.deploy"
                  @model={{this.slug}}
                  class={{buttonClass "default" "small"}}
                >デプロイを確認</LinkTo>
              </:actions>
            </Banner>
          {{else if this.activeDeployment}}
            <Banner
              @tone="info"
              @title="デプロイを実行中です"
              data-test-latest-deployment
            >
              <:default>
                <p class="overview-banner-commit">
                  <span
                    class="overview-sha"
                    title={{this.activeDeployment.commit_sha}}
                  >{{shortSha this.activeDeployment.commit_sha}}</span>
                  <span>{{firstLine
                      this.activeDeployment.commit_message
                    }}</span>
                </p>
                <p>完了するまで、下のリリースが応答し続けます。</p>
              </:default>
              <:actions>
                <LinkTo
                  @route="app.deploy"
                  @model={{this.slug}}
                  class={{buttonClass "default" "small"}}
                >進行状況を見る</LinkTo>
              </:actions>
            </Banner>
          {{/if}}
        {{/if}}

        {{#if this.isStale}}
          <Banner @tone="warning" data-test-stale>
            最新の状態を取得できていません。表示は最後に取得できた内容です。
          </Banner>
        {{/if}}
      </div>

      <div class="overview-main">
        {{#if this.app.current_deployment}}
          <TabSection
            @title={{if this.isStopped "停止中のリリース" "稼働中のリリース"}}
            data-test-release
          >
            <:default>
              <div class="overview-panel" data-test-current-deployment>
                <div class="overview-release-head">
                  <p class="overview-release-message">
                    {{firstLine this.app.current_deployment.commit_message}}
                  </p>
                  <StatusIndicator
                    @kind={{if this.isStopped "stopped" "running"}}
                    data-test-overview-state
                  />
                </div>
                <dl class="overview-facts">
                  <div>
                    <dt>コミット</dt>
                    <dd>
                      <span
                        class="overview-sha"
                        title={{this.app.current_deployment.commit_sha}}
                      >{{shortSha
                          this.app.current_deployment.commit_sha
                        }}</span>
                      <CopyButton
                        @text={{this.app.current_deployment.commit_sha}}
                        @label="コミット SHA"
                      />
                    </dd>
                  </div>
                  {{#if this.app.current_deployment.ref}}
                    <div>
                      <dt>ブランチ</dt>
                      <dd data-test-release-ref>
                        {{this.app.current_deployment.ref}}
                      </dd>
                    </div>
                  {{/if}}
                  <div>
                    <dt>デプロイ</dt>
                    <dd>
                      <RelativeTime
                        @time={{when this.app.current_deployment}}
                      />
                    </dd>
                  </div>
                  {{#if (duration this.app.current_deployment)}}
                    <div>
                      <dt>所要時間</dt>
                      <dd>{{duration this.app.current_deployment}}</dd>
                    </div>
                  {{/if}}
                </dl>
                {{#if this.isStopped}}
                  <div class="overview-stopped">
                    <p class="overview-stopped-hint" data-test-stopped-hint>
                      {{this.stoppedHint}}
                    </p>
                    <Button
                      @variant="primary"
                      @busy={{this.starting}}
                      @busyLabel="起動中…"
                      data-test-control="start"
                      {{on "click" this.start}}
                    >起動</Button>
                  </div>
                {{/if}}
              </div>

              <div class="overview-feedback" aria-live="polite">
                {{#if this.starting}}
                  <p class="overview-note">起動しています。完了まで待ちます…</p>
                {{else if this.notice}}
                  <p class="overview-note" data-test-notice>
                    <Icon @name="check" />{{this.notice}}
                  </p>
                {{/if}}
                <ErrorMessage @error={{this.error}} />
              </div>
            </:default>
          </TabSection>

          <TabSection @title="サービス">
            <AsyncView
              @data={{this.services}}
              @errorTitle="サービスの状態を取得できませんでした"
            >
              <:loading>
                <div
                  class="row-list overview-rows"
                  role="status"
                  aria-label="読み込み中"
                  data-test-loading
                >
                  {{#each SKELETON_ROWS as |n|}}
                    <div class="row" aria-hidden="true" data-skeleton={{n}}>
                      <span class="skeleton overview-skeleton"></span>
                    </div>
                  {{/each}}
                </div>
              </:loading>
              <:default as |services|>
                {{#if services.length}}
                  <RowList
                    class="overview-rows overview-services"
                    @label="サービス"
                    data-test-services
                  >
                    <:header>
                      <span>サービス</span>
                      <span>状態</span>
                      <span>ヘルスチェック</span>
                      <span>メモリ</span>
                    </:header>
                    <:rows as |Row|>
                      {{#each services key="name" as |svc|}}
                        {{#let
                          (serviceState svc) (memoryUsage svc)
                          as |state memory|
                        }}
                          <Row
                            data-test-service={{svc.name}}
                            data-problem={{if state.problem "true"}}
                          >
                            <div
                              class="overview-service-name"
                            >{{svc.name}}</div>
                            <div class="overview-service-state">
                              <StatusIndicator
                                @kind={{state.kind}}
                                @label={{state.label}}
                                @quiet={{isQuiet svc}}
                                data-test-service-state
                              />
                              {{#if svc.status}}
                                <span class="overview-secondary">
                                  {{svc.status}}
                                </span>
                              {{/if}}
                            </div>
                            <div
                              class="overview-service-health
                                {{if (isUnhealthy svc) 'is-bad'}}"
                              data-test-service-health
                            >
                              <span class="cell-label">ヘルスチェック</span>
                              {{#if svc.health}}
                                {{healthLabel svc.health}}
                              {{else}}
                                <span class="overview-secondary">なし</span>
                              {{/if}}
                            </div>
                            <div
                              class="overview-service-memory
                                {{if memory.high 'is-high'}}"
                              data-test-service-memory
                            >
                              <span class="cell-label">メモリ</span>
                              {{#if memory}}
                                <span class="overview-memory-value">
                                  {{#if memory.high}}
                                    <Icon @name="warning" />
                                  {{/if}}
                                  {{memory.text}}
                                </span>
                                {{#if memory.high}}
                                  <span class="overview-memory-note">
                                    上限の
                                    {{memory.percent}}%
                                  </span>
                                {{/if}}
                              {{else}}
                                <span
                                  class="overview-secondary"
                                  title="取得できませんでした。"
                                >—</span>
                              {{/if}}
                            </div>
                          </Row>
                        {{/let}}
                      {{/each}}
                    </:rows>
                  </RowList>
                {{else}}
                  <p
                    class="overview-panel overview-quiet"
                    data-test-no-services
                  >
                    動いているサービスはありません。
                  </p>
                {{/if}}
              </:default>
            </AsyncView>
          </TabSection>

          <TabSection @title="直近のデプロイ">
            <:actions>
              <LinkTo
                @route="app.deploy"
                @model={{this.slug}}
                class="overview-more"
                data-test-all-deployments
              >すべて見る</LinkTo>
            </:actions>
            <:default>
              <AsyncView
                @data={{this.deployments}}
                @errorTitle="デプロイの履歴を取得できませんでした"
              >
                <:loading>
                  <div
                    class="row-list overview-rows"
                    role="status"
                    aria-label="読み込み中"
                  >
                    {{#each SKELETON_ROWS as |n|}}
                      <div class="row" aria-hidden="true" data-skeleton={{n}}>
                        <span class="skeleton overview-skeleton"></span>
                      </div>
                    {{/each}}
                  </div>
                </:loading>
                <:default as |deployments|>
                  <RowList
                    class="overview-rows overview-deployments"
                    @label="直近のデプロイ"
                    data-test-recent-deployments
                  >
                    <:rows as |Row|>
                      {{#each deployments key="id" as |deployment|}}
                        <Row data-test-recent-deployment={{deployment.id}}>
                          <div class="overview-deployment-state">
                            <StatusIndicator
                              @kind={{deploymentKind deployment}}
                              @label={{deploymentLabel deployment}}
                              @quiet={{isUneventful deployment}}
                            />
                          </div>
                          <div class="overview-deployment-commit">
                            <span class="overview-deployment-line">
                              <LinkTo
                                @route="app.deploy"
                                @model={{this.slug}}
                                class="row-link overview-deployment-message"
                              >{{firstLine deployment.commit_message}}</LinkTo>
                              {{#if (this.isCurrent deployment)}}
                                <Badge @tone="ok" data-test-live>Live</Badge>
                              {{/if}}
                            </span>
                            <span class="overview-meta">
                              <span
                                class="overview-sha"
                                title={{deployment.commit_sha}}
                              >{{shortSha deployment.commit_sha}}</span>
                              {{#if deployment.ref}}
                                <span>{{deployment.ref}}</span>
                              {{/if}}
                            </span>
                          </div>
                          <div class="overview-deployment-time">
                            <RelativeTime @time={{when deployment}} />
                          </div>
                        </Row>
                      {{/each}}
                    </:rows>
                  </RowList>
                </:default>
              </AsyncView>
            </:default>
          </TabSection>
        {{else}}
          <div class="row-list" data-test-overview-empty>
            <EmptyState
              @title={{this.emptyState.title}}
              @description={{this.emptyState.description}}
            >
              <:action>
                <LinkTo
                  @route="app.deploy"
                  @model={{this.slug}}
                  class={{buttonClass "primary"}}
                  data-test-overview-deploy
                >{{this.emptyState.action}}</LinkTo>
              </:action>
            </EmptyState>
          </div>
        {{/if}}
      </div>

      <div class="overview-side">
        <TabSection @title="ドメイン" data-test-overview-domains>
          <:actions>
            <LinkTo
              @route="app.domains"
              @model={{this.slug}}
              class="overview-more"
            >管理</LinkTo>
          </:actions>
          <:default>
            {{#if this.domains.loaded}}
              <ul class="overview-panel overview-hostnames">
                {{#each this.hostnames key="name" as |hostname|}}
                  <li data-test-overview-domain={{hostname.name}}>
                    <span class="overview-hostname">{{hostname.name}}</span>
                    {{#if hostname.primary}}
                      <Badge @tone="info" data-test-primary>プライマリ</Badge>
                    {{/if}}
                    {{#if hostname.pending}}
                      <StatusIndicator @kind="pending" data-test-pending />
                    {{/if}}
                    {{#if hostname.automatic}}
                      <span class="overview-secondary">自動割当</span>
                    {{/if}}
                  </li>
                {{/each}}
              </ul>
            {{else if this.domains.error}}
              <p class="overview-panel overview-quiet" data-test-side-error>
                取得できませんでした。
                <Button
                  @variant="ghost"
                  @size="small"
                  {{on "click" this.domains.reload}}
                >再読み込み</Button>
              </p>
            {{else}}
              <div class="overview-panel" role="status" aria-label="読み込み中">
                <span class="skeleton overview-skeleton"></span>
              </div>
            {{/if}}
          </:default>
        </TabSection>

        <TabSection @title="バックアップ" data-test-overview-backup>
          <:actions>
            <LinkTo
              @route="app.backups"
              @model={{this.slug}}
              class="overview-more"
            >開く</LinkTo>
          </:actions>
          <:default>
            {{#if this.backupsLoading}}
              <div class="overview-panel" role="status" aria-label="読み込み中">
                <span class="skeleton overview-skeleton"></span>
              </div>
            {{else if this.backupsUnconfigured}}
              <p class="overview-panel overview-quiet" data-test-backup-state>
                バックアップは設定されていません。
              </p>
            {{else if this.snapshots.error}}
              <p class="overview-panel overview-quiet" data-test-side-error>
                取得できませんでした。
                <Button
                  @variant="ghost"
                  @size="small"
                  {{on "click" this.snapshots.reload}}
                >再読み込み</Button>
              </p>
            {{else}}
              <div class="overview-panel">
                <dl class="overview-facts overview-facts-stacked">
                  <div data-test-backup-state>
                    <dt>最終バックアップ</dt>
                    <dd>
                      {{#if this.lastSnapshotAt}}
                        <RelativeTime @time={{this.lastSnapshotAt}} />
                      {{else}}
                        まだありません
                      {{/if}}
                    </dd>
                  </div>
                  {{#if this.nextBackup}}
                    <div>
                      <dt>次回の予定</dt>
                      <dd>{{this.nextBackup}}</dd>
                    </div>
                  {{/if}}
                </dl>
                {{#if this.backupWarning}}
                  <p class="overview-warning" data-test-backup-warning>
                    <Icon @name="warning" />
                    <span>このホストのバックアップが 24
                      時間以上成功していません。</span>
                  </p>
                {{/if}}
              </div>
            {{/if}}
          </:default>
        </TabSection>
      </div>
    </div>
  </template>
}
