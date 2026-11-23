import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import AsyncView from 'koroku-console/components/async-view';
import Banner from 'koroku-console/components/banner';
import Button, { buttonClass } from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import CopyButton from 'koroku-console/components/copy-button';
import ErrorMessage from 'koroku-console/components/error-message';
import Icon from 'koroku-console/components/icon';
import Meter, { type MeterTone } from 'koroku-console/components/meter';
import RelativeTime from 'koroku-console/components/relative-time';
import RowList from 'koroku-console/components/row-list';
import StatusIndicator from 'koroku-console/components/status-indicator';
import TimeUntil from 'koroku-console/components/time-until';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import {
  diskLimitPercent,
  diskOverLimit,
} from 'koroku-console/utils/attention';
import { backupHealth } from 'koroku-console/utils/backup-status';
import {
  containerGroupRows,
  containerTotals,
} from 'koroku-console/utils/container-resources';
import { setChecked } from 'koroku-console/utils/dom';
import { ApiError, unavailability } from 'koroku-console/utils/errors';
import {
  formatBytes,
  formatDuration,
  formatPercent,
} from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import { reservationView } from 'koroku-console/utils/memory-reservation';
import {
  BACKUP_STATUS_LABELS,
  BACKUP_TRIGGER_LABELS,
  type StatusKind,
} from 'koroku-console/utils/status';
import type {
  BackupOverview,
  BackupRun,
  ContainerResources,
  DockerUsageItem,
  GitHubAppStatus,
  SystemInfo,
} from 'koroku-console/utils/types';

const SELECTION_LABELS: Record<string, string> = {
  all: 'すべてのリポジトリ',
  selected: '選んだリポジトリ',
};

function selectionLabel(selection: string): string {
  return SELECTION_LABELS[selection] ?? selection;
}

/**
 * The installation_accounts line to put in config.toml: what is configured
 * plus the installations koroku does not use yet.
 */
function accountsLine(status: GitHubAppStatus): string {
  const accounts = [...status.installation_accounts];
  for (const inst of status.installations) {
    if (!inst.used && !accounts.includes(inst.account)) {
      accounts.push(inst.account);
    }
  }
  if (accounts.length === 0) accounts.push('my-org');
  return `installation_accounts = [${accounts.map((a) => `"${a}"`).join(', ')}]`;
}

/** A running backup is followed closely; the host figures move slowly. */
const RUNNING_INTERVAL = 5000;
const HOST_INTERVAL = 30000;

/** Memory in use above which the row is flagged. */
const MEMORY_WARN_PERCENT = 90;

/** How many of the latest backup runs are listed. */
const RUNS_SHOWN = 8;

const SKELETON_ROWS = [1, 2, 3];

/** One line of the resources block: a share of something finite. */
interface Resource {
  name: 'disk' | 'memory' | 'swap';
  label: string;
  /** `null` when the host did not report it. */
  percent: number | null;
  detail: string;
  /** Text shown instead of the figures (no swap, not readable). */
  missing?: string;
  tone: MeterTone;
  /** Threshold drawn on the bar. */
  mark?: number;
  /** What is wrong, as a state with a label; absent when all is well. */
  flag?: { kind: StatusKind; label: string };
}

function share(used: number, total: number): number {
  return total > 0 ? (used / total) * 100 : 0;
}

function resources(system: SystemInfo): Resource[] {
  const rows: Resource[] = [];

  if (system.disk) {
    const over = diskOverLimit(system);
    const limit = diskLimitPercent(system);
    rows.push({
      name: 'disk',
      label: 'ディスク',
      percent: system.disk.used_percent,
      detail: `${formatBytes(system.disk.used_bytes)} / ${formatBytes(system.disk.total_bytes)}、デプロイの上限 ${limit}%`,
      tone: over ? 'danger' : 'ok',
      mark: limit,
      ...(over ? { flag: { kind: 'failed', label: '上限超過' } } : {}),
    });
  } else {
    rows.push(missing('disk', 'ディスク'));
  }

  if (system.memory) {
    const { total_bytes: total, available_bytes: available } = system.memory;
    const used = Math.max(0, total - available);
    const percent = share(used, total);
    const tight = percent >= MEMORY_WARN_PERCENT;
    rows.push({
      name: 'memory',
      label: 'メモリ',
      percent,
      detail: `${formatBytes(used)} / ${formatBytes(total)}、空き ${formatBytes(available)}`,
      tone: tight ? 'warn' : 'ok',
      ...(tight ? { flag: { kind: 'pending', label: '残りわずか' } } : {}),
    });
  } else {
    rows.push(missing('memory', 'メモリ'));
  }

  // Absent on a backend that does not report swap: then there is no row.
  if (system.swap) {
    const { total_bytes: total, used_bytes: used } = system.swap;
    rows.push({
      name: 'swap',
      label: 'スワップ',
      percent: share(used, total),
      detail: `${formatBytes(used)} / ${formatBytes(total)}`,
      tone: 'ok',
    });
  } else if (system.swap === null) {
    rows.push({ ...missing('swap', 'スワップ'), missing: 'ありません' });
  }
  return rows;
}

