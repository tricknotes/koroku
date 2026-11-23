import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ErrorMessage from 'koroku-console/components/error-message';
import Icon from 'koroku-console/components/icon';
import RelativeTime from 'koroku-console/components/relative-time';
import RowList from 'koroku-console/components/row-list';
import StatusIndicator from 'koroku-console/components/status-indicator';
import TimeUntil from 'koroku-console/components/time-until';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import { backupHealth } from 'koroku-console/utils/backup-status';
import { formatDuration } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import {
  BACKUP_STATUS_LABELS,
  BACKUP_TRIGGER_LABELS,
  type StatusKind,
} from 'koroku-console/utils/status';
import type {
  BackupOverview,
  BackupRun,
  SystemInfo,
} from 'koroku-console/utils/types';

/** A running backup is followed closely; the rest moves slowly. */
const RUNNING_INTERVAL = 5000;
const JOBS_INTERVAL = 30000;

/** How many of the latest backup runs are listed. */
const RUNS_SHOWN = 8;

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

/**
 * 定期処理 of the system screen (ARCHITECTURE 9.1): the backup, the log
 * archive and the Caddy configuration, and the latest backup runs.
 */
export default class SystemJobsTemplate extends Component {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  system = new Loadable<SystemInfo>(() => this.api.getSystem());
  backups = new Loadable<BackupOverview>(() => this.api.getBackups());

  @tracked startingBackup = false;
  @tracked backupError: unknown = null;

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.system.load();
    void this.backups.load();
    this.poller.every(this, RUNNING_INTERVAL, () => this.followBackup());
    this.poller.every(this, JOBS_INTERVAL, () => this.refresh());
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

  get stale(): boolean {
    return this.system.stale || this.backups.stale;
  }

  /** Follows a running backup until it finishes. */
  private async followBackup(): Promise<void> {
    if (this.backupRunning) await this.backups.load({ quiet: true });
  }

  private async refresh(): Promise<void> {
    const reads = [];
    if (this.system.loaded) reads.push(this.system.load({ quiet: true }));
    if (this.backups.loaded && !this.backupRunning) {
      reads.push(this.backups.load({ quiet: true }));
    }
    await Promise.all(reads);
  }

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
    {{pageTitle "定期処理"}}

    {{#if this.stale}}
      <div class="pane-notices">
        <Banner @tone="warning" data-test-stale>
          最新の状態を取得できていません。表示は最後に取得できた内容です。
        </Banner>
      </div>
    {{/if}}

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
  </template>
}
