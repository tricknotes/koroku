import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import AsyncView from 'paas-console/components/async-view';
import Badge from 'paas-console/components/badge';
import ErrorMessage from 'paas-console/components/error-message';
import FeatureUnavailable from 'paas-console/components/feature-unavailable';
import RelativeTime from 'paas-console/components/relative-time';
import type ApiService from 'paas-console/services/api';
import type PollerService from 'paas-console/services/poller';
import { preventDefault, setChecked } from 'paas-console/utils/dom';
import { unavailability } from 'paas-console/utils/errors';
import {
  formatBytes,
  formatDateTime,
  formatDuration,
  formatPercent,
} from 'paas-console/utils/format';
import Loadable from 'paas-console/utils/loadable';
import {
  BACKUP_STATUS_LABELS,
  BACKUP_STATUS_TONES,
  BACKUP_TRIGGER_LABELS,
} from 'paas-console/utils/status';
import type {
  BackupOverview,
  BackupRun,
  SystemInfo,
} from 'paas-console/utils/types';

const POLL_INTERVAL = 5000;

function runStatusLabel(run: BackupRun): string {
  return BACKUP_STATUS_LABELS[run.status] ?? run.status;
}

function runStatusTone(run: BackupRun) {
  return BACKUP_STATUS_TONES[run.status] ?? 'muted';
}

function runTriggerLabel(run: BackupRun): string {
  return BACKUP_TRIGGER_LABELS[run.trigger] ?? run.trigger;
}

function memoryUsedBytes(memory: NonNullable<SystemInfo['memory']>): number {
  return Math.max(0, memory.total_bytes - memory.available_bytes);
}

function memoryUsedPercent(memory: NonNullable<SystemInfo['memory']>): number {
  if (memory.total_bytes <= 0) return 0;
  return (memoryUsedBytes(memory) / memory.total_bytes) * 100;
}

/** Deployments are refused above this disk usage (ARCHITECTURE 8.2). */
const DISK_LIMIT_PERCENT = 85;

function diskIsHigh(disk: NonNullable<SystemInfo['disk']>): boolean {
  return disk.used_percent > DISK_LIMIT_PERCENT;
}

export default class SystemTemplate extends Component {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  system = new Loadable<SystemInfo>(() => this.api.getSystem());
  backups = new Loadable<BackupOverview>(() => this.api.getBackups());

  @tracked pruneImages = true;
  @tracked pruneBuilder = true;
  @tracked pruning = false;
  @tracked pruneOutput: string | null = null;
  @tracked pruneError: unknown = null;