function missing(name: Resource['name'], label: string): Resource {
  return {
    name,
    label,
    percent: null,
    detail: '',
    missing: '取得できません',
    tone: 'ok',
  };
}

function hasPercent(resource: Resource): resource is Resource & {
  percent: number;
} {
  return resource.percent !== null;
}

const DOCKER_LABELS: Record<DockerUsageItem['type'], string> = {
  images: 'イメージ',
  build_cache: 'ビルドキャッシュ',
  volumes: 'volume',
  containers: 'コンテナ',
};

const DOCKER_ORDER: DockerUsageItem['type'][] = [
  'images',
  'build_cache',
  'volumes',
  'containers',
];

function dockerRows(items: DockerUsageItem[]): DockerUsageItem[] {
  return [...items].sort(
    (a, b) => DOCKER_ORDER.indexOf(a.type) - DOCKER_ORDER.indexOf(b.type),
  );
}

function dockerLabel(item: DockerUsageItem): string {
  return DOCKER_LABELS[item.type] ?? item.type;
}

/** Volumes and containers are never pruned: nothing to expect there. */
function reclaimable(item: DockerUsageItem): string {
  if (item.type === 'volumes' || item.type === 'containers') return '—';
  return formatBytes(item.reclaimable_bytes);
}

function dockerTotal(items: DockerUsageItem[]): string {
  return formatBytes(items.reduce((sum, item) => sum + item.total_bytes, 0));
}

/** The state of a background job: one of the five states plus a label. */
interface JobState {
  kind: StatusKind;
  label: string;
  quiet: boolean;
}

function archiveState(
  archive: NonNullable<SystemInfo['log_archive']>,
): JobState {
  if (!archive.configured) {
    return { kind: 'stopped', label: '未設定', quiet: true };
  }
  if (archive.last_error !== '') {
    return { kind: 'failed', label: '直近の実行が失敗', quiet: false };
  }
  if (!archive.last_success_at) {
    return { kind: 'stopped', label: 'まだ実行されていません', quiet: true };
  }
  return { kind: 'running', label: '正常', quiet: true };
}

function caddyState(caddy: NonNullable<SystemInfo['caddy']>): JobState {
  if (!caddy.enabled) return { kind: 'stopped', label: '無効', quiet: true };
  if (caddy.last_error !== '') {
    return { kind: 'failed', label: '投入に失敗', quiet: false };
  }
  if (!caddy.last_loaded_at) {
    return { kind: 'pending', label: 'まだ投入されていません', quiet: false };
  }
  return { kind: 'running', label: '正常', quiet: true };
}

const RUN_KINDS: Record<BackupRun['status'], StatusKind> = {
  running: 'deploying',
  succeeded: 'running',
  failed: 'failed',
};

function runKind(run: BackupRun): StatusKind {
  return RUN_KINDS[run.status] ?? 'stopped';
}

function runLabel(run: BackupRun): string {
  return BACKUP_STATUS_LABELS[run.status] ?? run.status;
}

function runSucceeded(run: BackupRun): boolean {
  return run.status === 'succeeded';
}

function triggerLabel(run: BackupRun): string {
  return BACKUP_TRIGGER_LABELS[run.trigger] ?? run.trigger;
}

function duration(run: BackupRun): string {
  return formatDuration(run.started_at, run.finished_at) || '—';
}

function latestRuns(overview: BackupOverview): BackupRun[] {
  return (overview.runs ?? []).slice(0, RUNS_SHOWN);
}

export default class SystemTemplate extends Component {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  system = new Loadable<SystemInfo>(() => this.api.getSystem());
  docker = new Loadable<DockerUsageItem[]>(() => this.api.getDockerUsage());
  containers = new Loadable<ContainerResources>(() =>
    this.api.getContainerResources(),
  );
  backups = new Loadable<BackupOverview>(() => this.api.getBackups());
  github = new Loadable<GitHubAppStatus>(() => this.api.getGitHubApp());

  @tracked confirmingPrune = false;
  @tracked pruneImages = true;
  @tracked pruneBuilder = true;
  @tracked pruning = false;
  @tracked pruned = false;
  @tracked pruneOutput: string | null = null;
  @tracked prunedBytes: number | null = null;
  @tracked pruneError: unknown = null;

