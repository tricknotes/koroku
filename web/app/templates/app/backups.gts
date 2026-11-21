import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import AsyncView from 'koroku-console/components/async-view';
import Badge from 'koroku-console/components/badge';
import Banner from 'koroku-console/components/banner';
import Button, { buttonClass } from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import DbCopyPanel from 'koroku-console/components/db-copy-panel';
import CopyButton from 'koroku-console/components/copy-button';
import EmptyState from 'koroku-console/components/empty-state';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import RelativeTime from 'koroku-console/components/relative-time';
import RoleNote from 'koroku-console/components/role-note';
import RowList from 'koroku-console/components/row-list';
import StatusIndicator from 'koroku-console/components/status-indicator';
import TimeUntil from 'koroku-console/components/time-until';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type SessionService from 'koroku-console/services/session';
import type AppContext from 'koroku-console/utils/app-context';
import BackupForm, {
  parseList,
  volumeChoices,
  type VolumeChoice,
} from 'koroku-console/utils/backup-form';
import {
  appBackupHealth,
  targetLabel,
} from 'koroku-console/utils/backup-status';
import { findPreset, HOOK_PRESETS } from 'koroku-console/utils/db-copy';
import {
  inputChecked,
  inputValue,
  preventDefault,
  setChecked,
  setValue,
} from 'koroku-console/utils/dom';
import { fieldErrors } from 'koroku-console/utils/errors';
import { formatBytes, formatDateTimeJst } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import { can } from 'koroku-console/utils/roles';
import { BACKUP_TRIGGER_LABELS } from 'koroku-console/utils/status';
import type {
  App,
  AppVolume,
  AppBackupStatus,
  BackupOverview,
  BackupRun,
  Snapshot,
} from 'koroku-console/utils/types';

/** A running backup is followed closely; an idle one is only looked at. */
const RUNNING_INTERVAL = 5000;
const IDLE_INTERVAL = 30000;

const SKELETON_ROWS = [1, 2, 3];

const KIND_LABELS: Record<Snapshot['kind'], string> = {
  volume: 'volume',
  dump: 'ダンプ',
};

function kindLabel(snapshot: Snapshot): string {
  return KIND_LABELS[snapshot.kind] ?? snapshot.kind;
}

function isVolume(snapshot: Snapshot): boolean {
  return snapshot.kind === 'volume';
}

function size(snapshot: Snapshot): string {
  return formatBytes(snapshot.size_bytes) || '—';
}

function restoreLabel(snapshot: Snapshot): string {
  return `${snapshot.target} をこのスナップショットからリストア`;
}

function downloadLabel(snapshot: Snapshot): string {
  return `${snapshot.target} のダンプをダウンロード`;
}