  @tracked startingBackup = false;
  @tracked backupStarted = false;
  @tracked backupError: unknown = null;

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.system.load();
    void this.backups.load();
    this.poller.every(this, POLL_INTERVAL, () => this.refreshWhileRunning());
  }

  get pruneUnavailable() {
    return unavailability(this.pruneError);
  }

  get nothingToPrune(): boolean {
    return !this.pruneImages && !this.pruneBuilder;
  }

  /** Follows a running backup until it finishes. */
  async refreshWhileRunning(): Promise<void> {
    if (this.backups.value?.running) await this.backups.load({ quiet: true });
  }

  prune = async () => {
    this.pruning = true;
    this.pruneOutput = null;
    this.pruneError = null;
    try {
      const { output } = await this.api.prune({
        images: this.pruneImages,
        builder: this.pruneBuilder,
      });
      this.pruneOutput = output;
      await this.system.load({ quiet: true });
    } catch (error) {
      this.pruneError = error;
    } finally {
      this.pruning = false;
    }
  };

  runBackup = async () => {
    this.startingBackup = true;
    this.backupStarted = false;
    this.backupError = null;
    try {
      await this.api.runBackup();
      this.backupStarted = true;
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
      <h2>システム</h2>
    </div>

    <section class="card" aria-labelledby="resources-title">
      <h3 id="resources-title">リソース</h3>
      <AsyncView @data={{this.system}} as |system|>
        <dl class="definition" data-test-system>
          <dt>ディスク</dt>
          <dd data-test-disk>
            {{#if system.disk}}
              <meter
                class="meter"
                min="0"
                max="100"
                high="85"
                value={{system.disk.used_percent}}
                aria-label="ディスク使用率"
              ></meter>
              {{formatPercent system.disk.used_percent}}
              <span class="muted">（{{formatBytes system.disk.used_bytes}}
                /
                {{formatBytes system.disk.total_bytes}}）</span>
              {{#if (diskIsHigh system.disk)}}
                <p class="alert alert-danger" data-test-disk-warning>
                  使用率が 85% を超えているため、デプロイは拒否されます。下の
                  prune で空きを作ってください。
                </p>
              {{/if}}
            {{else}}
              <span class="muted">取得できません</span>
            {{/if}}
          </dd>
          <dt>メモリ</dt>
          <dd data-test-memory>
            {{#if system.memory}}
              <meter
                class="meter"
                min="0"
                max="100"
                high="90"
                value={{memoryUsedPercent system.memory}}
                aria-label="メモリ使用率"
              ></meter>
              {{formatPercent (memoryUsedPercent system.memory)}}
              <span class="muted">（{{formatBytes
                  (memoryUsedBytes system.memory)
                }}
                /
                {{formatBytes system.memory.total_bytes}}、空き
                {{formatBytes system.memory.available_bytes}}）</span>
            {{else}}
              <span class="muted">取得できません</span>
            {{/if}}
          </dd>
          <dt>Docker</dt>
          <dd data-test-docker-version>
            {{#if system.docker_version}}
              {{system.docker_version}}
            {{else}}
              <span class="muted">取得できません</span>
            {{/if}}
          </dd>
          <dt>paasd</dt>
          <dd>{{system.version}}</dd>
          <dt>ベースドメイン</dt>
          <dd>{{system.base_domain}}</dd>
        </dl>
      </AsyncView>
    </section>

    <section class="card" aria-labelledby="prune-title">
      <h3 id="prune-title">不要なデータの削除（prune）</h3>
      <p
        class="muted"
      >使われていないイメージとビルドキャッシュを消して、ディスクを空けます。稼働中のアプリには影響しません。</p>

      {{#if this.pruneUnavailable}}
        <FeatureUnavailable @kind={{this.pruneUnavailable}} />
      {{else}}
        <form
          class="form"
          data-test-prune-form
          {{on "submit" (preventDefault this.prune)}}
        >
          <label class="choice">
            <input
              type="checkbox"
              checked={{this.pruneImages}}
              data-test-prune-images
              {{on "change" (setChecked this "pruneImages")}}
            />
            使われていないイメージ
          </label>
          <label class="choice">
            <input
              type="checkbox"
              checked={{this.pruneBuilder}}
              data-test-prune-builder
              {{on "change" (setChecked this "pruneBuilder")}}
            />
            ビルドキャッシュ
          </label>
          <div class="form-actions">
            <button
              type="submit"
              class="button"
              disabled={{if this.pruning true this.nothingToPrune}}
              data-test-prune-submit
            >{{if this.pruning "実行中…" "prune を実行"}}</button>
          </div>
        </form>

        <div aria-live="polite">
          <ErrorMessage @error={{this.pruneError}} />
          {{#if this.pruneOutput}}
            <pre class="code" data-test-prune-output>{{this.pruneOutput}}</pre>
          {{/if}}
        </div>
      {{/if}}
    </section>

    <section class="card" aria-labelledby="backup-title">
      <h3 id="backup-title">バックアップ</h3>
      <AsyncView @data={{this.backups}} as |backups|>
        {{#if backups.configured}}
          {{#if backups.warning}}
            <p class="alert alert-danger" role="alert" data-test-backup-warning>
              24
              時間以上バックアップに成功していません。直近の実行結果を確認してください。
            </p>
          {{/if}}

          <dl class="definition">
            <dt>最終成功</dt>
            <dd data-test-backup-last-success>
              {{#if backups.last_success_at}}
                {{formatDateTime backups.last_success_at}}
                <span class="muted">
                  <RelativeTime @time={{backups.last_success_at}} />
                </span>
              {{else}}
                <span class="muted">まだ成功していません</span>
              {{/if}}
            </dd>
            <dt>状態</dt>
            <dd aria-live="polite" data-test-backup-running>
              {{if backups.running "実行中" "待機中（毎日 03:00 JST に実行）"}}
            </dd>
          </dl>

          <div class="form-actions">
            <button
              type="button"
              class="button"
              disabled={{if this.startingBackup true backups.running}}
              data-test-backup-run
              {{on "click" this.runBackup}}
            >今すぐバックアップ</button>
          </div>

          <div aria-live="polite">
            {{#if this.backupStarted}}
              <p
                class="notice"
                data-test-backup-started
              >バックアップを開始しました。</p>
            {{/if}}
            <ErrorMessage @error={{this.backupError}} />
          </div>

          <h4>直近の実行</h4>
          {{#if backups.runs.length}}
            <div class="table-wrap">
              <table class="table" data-test-backup-runs>
                <thead>
                  <tr>
                    <th scope="col">開始</th>
                    <th scope="col">きっかけ</th>
                    <th scope="col">結果</th>
                    <th scope="col">所要</th>
                    <th scope="col">エラー</th>
                  </tr>
                </thead>
                <tbody>
                  {{#each backups.runs key="id" as |run|}}
                    <tr data-test-backup-run-row={{run.id}}>
                      <td>{{formatDateTime run.started_at}}</td>
                      <td>{{runTriggerLabel run}}</td>
                      <td>
                        <Badge @tone={{runStatusTone run}}>
                          {{runStatusLabel run}}
                        </Badge>
                      </td>
                      <td>{{formatDuration run.started_at run.finished_at}}</td>
                      <td>{{run.error}}</td>
                    </tr>
                  {{/each}}
                </tbody>
              </table>
            </div>
          {{else}}
            <p class="muted">実行履歴はまだありません。</p>
          {{/if}}
        {{else}}
          <FeatureUnavailable @kind="not_configured" />
        {{/if}}
      </AsyncView>
    </section>
  </template>
}