  @tracked startingBackup = false;
  @tracked backupError: unknown = null;

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.system.load();
    void this.docker.load();
    void this.containers.load();
    void this.backups.load();
    void this.github.load();
    this.poller.every(this, RUNNING_INTERVAL, () => this.followBackup());
    this.poller.every(this, HOST_INTERVAL, () => this.refreshHost());
  }

  get backupHealth() {
    return this.backups.value ? backupHealth(this.backups.value) : null;
  }

  get backupRunning(): boolean {
    return this.backups.value?.running ?? false;
  }

  /** The backup figures are still on their way for the first time. */
  get backupsPending(): boolean {
    return !this.backups.loaded && !this.backups.error;
  }

  /** What the last prune freed, as Docker reported it. */
  get prunedSize(): string | null {
    return this.prunedBytes === null ? null : formatBytes(this.prunedBytes);
  }

  get nothingToPrune(): boolean {
    return !this.pruneImages && !this.pruneBuilder;
  }

  /** The backend has no prune endpoint: said in place of the result. */
  get pruneUnavailable(): boolean {
    return unavailability(this.pruneError) !== null;
  }

  get stale(): boolean {
    return this.system.stale || this.containers.stale || this.backups.stale;
  }

  /** Follows a running backup until it finishes. */
  private async followBackup(): Promise<void> {
    if (this.backupRunning) await this.backups.load({ quiet: true });
  }

  private async refreshHost(): Promise<void> {
    const reads = [];
    if (this.system.loaded) reads.push(this.system.load({ quiet: true }));
    if (this.containers.loaded) {
      reads.push(this.containers.load({ quiet: true }));
    }
    if (this.backups.loaded && !this.backupRunning) {
      reads.push(this.backups.load({ quiet: true }));
    }
    await Promise.all(reads);
  }

  requestPrune = () => {
    this.pruneImages = true;
    this.pruneBuilder = true;
    this.confirmingPrune = true;
  };

  cancelPrune = () => {
    this.confirmingPrune = false;
  };

  prune = async () => {
    if (this.nothingToPrune) return;
    this.pruning = true;
    this.pruned = false;
    this.pruneOutput = null;
    this.prunedBytes = null;
    this.pruneError = null;
    try {
      const { output, reclaimed_bytes } = await this.api.prune({
        images: this.pruneImages,
        builder: this.pruneBuilder,
      });
      this.pruneOutput = output;
      this.prunedBytes = reclaimed_bytes ?? null;
      this.pruned = true;
    } catch (error) {
      this.pruneError = error;
      // `500 prune_failed` carries what the command printed until it failed.
      if (error instanceof ApiError) {
        const output = error.details['output'];
        if (typeof output === 'string') this.pruneOutput = output;
      }
    } finally {
      this.pruning = false;
      this.confirmingPrune = false;
    }
    // Whatever happened, the numbers on screen are now out of date.
    await Promise.all([
      this.system.load({ quiet: true }),
      this.docker.load({ quiet: true }),
    ]);
  };

  runBackup = async () => {
    if (this.startingBackup || this.backupRunning) return;
    this.startingBackup = true;
    this.backupError = null;
    try {
      await this.api.runBackup();
    } catch (error) {
      this.backupError = error;
    } finally {
      this.startingBackup = false;
    }
    await this.backups.load({ quiet: true });
  };

  <template>
    {{pageTitle "システム"}}

    <div class="page-header">
      <h1>システム</h1>
    </div>

    {{#if this.stale}}
      <div class="pane-notices">
        <Banner @tone="warning" data-test-stale>
          最新の状態を取得できていません。表示は最後に取得できた内容です。
        </Banner>
      </div>
    {{/if}}

    <section class="pane" aria-labelledby="resources-title">
      <div class="pane-header">
        <h2 class="pane-title" id="resources-title">ホストのリソース</h2>
      </div>

      <AsyncView
        @data={{this.system}}
        @errorTitle="ホストの状態を取得できませんでした"
      >
        <:loading>
          <div
            class="box resource-rows"
            role="status"
            aria-label="読み込み中"
            data-test-loading
          >
            {{#each SKELETON_ROWS as |n|}}
              <div class="resource" aria-hidden="true" data-skeleton={{n}}>
                <span class="skeleton system-skeleton"></span>
              </div>
            {{/each}}
          </div>
        </:loading>

        <:default as |system|>
          {{#if (diskOverLimit system)}}
            <div class="pane-notices">
              <Banner
                @tone="error"
                @title="ディスク使用率が上限を超えています"
                data-test-disk-warning
              >
                使用率が
                {{diskLimitPercent system}}%
                を下回るまで、デプロイを受け付けません。下の「不要なデータを削除」で空きを作ってください。
              </Banner>
            </div>
          {{/if}}

          <ul class="box resource-rows" data-test-system>
            {{#each (resources system) key="name" as |resource|}}
              <li
                class="resource"
                data-test-resource={{resource.name}}
                data-tone={{resource.tone}}
              >
                <span class="resource-name">{{resource.label}}</span>
                {{#if (hasPercent resource)}}
                  <span class="resource-value">
                    {{formatPercent resource.percent}}
                  </span>
                  <Meter
                    class="resource-meter"
                    @label="{{resource.label}}の使用率"
                    @percent={{resource.percent}}
                    @mark={{resource.mark}}
                    @tone={{resource.tone}}
                  />
                  <span class="resource-detail">{{resource.detail}}</span>
                  <span class="resource-flag">
                    {{#if resource.flag}}
                      <StatusIndicator
                        @kind={{resource.flag.kind}}
                        @label={{resource.flag.label}}
                        data-test-resource-flag
                      />
                    {{/if}}
                  </span>
                {{else}}
                  <span class="resource-missing">{{resource.missing}}</span>
                {{/if}}
              </li>
            {{/each}}
          </ul>
        </:default>
      </AsyncView>
    </section>

    <section class="pane" aria-labelledby="containers-title">
      <div class="pane-header">
        <h2 class="pane-title" id="containers-title">アプリごとのメモリと CPU</h2>
      </div>

      <AsyncView
        @data={{this.containers}}
        @errorTitle="コンテナのメモリと CPU を取得できませんでした"
      >
        <:loading>
          <div
            class="row-list container-rows"
            role="status"
            aria-label="読み込み中"
            data-test-containers-loading
          >
            {{#each SKELETON_ROWS as |n|}}
              <div class="row" aria-hidden="true" data-skeleton={{n}}>
                <span class="skeleton system-skeleton"></span>
              </div>
            {{/each}}
          </div>
        </:loading>

        <:default as |resources|>
          {{#if resources.groups.length}}
            <RowList
              class="container-rows"
              @label="アプリごとのメモリと CPU"
              data-test-containers
            >
              <:header>
                <span>アプリ</span>
                <span>メモリ</span>
                <span>CPU（{{resources.cpu_count}} コア）</span>
              </:header>
              <:rows as |Row|>
                {{#each (containerGroupRows resources) key="key" as |row|}}
                  <Row data-test-container-group={{row.label}}>
                    <span class="usage-app">
                      {{#if row.app}}
                        <LinkTo
                          @route="app.resources"
                          @model={{row.app}}
                          class="row-link usage-name"
                          data-test-container-app
                        >{{row.app}}</LinkTo>
                      {{else}}
                        <span class="usage-name">{{row.label}}</span>
                      {{/if}}
                      <span
                        class="usage-breakdown"
                        data-test-container-breakdown
                      >
                        {{row.breakdown}}
                      </span>
                    </span>
                    <span class="usage-cell usage-memory">
                      <span class="cell-label">メモリ</span>
                      <span class="usage-value" data-test-container-memory>
                        {{row.memory}}
                      </span>
                      <Meter
                        class="usage-meter"
                        @label="{{row.label}}のメモリ（ホスト全体に対する割合）"
                        @percent={{row.memoryPercent}}
                      />
                      <span class="usage-detail" data-test-container-limit>
                        {{row.limit}}
                        {{#if row.nearLimit}}
                          <StatusIndicator
                            @kind="pending"
                            @label="上限間近: {{row.nearLimit}}"
                            data-test-container-near-limit
                          />
                        {{/if}}
                      </span>
                    </span>
                    <span class="usage-cell usage-cpu">
                      <span class="cell-label">CPU</span>
                      <span class="usage-value" data-test-container-cpu>
                        {{row.cpu}}
                      </span>
                      <Meter
                        class="usage-meter"
                        @label="{{row.label}}の CPU（全コアに対する割合）"
                        @percent={{row.cpuShare}}
                      />
                    </span>
                  </Row>
                {{/each}}
              </:rows>
            </RowList>
            {{#let (containerTotals resources) as |totals|}}
              <p class="system-note" data-test-containers-total>
                コンテナ全体で、メモリ
                {{totals.memory}}（ホストの
                {{totals.memoryPercent}}）、CPU
                {{totals.cpu}}。CPU は Docker と同じく 1 コアを 100%
                とした値で、このホストは
                {{totals.cpuCount}}
                コアなので最大
                {{totals.cpuMax}}
                です。計測した瞬間の値です。メーターはホスト全体（メモリ
                {{totals.hostMemory}}、CPU
                {{totals.cpuMax}}）に対する割合です。「その他」は koroku
                のアプリではないコンテナです。
              </p>
            {{/let}}
          {{else}}
            <p class="system-note" data-test-containers-empty>
              稼働中のコンテナはありません。
            </p>
          {{/if}}
        </:default>
      </AsyncView>
    </section>

    {{#if this.system.value}}
      {{#let (reservationView this.system.value) as |reservation|}}
        {{#if reservation}}
          <section
            class="pane"
            aria-labelledby="reservation-title"
            data-test-reservation
          >
            <div class="pane-header">
              <h2
                class="pane-title"
                id="reservation-title"
              >本番アプリのメモリの予約</h2>
            </div>
            {{#if reservation.warning}}
              <div class="pane-notices">
                <Banner
                  @tone="warning"
                  @title="予約がホストのメモリに対して多すぎます"
                  data-test-reservation-warning
                >
                  本番アプリの予約の合計
                  {{reservation.total}}
                  に余白
                  {{reservation.margin}}
                  を足すと、ホストのメモリ
                  {{reservation.host}}
                  を超えます。予約どうしで回収を押し付け合い、本番を守る効き目が薄れます。アプリの設定タブで予約を減らしてください。
                </Banner>
              </div>
            {{/if}}
            <ul class="box resource-rows" data-test-reservation-total>
              <li
                class="resource"
                data-tone={{if reservation.warning "warn" "ok"}}
              >
                <span class="resource-name">予約の合計</span>
                {{#if reservation.host}}
                  <span class="resource-value">
                    {{formatPercent reservation.percent}}
                  </span>
                  <Meter
                    class="resource-meter"
                    @label="本番アプリのメモリの予約（ホストのメモリに対する割合）"
                    @percent={{reservation.percent}}
                    @mark={{reservation.mark}}
                    @tone={{if reservation.warning "warn" "ok"}}
                  />
                  <span class="resource-detail">
                    {{reservation.total}}
                    /
                    {{reservation.host}}{{#if reservation.room}}、あと
                      {{reservation.room}}
                      まで{{/if}}
                  </span>
                  <span class="resource-flag">
                    {{#if reservation.warning}}
                      <StatusIndicator
                        @kind="pending"
                        @label="余白不足"
                        data-test-reservation-flag
                      />
                    {{/if}}
                  </span>
                {{else}}
                  <span class="resource-missing">
                    {{reservation.total}}（ホストのメモリを取得できません）
                  </span>
                {{/if}}
              </li>
            </ul>
            {{#if reservation.apps.length}}
              <ul class="box reservation-apps" data-test-reservation-apps>
                {{#each reservation.apps key="slug" as |app|}}
                  <li
                    class="reservation-app"
                    data-test-reservation-app={{app.slug}}
                  >
                    <span class="usage-app">
                      <LinkTo
                        @route="app.settings"
                        @model={{app.slug}}
                        class="row-link usage-name"
                      >{{app.slug}}</LinkTo>
                      <span class="usage-breakdown">{{app.breakdown}}</span>
                      {{#if app.switched}}
                        <span
                          class="usage-breakdown"
                          data-test-reservation-switched
                        >ステージングに切り替え済み。次のデプロイで外れます。</span>
                      {{/if}}
                    </span>
                    <span class="usage-value">{{app.total}}</span>
                  </li>
                {{/each}}
              </ul>
            {{/if}}
            <p class="system-note" data-test-reservation-note>
              {{#if reservation.apps.length}}
                停止していない本番アプリが、稼働中のリリースで予約している量です。ホストのメモリが足りなくなったとき、カーネルは予約した量までを後回しにして、ステージングのアプリから先に回収します。合計にホストやビルド、ステージングのための余白
                {{reservation.margin}}
                を足して、ホストのメモリに収まるようにします。
              {{else}}
                予約している本番アプリはありません。本番のアプリをデプロイすると、各サービスのメモリ上限の半分（設定タブで変えられます）を予約します。
              {{/if}}
            </p>
          </section>
        {{/if}}
      {{/let}}
    {{/if}}

    <section class="pane" aria-labelledby="docker-title">
      <div class="pane-header">
        <h2 class="pane-title" id="docker-title">Docker のディスク使用量</h2>
        <Button
          @busy={{this.pruning}}
          @busyLabel="削除中…"
          data-test-prune-open
          {{on "click" this.requestPrune}}
        >不要なデータを削除…</Button>
      </div>

      <div class="pane-notices" aria-live="polite">
        {{#if this.pruneUnavailable}}
          <Banner @tone="warning" data-test-prune-unavailable>
            このサーバーは、不要なデータの削除にまだ対応していません。
          </Banner>
        {{else if this.pruneError}}
          <ErrorMessage @error={{this.pruneError}} />
        {{else if this.pruned}}
          <Banner data-test-pruned>
            不要なデータを削除しました。{{#if this.prunedSize}}Docker の報告では
              {{this.prunedSize}}
              空きました。{{/if}}
          </Banner>
        {{/if}}
        {{#if this.pruneOutput}}
          <pre
            class="code-block"
            aria-label="コマンドの出力"
            data-test-prune-output
          >{{this.pruneOutput}}</pre>
        {{/if}}
      </div>

      <AsyncView
        @data={{this.docker}}
        @errorTitle="Docker のディスク使用量を取得できませんでした"
      >
        <:loading>
          <div
            class="row-list docker-rows"
            role="status"
            aria-label="読み込み中"
            data-test-docker-loading
          >
            {{#each SKELETON_ROWS as |n|}}
              <div class="row" aria-hidden="true" data-skeleton={{n}}>
                <span class="skeleton system-skeleton"></span>
              </div>
            {{/each}}
          </div>
        </:loading>

        <:default as |items|>
          <RowList
            class="docker-rows"
            @label="Docker のディスク使用量"
            data-test-docker
          >
            <:header>
              <span>種類</span>
              <span class="docker-number">件数</span>
              <span class="docker-number">使用量</span>
              <span class="docker-number">空く見込み</span>
            </:header>
            <:rows as |Row|>
              {{#each (dockerRows items) key="type" as |item|}}
                <Row data-test-docker-item={{item.type}}>
                  <span class="docker-type">{{dockerLabel item}}</span>
                  <span class="docker-number">
                    <span class="cell-label">件数</span>
                    {{item.count}}
                  </span>
                  <span class="docker-number">
                    <span class="cell-label">使用量</span>
                    {{formatBytes item.total_bytes}}
                  </span>
                  <span class="docker-number docker-reclaimable">
                    <span class="cell-label">空く見込み</span>
                    {{reclaimable item}}
                  </span>
                </Row>
              {{/each}}
            </:rows>
          </RowList>
          <p class="system-note" data-test-docker-total>
            合計
            {{dockerTotal
              items
            }}。「空く見込み」は「不要なデータを削除」で空く量の目安で、少なめに出ます。消えるのは、どのコンテナにも使われていないタグのないイメージと、1
            週間使われていないビルドキャッシュです。ロールバック用に残している直近のリリースや停止中のアプリのイメージ、volume
            は消しません。
          </p>
        </:default>
      </AsyncView>
    </section>

    <section class="pane" aria-labelledby="jobs-title">
      <div class="pane-header">
        <h2 class="pane-title" id="jobs-title">定期処理</h2>
      </div>

      <div class="pane-notices" aria-live="polite">
        <ErrorMessage @error={{this.backupError}} />
      </div>

      <ul class="box job-rows">
        <li class="job" data-test-job="backup">
          <span class="job-name">バックアップ</span>
          {{#if this.backups.value}}
            {{#let this.backups.value as |backups|}}
              <span class="job-state" aria-live="polite" data-test-backup-state>
                {{#if this.backupHealth}}
                  <StatusIndicator
                    @kind={{this.backupHealth.kind}}
                    @label={{this.backupHealth.label}}
                    @quiet={{this.backupHealth.quiet}}
                  />
                {{/if}}
              </span>
              {{#if backups.configured}}
                <dl class="job-facts">
                  <div>
                    <dt>最終成功</dt>
                    <dd data-test-backup-last-success>
                      {{#if backups.last_success_at}}
                        <RelativeTime @time={{backups.last_success_at}} />
                      {{else}}
                        <span class="fact-none">まだありません</span>
                      {{/if}}
                    </dd>
                  </div>
                  <div>
                    <dt>次回</dt>
                    <dd data-test-backup-next-run>
                      {{#if backups.next_run_at}}
                        <TimeUntil @time={{backups.next_run_at}} />
                      {{else}}
                        <span class="fact-none">—</span>
                      {{/if}}
                    </dd>
                  </div>
                </dl>
                <span class="job-action">
                  <Button
                    @size="small"
                    @busy={{backups.running}}
                    @busyLabel="実行中…"
                    @disabled={{this.startingBackup}}
                    data-test-backup-run
                    {{on "click" this.runBackup}}
                  >今すぐ実行</Button>
                </span>
                {{#if this.backupHealth.failedRun}}
                  <p class="job-problem" data-test-backup-error>
                    <Icon @name="error" />
                    <span>
                      直近のエラー:
                      {{this.backupHealth.failedRun.error}}
                    </span>
                  </p>
                {{/if}}
              {{else}}
                <p class="job-note" data-test-backup-unconfigured>
                  いまは、どのアプリのデータも退避されていません。
                  <code>/etc/koroku/config.toml</code>
                  の
                  <code>[storage]</code>
                  と、restic のパスワードファイル
                  <code>/etc/koroku/restic.key</code>
                  を置くと有効になります。
                </p>
              {{/if}}
            {{/let}}
          {{else if this.backupsPending}}
            <span class="job-state">
              <span class="skeleton system-skeleton"></span>
              <span class="visually-hidden">読み込み中</span>
            </span>
          {{else if this.backups.unavailable}}
            <p class="job-note" data-test-backup-unavailable>
              このサーバーは、バックアップにまだ対応していません。
            </p>
          {{else}}
            <p class="job-problem" data-test-backup-load-error>
              <Icon @name="error" />
              <span>
                状況を取得できませんでした。{{this.backups.errorMessage}}
              </span>
            </p>
            <span class="job-action">
              <Button @size="small" {{on "click" this.backups.reload}}>
                再読み込み
              </Button>
            </span>
          {{/if}}
        </li>

        {{#if this.system.value.log_archive}}
          {{#let this.system.value.log_archive as |archive|}}
            <li class="job" data-test-job="log-archive">
              <span class="job-name">ログ退避</span>
              {{#let (archiveState archive) as |state|}}
                <span class="job-state" data-test-archive-state>
                  <StatusIndicator
                    @kind={{state.kind}}
                    @label={{state.label}}
                    @quiet={{state.quiet}}
                  />
                </span>
              {{/let}}
              {{#if archive.configured}}
                <dl class="job-facts">
                  <div>
                    <dt>最終成功</dt>
                    <dd>
                      {{#if archive.last_success_at}}
                        <RelativeTime @time={{archive.last_success_at}} />
                      {{else}}
                        <span class="fact-none">まだありません</span>
                      {{/if}}
                    </dd>
                  </div>
                  <div>
                    <dt>次回</dt>
                    <dd data-test-archive-next-run>
                      {{#if archive.next_run_at}}
                        <TimeUntil @time={{archive.next_run_at}} />
                      {{else}}
                        <span class="fact-none">—</span>
                      {{/if}}
                    </dd>
                  </div>
                </dl>
                {{#if archive.last_error}}
                  <p class="job-problem" data-test-archive-error>
                    <Icon @name="error" />
                    <span>直近のエラー: {{archive.last_error}}</span>
                  </p>
                {{/if}}
              {{else}}
                <p class="job-note">
                  オブジェクトストレージが未設定のため、ログはこのホストに 14
                  日分だけ残ります。
                </p>
              {{/if}}
            </li>
          {{/let}}
        {{/if}}

        {{#if this.system.value.caddy}}
          {{#let this.system.value.caddy as |caddy|}}
            <li class="job" data-test-job="caddy">
              <span class="job-name">Caddy への設定投入</span>
              {{#let (caddyState caddy) as |state|}}
                <span class="job-state" data-test-caddy-state>
                  <StatusIndicator
                    @kind={{state.kind}}
                    @label={{state.label}}
                    @quiet={{state.quiet}}
                  />
                </span>
              {{/let}}
              {{#if caddy.enabled}}
                <dl class="job-facts">
                  <div>
                    <dt>最終投入</dt>
                    <dd>
                      {{#if caddy.last_loaded_at}}
                        <RelativeTime @time={{caddy.last_loaded_at}} />
                      {{else}}
                        <span class="fact-none">まだありません</span>
                      {{/if}}
                    </dd>
                  </div>
                </dl>
                {{#if caddy.last_error}}
                  <p class="job-problem" data-test-caddy-error>
                    <Icon @name="error" />
                    <span>直近のエラー: {{caddy.last_error}}</span>
                  </p>
                {{/if}}
              {{else}}
                <p class="job-note">
                  設定で無効になっているため、経路の設定は Caddy
                  に投入されません。
                </p>
              {{/if}}
            </li>
          {{/let}}
        {{/if}}
      </ul>
    </section>

    {{#if this.backups.value.configured}}
      {{#let (latestRuns this.backups.value) as |runs|}}
        <section class="pane" aria-labelledby="runs-title">
          <div class="pane-header">
            <h2 class="pane-title" id="runs-title">バックアップの実行履歴</h2>
          </div>
          {{#if runs.length}}
            <RowList
              class="run-rows"
              @label="バックアップの実行履歴"
              data-test-backup-runs
            >
              <:header>
                <span>結果</span>
                <span>開始</span>
                <span>きっかけ</span>
                <span>所要時間</span>
                <span>エラー</span>
              </:header>
              <:rows as |Row|>
                {{#each runs key="id" as |run|}}
                  <Row data-test-backup-run-row={{run.id}}>
                    <span class="run-status">
                      <StatusIndicator
                        @kind={{runKind run}}
                        @label={{runLabel run}}
                        @quiet={{runSucceeded run}}
                      />
                    </span>
                    <span class="run-start">
                      <span class="cell-label">開始</span>
                      <RelativeTime @time={{run.started_at}} />
                    </span>
                    <span class="run-trigger">
                      <span class="cell-label">きっかけ</span>
                      {{triggerLabel run}}
                    </span>
                    <span class="run-duration">
                      <span class="cell-label">所要時間</span>
                      {{duration run}}
                    </span>
                    <span class="run-error">{{run.error}}</span>
                  </Row>
                {{/each}}
              </:rows>
            </RowList>
          {{else}}
            <p class="system-note" data-test-no-runs>
              まだ実行されていません。最初の定期実行を待つか、上の「今すぐ実行」で取得します。
            </p>
          {{/if}}
        </section>
      {{/let}}
    {{/if}}

    {{! A server without this endpoint has nothing to show here. }}
    {{#unless this.github.unavailable}}
      <section
        class="pane"
        id="github-app"
        aria-labelledby="github-title"
        data-test-github-app
      >
        <div class="pane-header">
          <h2 class="pane-title" id="github-title">GitHub App</h2>
        </div>
        <AsyncView
          @data={{this.github}}
          @errorTitle="GitHub App の情報を取得できませんでした"
          as |gh|
        >
          {{#if gh.app}}
            <dl class="readout">
              <dt>App</dt>
              <dd>
                <a
                  href={{gh.app.html_url}}
                  target="_blank"
                  rel="noopener noreferrer"
                  class="mono"
                  data-test-github-app-link
                >{{gh.app.slug}}</a>
              </dd>
            </dl>
            <ul class="box job-rows" data-test-github-installations>
              {{#each gh.installations as |inst|}}
                <li class="job" data-test-github-installation={{inst.account}}>
                  <span class="job-name mono">{{inst.account}}</span>
                  <span class="job-state">
                    {{#if inst.used}}
                      <StatusIndicator
                        @kind="running"
                        @label="使う"
                        @quiet={{true}}
                      />
                    {{else}}
                      <StatusIndicator @kind="pending" @label="使わない" />
                    {{/if}}
                  </span>
                  <span class="job-facts">{{selectionLabel
                      inst.repository_selection
                    }}</span>
                </li>
              {{else}}
                <li class="job" data-test-github-no-installation>
                  <span
                    class="job-name"
                  >まだどこにもインストールされていません。</span>
                </li>
              {{/each}}
            </ul>

            <div class="github-guide" data-test-github-guide>
              <h3 class="github-guide-title">別の org のリポジトリを使う</h3>
              <p class="github-guide-lead">
                自分が管理する別の org の private
                リポジトリをデプロイするには、App をその org
                にもインストールし、koroku
                が使うインストール先に加えます。public
                リポジトリなら、インストールせずに作成画面で URL
                を指定するだけで使えます。
              </p>
              <ol class="github-guide-steps">
                <li>
                  <span>App
                    をどのアカウントにもインストールできるようにします（App
                    の設定の
                    <strong>Advanced</strong>
                    →
                    <strong>Make public</strong>）。誰でもインストールできるようになりますが、koroku
                    は 3 で加えたアカウントのインストールしか使いません。</span>
                  {{#if gh.app.settings_url}}
                    <a
                      class={{buttonClass "default" "small"}}
                      href="{{gh.app.settings_url}}/advanced"
                      target="_blank"
                      rel="noopener noreferrer"
                      data-test-github-settings
                    >App の設定を開く<Icon @name="external" /></a>
                  {{/if}}
                </li>
                <li>
                  <span>App をその org
                    にインストールし、デプロイするリポジトリを選びます。</span>
                  <a
                    class={{buttonClass "default" "small"}}
                    href={{gh.app.install_url}}
                    target="_blank"
                    rel="noopener noreferrer"
                    data-test-github-install
                  >インストール先を選ぶ<Icon @name="external" /></a>
                </li>
                <li>
                  <span>サーバーの
                    <code>/etc/koroku/config.toml</code>
                    の
                    <code>[github]</code>
                    に次の行を書き、koroku
                    を再起動します。「使わない」と表示されているインストール先は、ここに加えると使われます。</span>
                  <span class="github-guide-config">
                    <code class="mono" data-test-github-config>{{accountsLine
                        gh
                      }}</code>
                    <CopyButton
                      @text={{accountsLine gh}}
                      @label="config.toml に書く行"
                    />
                  </span>
                </li>
              </ol>
            </div>
          {{else}}
            <p class="github-unconfigured" data-test-github-unconfigured>
              GitHub App が設定されていません。設定すると、GitHub
              でログインし、private リポジトリをデプロイできます（docs/SETUP.md
              の 4 節）。
            </p>
          {{/if}}
        </AsyncView>
      </section>
    {{/unless}}

    {{#if this.system.value}}
      {{#let this.system.value as |system|}}
        <section class="pane" aria-labelledby="versions-title">
          <div class="pane-header">
            <h2 class="pane-title" id="versions-title">バージョン</h2>
          </div>
          <dl class="readout system-versions">
            <dt>koroku</dt>
            <dd data-test-koroku-version>{{system.version}}</dd>
            <dt>Docker</dt>
            <dd data-test-docker-version>
              {{#if system.docker_version}}
                {{system.docker_version}}
              {{else}}
                <span class="fact-none">取得できません</span>
              {{/if}}
            </dd>
            <dt>ベースドメイン</dt>
            <dd><span class="mono">{{system.base_domain}}</span></dd>
          </dl>
        </section>
      {{/let}}
    {{/if}}

    {{#if this.confirmingPrune}}
      <ConfirmDialog
        @title="不要なデータを削除しますか？"
        @confirmLabel="削除する"
        @danger={{true}}
        @busy={{this.pruning}}
        @onConfirm={{this.prune}}
        @onCancel={{this.cancelPrune}}
      >
        <p>
          使われていないデータを Docker
          から消して、ディスクを空けます。稼働中のアプリには影響しません。消したものは戻せませんが、必要になれば次のビルドで作り直されます。
        </p>
        <fieldset class="option-group dialog-options">
          <legend class="visually-hidden">削除するもの</legend>
          <label class="option">
            <input
              type="checkbox"
              checked={{this.pruneImages}}
              disabled={{this.pruning}}
              data-test-prune-images
              {{on "change" (setChecked this "pruneImages")}}
            />
            <span class="option-body">
              <span class="option-label">タグのないイメージ</span>
              <span class="option-hint">
                ビルドで置き換えられ、どこからも参照されなくなったイメージです。
              </span>
            </span>
          </label>
          <label class="option">
            <input
              type="checkbox"
              checked={{this.pruneBuilder}}
              disabled={{this.pruning}}
              data-test-prune-builder
              {{on "change" (setChecked this "pruneBuilder")}}
            />
            <span class="option-body">
              <span class="option-label">ビルドキャッシュ</span>
              <span class="option-hint">
                1 週間使われていないものだけを消します。
              </span>
            </span>
          </label>
        </fieldset>
        {{#if this.nothingToPrune}}
          <p role="alert" data-test-prune-nothing>
            削除するものを 1 つ以上選んでください。
          </p>
        {{/if}}
      </ConfirmDialog>
    {{/if}}
  </template>
}