function triggerLabel(run: Pick<BackupRun, 'trigger'>): string {
  return BACKUP_TRIGGER_LABELS[run.trigger] ?? run.trigger;
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppBackupsTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;
  @service declare session: SessionService;

  overview = new Loadable<BackupOverview>(() => this.api.getBackups());
  /** This app's outcome only: other apps' failures stay out of this tab. */
  status = new Loadable<AppBackupStatus>(() =>
    this.api.getAppBackupStatus(this.slug),
  );
  snapshots = new Loadable<Snapshot[]>(() => this.api.listSnapshots(this.slug));
  /** The app's named volumes; the settings offer them as checkboxes. */
  volumeList = new Loadable<AppVolume[]>(() => this.api.listVolumes(this.slug));

  @tracked starting = false;
  @tracked runError: unknown = null;

  @tracked restoring: Snapshot | null = null;
  @tracked restoreBusy = false;
  @tracked restored: Snapshot | null = null;
  @tracked restoreError: unknown = null;

  @tracked form: BackupForm;
  /** Volume names typed in by hand, so they stay listed until saved. */
  @tracked addedVolumes: string[] = [];
  @tracked volumeName = '';
  @tracked saving = false;
  @tracked saved = false;
  @tracked nothingToSave = false;
  @tracked saveError: unknown = null;
  @tracked errors: Record<string, string> = {};

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.form = new BackupForm(this.app);
    void this.load();
    this.poller.every(this, RUNNING_INTERVAL, () => this.followRun());
    this.poller.every(this, IDLE_INTERVAL, () => this.refreshOverview());
  }

  get slug(): string {
    return this.args.model.slug;
  }

  get app(): App {
    return this.args.model.app;
  }

  /** Running, restoring, downloading and the settings need manage. */
  get canManage(): boolean {
    return can(this.app, 'manage');
  }

  get readOnly(): boolean {
    return !this.canManage;
  }

  get isOwner(): boolean {
    return this.session.isOwner;
  }

  get health() {
    const status = this.status.value;
    return status ? appBackupHealth(this.overview.value, status) : null;
  }

  /** When all of this app's targets were last stored. */
  get lastSuccessAt(): string | null {
    const status = this.status.value;
    return status ? status.last_success_at : null;
  }

  get running(): boolean {
    return this.overview.value?.running ?? false;
  }

  /** Loaded and nothing there: the empty state then holds the one action. */
  get noSnapshots(): boolean {
    return this.snapshots.loaded && this.snapshots.value?.length === 0;
  }

  /** The first answer about the volumes is still on its way. */
  get volumesLoading(): boolean {
    return !this.volumeList.loaded && !this.volumeList.error;
  }

  /**
   * The volume list could not be read. A backend that does not have the
   * endpoint is not a failure to report: the tab then works as it did
   * before the list existed.
   */
  get volumesFailed(): boolean {
    return Boolean(this.volumeList.error) && !this.volumeList.unavailable;
  }

  /** The volume list is on screen (and not what stands in for it). */
  get volumesListed(): boolean {
    return this.volumeList.loaded && !this.volumeList.error;
  }

  /**
   * The app's named volumes with what the form says about each. Without
   * the volume list, the volumes that have a snapshot stand in for it.
   */
  get volumes(): VolumeChoice[] {
    return volumeChoices({
      listed: this.volumesListed ? this.volumeList.value : null,
      snapshots: this.snapshots.value ?? [],
      excluded: this.form.excluded,
      also: [...(this.app.backup_exclude_volumes ?? []), ...this.addedVolumes],
    });
  }

  get restoreExample(): string {
    const service = this.app.dump_hook_service || 'db';
    return `docker compose -p koroku-${this.slug} exec -T ${service} psql -U postgres app < dump.sql`;
  }

  get otherErrors(): string[] {
    const known = new Set([
      'backup_exclude_volumes',
      'dump_hook_service',
      'dump_hook_command',
    ]);
    return Object.entries(this.errors)
      .filter(([name]) => !known.has(name))
      .map(([name, reason]) => `${name}: ${reason}`);
  }

  /**
   * The overview first: without backups there are no snapshots to ask for
   * and no settings to show the volumes in.
   */
  private async load(): Promise<void> {
    await this.overview.load();
    if (!this.overview.value?.configured) return;
    await Promise.all([
      this.status.load(),
      this.snapshots.load(),
      this.volumeList.load(),
    ]);
  }

  /** What a finished run may have changed: snapshots, and volumes it saw. */
  private async refreshResults(): Promise<void> {
    await Promise.all([
      this.status.load({ quiet: true }),
      this.snapshots.load({ quiet: true }),
      this.volumeList.load({ quiet: true }),
    ]);
  }

  reload = () => this.load();

  /** Follows a running backup until it ends, then shows what it produced. */
  private async followRun(): Promise<void> {
    if (!this.running) return;
    await this.overview.load({ quiet: true });
    if (!this.running) await this.refreshResults();
  }

  /** Notices a scheduled run that started while the tab was open. */
  private async refreshOverview(): Promise<void> {
    if (!this.overview.value?.configured || this.running) return;
    await this.overview.load({ quiet: true });
  }

  downloadUrl = (snapshot: Snapshot): string =>
    this.api.snapshotDownloadUrl(this.slug, snapshot.id);

  /**
   * A download is a plain link, so a `reauth_required` answer would land in
   * a file. Ask for the new login before leaving instead.
   */
  checkDownload = (event: Event) => {
    if (this.session.recentlyAuthenticated) return;
    event.preventDefault();
    this.session.promptReauth();
  };

  runNow = async () => {
    if (this.starting || this.running) return;
    this.starting = true;
    this.runError = null;
    try {
      await this.api.runBackup();
    } catch (error) {
      this.runError = error;
    } finally {
      this.starting = false;
    }
    await this.overview.load({ quiet: true });
    if (!this.running) await this.refreshResults();
  };

  requestRestore = (snapshot: Snapshot) => {
    this.restoring = snapshot;
    this.restored = null;
    this.restoreError = null;
  };

  cancelRestore = () => {
    this.restoring = null;
  };

  confirmRestore = async () => {
    const snapshot = this.restoring;
    if (!snapshot) return;
    this.restoreBusy = true;
    try {
      await this.api.restoreSnapshot(this.slug, snapshot.id);
      this.restored = snapshot;
    } catch (error) {
      this.restoreError = error;
    } finally {
      this.restoreBusy = false;
      this.restoring = null;
    }
    // The restore stops and starts the app.
    await this.args.model.reload();
  };

  toggleVolume = (name: string, event: Event) => {
    this.form.setIncluded(name, inputChecked(event));
    this.touch();
  };

  setVolumeName = (event: Event) => {
    this.volumeName = inputValue(event);
  };

  /** Takes the typed names out of the backup. */
  excludeTyped = () => {
    const names = parseList(this.volumeName);
    if (names.length === 0) return;
    this.addedVolumes = [...new Set([...this.addedVolumes, ...names])];
    for (const name of names) this.form.setIncluded(name, false);
    this.volumeName = '';
    this.touch();
  };

  /** Enter in the name field adds the name instead of saving the form. */
  excludeOnEnter = (event: KeyboardEvent) => {
    if (event.key !== 'Enter') return;
    event.preventDefault();
    this.excludeTyped();
  };

  touch = () => {
    this.saved = false;
    this.nothingToSave = false;
  };

  /** Fills the dump hook from a preset; the choice itself is not kept. */
  applyDumpPreset = (event: Event) => {
    const preset = findPreset(inputValue(event));
    if (preset) {
      this.form.dumpHookService = preset.service;
      this.form.dumpHookCommand = preset.dump;
    }
    (event.target as HTMLSelectElement).value = '';
    this.touch();
  };

  save = async () => {
    this.saved = false;
    this.nothingToSave = false;
    this.saveError = null;
    this.errors = this.form.validate();
    if (Object.keys(this.errors).length > 0) return;

    const patch = this.form.patch(this.app);
    if (Object.keys(patch).length === 0) {
      this.nothingToSave = true;
      return;
    }

    this.saving = true;
    try {
      const app = await this.api.updateApp(this.slug, patch);
      this.args.model.set(app);
      this.form = new BackupForm(app);
      // What was typed in is saved now, or was put back in and is gone.
      this.addedVolumes = [];
      this.saved = true;
    } catch (error) {
      this.saveError = error;
      this.errors = fieldErrors(error);
    } finally {
      this.saving = false;
    }
    // The list follows the saved exclusions: a name that matches no volume
    // is only listed while it is excluded.
    if (this.saved) await this.volumeList.load({ quiet: true });
  };

  <template>
    <AsyncView
      @data={{this.overview}}
      @errorTitle="バックアップの状況を取得できませんでした"
      as |overview|
    >
      {{#if overview.configured}}
        <div class="pane-notices" aria-live="polite">
          {{#if this.overview.stale}}
            <Banner @tone="warning" data-test-stale>
              最新の状態を取得できていません。表示は最後に取得できた内容です。
            </Banner>
          {{/if}}
          {{#if this.health.failedRun}}
            <Banner
              @tone="error"
              @title={{if
                this.health.failedRun.whole_run
                "直近のバックアップの実行が、取得の前に失敗しました"
                "直近のバックアップで、このアプリの取得に失敗しました"
              }}
              data-test-backup-failed
            >
              {{#if this.health.failedRun.whole_run}}
                <span>
                  {{#if this.health.failedRun.error}}
                    {{this.health.failedRun.error}}
                  {{else}}
                    このアプリを含め、どのアプリも取得できていません。
                  {{/if}}
                </span>
              {{else}}
                <ul class="banner-list" data-test-backup-failures>
                  {{#each this.health.failures as |failure|}}
                    <li><strong>{{targetLabel failure}}</strong>:
                      {{failure.error}}</li>
                  {{/each}}
                </ul>
              {{/if}}
              <span class="banner-detail">
                {{triggerLabel this.health.failedRun}}実行、開始は
                <RelativeTime @time={{this.health.failedRun.started_at}} />
              </span>
              {{#if this.health.stale}}
                <span class="banner-detail">
                  24 時間以上、このアプリのバックアップが成功していません。
                </span>
              {{/if}}
            </Banner>
          {{else if this.health.stale}}
            <Banner @tone="warning" data-test-backup-warning>
              {{#if this.status.value.last_success_at}}
                24 時間以上、このアプリのバックアップが成功していません。
              {{else}}
                このアプリはまだ一度もバックアップに成功していません。
              {{/if}}
            </Banner>
          {{/if}}
          {{#if this.health.warnings.length}}
            <Banner
              @tone="warning"
              @title="一部のファイルを読めずに保存しました"
              data-test-backup-target-warnings
            >
              <ul class="banner-list">
                {{#each this.health.warnings as |warning|}}
                  <li><strong>{{targetLabel warning}}</strong>:
                    {{warning.error}}</li>
                {{/each}}
              </ul>
              <span
                class="banner-detail"
              >稼働中に変わったか消えたファイルです。スナップショットは保存されています。</span>
            </Banner>
          {{/if}}
          {{#if this.restored}}
            <Banner data-test-restored>
              volume
              <strong>{{this.restored.target}}</strong>
              をリストアしました。アプリは元の状態に戻っています。
            </Banner>
          {{/if}}
          <ErrorMessage @error={{this.restoreError}} />
          <ErrorMessage @error={{this.runError}} />
        </div>

        <section class="pane" aria-labelledby="backup-status-title">
          <h2 class="visually-hidden" id="backup-status-title">
            バックアップの状況
          </h2>
          <div class="box backup-summary" data-test-backup-summary>
            <dl class="facts">
              <div class="fact">
                <dt>状態</dt>
                <dd aria-live="polite" data-test-backup-state>
                  {{#if this.health}}
                    <StatusIndicator
                      @kind={{this.health.kind}}
                      @label={{this.health.label}}
                      @quiet={{this.health.quiet}}
                    />
                  {{/if}}
                </dd>
              </div>
              <div class="fact">
                <dt>最終成功</dt>
                <dd data-test-backup-last-success>
                  {{#if this.lastSuccessAt}}
                    <RelativeTime @time={{this.lastSuccessAt}} />
                  {{else}}
                    <span class="fact-none">まだありません</span>
                  {{/if}}
                </dd>
              </div>
              <div class="fact">
                <dt>次回の実行</dt>
                <dd data-test-backup-next-run>
                  {{#if overview.next_run_at}}
                    <TimeUntil @time={{overview.next_run_at}} />
                  {{else}}
                    <span class="fact-none">—</span>
                  {{/if}}
                </dd>
              </div>
            </dl>
            {{#if this.canManage}}
              {{#unless this.noSnapshots}}
                <Button
                  @busy={{overview.running}}
                  @busyLabel="実行中…"
                  @disabled={{this.starting}}
                  data-test-backup-run
                  {{on "click" this.runNow}}
                >今すぐ実行</Button>
              {{/unless}}
            {{/if}}
          </div>
          <p class="backup-note">
            バックアップは、すべてのアプリと基盤の設定をまとめて 1
            回の実行で取ります。{{#if this.isOwner}}実行の履歴は<LinkTo
                @route="system"
              >システム</LinkTo>にあります。{{/if}}
          </p>
        </section>

        <section class="pane" aria-labelledby="snapshots-title">
          <div class="pane-header">
            <h2 class="pane-title" id="snapshots-title">スナップショット</h2>
          </div>

          <AsyncView
            @data={{this.snapshots}}
            @errorTitle="スナップショットの一覧を取得できませんでした"
          >
            <:loading>
              <div
                class="row-list snapshot-rows"
                role="status"
                aria-label="読み込み中"
                data-test-loading
              >
                {{#each SKELETON_ROWS as |n|}}
                  <div class="row" aria-hidden="true" data-skeleton={{n}}>
                    <span class="skeleton snapshot-skeleton"></span>
                  </div>
                {{/each}}
              </div>
            </:loading>

            <:default as |snapshots|>
              {{#if snapshots.length}}
                <RowList class="snapshot-rows" @label="スナップショット">
                  <:header>
                    <span>取得日時</span>
                    <span>種類</span>
                    <span>対象</span>
                    <span>サイズ</span>
                    <span>ID</span>
                  </:header>
                  <:rows as |Row|>
                    {{#each snapshots key="id" as |snapshot|}}
                      <Row data-test-snapshot={{snapshot.id}}>
                        <div class="snapshot-time">
                          <span class="cell-label">取得日時</span>
                          <RelativeTime @time={{snapshot.time}} />
                        </div>
                        <div class="snapshot-kind">
                          <span class="visually-hidden">種類</span>
                          <Badge data-test-snapshot-kind>
                            {{kindLabel snapshot}}
                          </Badge>
                        </div>
                        <div class="snapshot-target">
                          <span class="cell-label">対象</span>
                          <span>{{snapshot.target}}</span>
                        </div>
                        <div class="snapshot-size" data-test-snapshot-size>
                          <span class="cell-label">サイズ</span>
                          <span>{{size snapshot}}</span>
                        </div>
                        <div class="snapshot-id">
                          <span class="cell-label">ID</span>
                          <span class="mono">{{snapshot.id}}</span>
                        </div>
                        <div class="snapshot-action">
                          {{#if this.readOnly}}
                            {{! Restoring and downloading need manage. }}
                          {{else if (isVolume snapshot)}}
                            <Button
                              @size="small"
                              @disabled={{this.restoreBusy}}
                              class="button-danger-outline"
                              aria-label={{restoreLabel snapshot}}
                              data-test-snapshot-restore
                              {{on "click" (fn this.requestRestore snapshot)}}
                            >リストア</Button>
                          {{else}}
                            <a
                              class={{buttonClass "default" "small"}}
                              href={{this.downloadUrl snapshot}}
                              download
                              aria-label={{downloadLabel snapshot}}
                              data-test-snapshot-download
                              {{on "click" this.checkDownload}}
                            >ダウンロード</a>
                          {{/if}}
                        </div>
                      </Row>
                    {{/each}}
                  </:rows>
                </RowList>

                <details class="disclosure" data-test-dump-help>
                  <summary>ダンプを DB に戻すには</summary>
                  <p>
                    ダンプの流し込みは手動です。ダウンロードしたファイルをサーバーに置き、DB
                    のサービスへ標準入力で渡します。PostgreSQL の例:
                  </p>
                  <div class="command">
                    <pre class="code-block">{{this.restoreExample}}</pre>
                    <CopyButton
                      @text={{this.restoreExample}}
                      @label="コマンド"
                    />
                  </div>
                  <p>
                    サービス名、ユーザー名、データベース名はアプリに合わせて読み替えてください。
                  </p>
                </details>
              {{else}}
                <div class="box" data-test-no-snapshots>
                  <EmptyState
                    @title="スナップショットはまだありません"
                    @description="定期実行を待つか、いま 1 回実行すると、このアプリの volume とダンプがここに並びます。"
                  >
                    <:action>
                      {{#if this.canManage}}
                        <Button
                          @busy={{overview.running}}
                          @busyLabel="実行中…"
                          @disabled={{this.starting}}
                          data-test-backup-run
                          {{on "click" this.runNow}}
                        >今すぐ実行</Button>
                      {{/if}}
                    </:action>
                  </EmptyState>
                </div>
              {{/if}}
            </:default>
          </AsyncView>
        </section>

        <section class="pane" aria-labelledby="backup-settings-title">
          <div class="pane-header">
            <h2 class="pane-title" id="backup-settings-title">
              バックアップの設定
            </h2>
          </div>

          {{#if this.form.available}}
            {{#if this.readOnly}}
              <RoleNote @needed="manage" @what="バックアップの設定の変更" />
            {{/if}}
            <form
              novalidate
              data-test-backup-form
              {{on "submit" (preventDefault this.save)}}
              {{on "input" this.touch}}
            >
              <fieldset class="role-fieldset" disabled={{this.readOnly}}>
                <div class="form-group">
                  <div>
                    <h3 class="form-group-title">対象の volume</h3>
                    <p class="form-group-description">
                      named volume は、外したもの以外すべて取ります。稼働中の DB
                      のデータディレクトリはコピーしても整合しないので、対象から外してダンプフックで取ります。
                    </p>
                  </div>
                  <div class="form-group-body">
                    {{#if this.volumesLoading}}
                      <div
                        class="volume-loading"
                        role="status"
                        aria-label="volume の一覧を読み込み中"
                        data-test-volumes-loading
                      >
                        {{#each SKELETON_ROWS as |n|}}
                          <span
                            class="skeleton"
                            aria-hidden="true"
                            data-skeleton={{n}}
                          ></span>
                        {{/each}}
                      </div>
                    {{else}}
                      {{#if this.volumesFailed}}
                        <Banner
                          class="volume-list-error"
                          @tone="warning"
                          @title="volume の一覧を取得できませんでした"
                          data-test-volumes-error
                        >
                          <:default>
                            <span>{{this.volumeList.errorMessage}}</span>
                            <span class="banner-detail">
                              スナップショットのある volume と、対象から外した
                              volume だけを表示しています。
                            </span>
                          </:default>
                          <:actions>
                            <Button
                              @size="small"
                              @busy={{this.volumeList.loading}}
                              data-test-volumes-retry
                              {{on "click" this.volumeList.reload}}
                            >再読み込み</Button>
                          </:actions>
                        </Banner>
                      {{/if}}
                      {{#if this.volumes.length}}
                        <fieldset class="option-group" data-test-volumes>
                          <legend>バックアップする volume</legend>
                          {{#each this.volumes key="name" as |volume|}}
                            <label
                              class="option"
                              data-test-volume={{volume.name}}
                            >
                              <input
                                type="checkbox"
                                checked={{volume.included}}
                                data-test-volume-include
                                {{on
                                  "change"
                                  (fn this.toggleVolume volume.name)
                                }}
                              />
                              <span class="option-body">
                                <span
                                  class="option-label"
                                >{{volume.name}}</span>
                                <span class="option-hint" data-test-volume-hint>
                                  {{#if volume.missing}}
                                    {{#if volume.included}}
                                      この名前の volume
                                      は、いまはありません。作られたらバックアップします
                                    {{else}}
                                      バックアップしません（この名前の volume
                                      は、いまはありません）
                                    {{/if}}
                                  {{else if volume.included}}
                                    {{#if volume.latest}}
                                      最終取得
                                      <RelativeTime @time={{volume.latest}} />
                                    {{else}}
                                      次回のバックアップから取ります
                                    {{/if}}
                                  {{else}}
                                    バックアップしません
                                  {{/if}}
                                </span>
                              </span>
                            </label>
                          {{/each}}
                        </fieldset>
                      {{else if this.volumesListed}}
                        <p class="muted" data-test-no-volumes>
                          このアプリには named volume がありません。Compose
                          ファイルに named volume
                          があれば、デプロイのあと、ここに並びます。
                        </p>
                      {{else}}
                        <p class="muted" data-test-no-volumes>
                          表示できる volume がありません。
                        </p>
                      {{/if}}
                    {{/if}}

                    <FormField
                      @label="一覧にない volume を名前で外す"
                      @hint="まだ作られていない volume を、先に対象から外しておけます。Compose ファイルに書いた volume 名を入力します。"
                      @error={{this.errors.backup_exclude_volumes}}
                      data-test-field-group="backup_exclude_volumes"
                      as |field|
                    >
                      <div class="inline-add">
                        <input
                          id={{field.id}}
                          type="text"
                          spellcheck="false"
                          autocomplete="off"
                          placeholder="pgdata"
                          value={{this.volumeName}}
                          aria-describedby={{field.describedBy}}
                          aria-invalid={{field.invalid}}
                          data-test-exclude-name
                          {{on "input" this.setVolumeName}}
                          {{on "keydown" this.excludeOnEnter}}
                        />
                        <Button
                          data-test-exclude-add
                          {{on "click" this.excludeTyped}}
                        >対象から外す</Button>
                      </div>
                    </FormField>
                  </div>
                </div>

                <div class="form-group">
                  <div>
                    <h3 class="form-group-title">ダンプフック</h3>
                    <p class="form-group-description">
                      バックアップのたびに、指定したサービスのコンテナでコマンドを実行し、標準出力をダンプとして保存します。staging
                      アプリへの DB
                      のコピーでも、コピー元としてこのコマンドを使います。使わないときは両方を空にします。
                    </p>
                  </div>
                  <div class="form-group-body">
                    <FormField @label="種類から入力" as |field|>
                      <select
                        id={{field.id}}
                        data-test-dump-preset
                        {{on "change" this.applyDumpPreset}}
                      >
                        <option value="">選択すると下の欄を埋めます</option>
                        {{#each HOOK_PRESETS key="id" as |preset|}}
                          <option value={{preset.id}}>{{preset.label}}</option>
                        {{/each}}
                      </select>
                    </FormField>
                    <div class="form-grid">
                      <FormField
                        @label="サービス"
                        @error={{this.errors.dump_hook_service}}
                        data-test-field-group="dump_hook_service"
                        as |field|
                      >
                        <input
                          id={{field.id}}
                          type="text"
                          spellcheck="false"
                          autocomplete="off"
                          placeholder="db"
                          value={{this.form.dumpHookService}}
                          aria-describedby={{field.describedBy}}
                          aria-invalid={{field.invalid}}
                          data-test-field="dump_hook_service"
                          {{on "input" (setValue this.form "dumpHookService")}}
                        />
                      </FormField>
                      <FormField
                        class="field-wide"
                        @label="コマンド"
                        @error={{this.errors.dump_hook_command}}
                        data-test-field-group="dump_hook_command"
                        as |field|
                      >
                        <input
                          id={{field.id}}
                          type="text"
                          class="mono"
                          spellcheck="false"
                          autocomplete="off"
                          placeholder="pg_dump -U postgres app"
                          value={{this.form.dumpHookCommand}}
                          aria-describedby={{field.describedBy}}
                          aria-invalid={{field.invalid}}
                          data-test-field="dump_hook_command"
                          {{on "input" (setValue this.form "dumpHookCommand")}}
                        />
                      </FormField>
                    </div>
                  </div>
                </div>

                <div class="form-group">
                  <div>
                    <h3 class="form-group-title">デプロイの前</h3>
                    <p class="form-group-description">
                      コンテナを入れ替える直前に、このアプリだけのバックアップを取ります。失敗したらデプロイを中止します。
                    </p>
                  </div>
                  <div class="form-group-body">
                    <label class="option">
                      <input
                        type="checkbox"
                        checked={{this.form.backupBeforeDeploy}}
                        data-test-field="backup_before_deploy"
                        {{on
                          "change"
                          (setChecked this.form "backupBeforeDeploy")
                        }}
                      />
                      <span class="option-body">
                        <span class="option-label">
                          デプロイの前にバックアップを取る
                        </span>
                      </span>
                    </label>
                  </div>
                </div>

                <div aria-live="polite">
                  <ErrorMessage @error={{this.saveError}} />
                  {{#if this.otherErrors.length}}
                    <ul class="form-problems" data-test-other-errors>
                      {{#each this.otherErrors as |line|}}
                        <li>{{line}}</li>
                      {{/each}}
                    </ul>
                  {{/if}}
                </div>

                <div class="form-footer">
                  <Button
                    @type="submit"
                    @variant="primary"
                    @busy={{this.saving}}
                    @busyLabel="保存中…"
                    data-test-backup-save
                  >保存</Button>
                  <p class="form-status" aria-live="polite">
                    {{#if this.saved}}
                      <span class="form-saved" data-test-backup-saved>
                        <Icon @name="check" />
                        保存しました。次回のバックアップから反映されます。
                      </span>
                    {{else if this.nothingToSave}}
                      <span data-test-backup-unchanged>変更はありません。</span>
                    {{else}}
                      次回のバックアップから反映されます。
                    {{/if}}
                  </p>
                </div>
              </fieldset>
            </form>
          {{else}}
            <p class="muted" data-test-backup-settings-unavailable>
              バックアップの設定は、このサーバーではまだ利用できません。
            </p>
          {{/if}}
        </section>
      {{else}}
        <div class="box unconfigured" data-test-backups-unconfigured>
          <p class="empty-state-title">バックアップは設定されていません</p>
          <p class="empty-state-description">
            このアプリのデータは、どこにも退避されていません。サーバーに次の 2
            つを置いて koroku を再起動すると、定期的に取得されます。
          </p>
          <ul class="unconfigured-list">
            <li>
              <code>/etc/koroku/config.toml</code>
              の
              <code>[storage]</code>
              （オブジェクトストレージの接続情報）
            </li>
            <li>
              <code>/etc/koroku/restic.key</code>
              （restic のリポジトリパスワード）
            </li>
          </ul>
          <div class="empty-state-action">
            <Button
              @busy={{this.overview.loading}}
              data-test-backups-recheck
              {{on "click" this.reload}}
            >もう一度確認</Button>
          </div>
        </div>
      {{/if}}

      {{#unless @model.isProduction}}
        <DbCopyPanel
          @model={{@model}}
          @backupsConfigured={{overview.configured}}
        />
      {{/unless}}
    </AsyncView>

    {{#if this.restoring}}
      <ConfirmDialog
        @title="volume をリストアしますか？"
        @confirmLabel="リストアする"
        @danger={{true}}
        @requireText={{this.slug}}
        @busy={{this.restoreBusy}}
        @onConfirm={{this.confirmRestore}}
        @onCancel={{this.cancelRestore}}
      >
        <p>
          volume
          <strong>{{this.restoring.target}}</strong>
          を、{{formatDateTimeJst this.restoring.time}}
          に取得したスナップショット
          <code>{{this.restoring.id}}</code>
          の内容に戻します。
        </p>
        <ul>
          <li>
            リストアの間、{{if @model.isProduction "本番アプリ" "アプリ"}}
            <strong>{{this.slug}}</strong>
            は<strong>停止</strong>し、アクセスには 503 のページが返ります。
          </li>
          <li>
            いまの volume の中身は消さずにサーバー上へ<strong>退避</strong>し、7
            日間残します。
          </li>
          <li>
            終わると、稼働中だったアプリは起動します。完了までこの画面で待ちます。
          </li>
        </ul>
      </ConfirmDialog>
    {{/if}}
  </template>
}
