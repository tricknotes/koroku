import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn } from '@ember/helper';
import type Owner from '@ember/owner';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type AppContext from 'koroku-console/utils/app-context';
import {
  CopySettingsForm,
  findPreset,
  HOOK_PRESETS,
  isRestore,
  restoreProblem,
} from 'koroku-console/utils/db-copy';
import { inputValue, preventDefault, setValue } from 'koroku-console/utils/dom';
import { fieldErrors } from 'koroku-console/utils/errors';
import {
  formatBytes,
  formatDateTimeJst,
  formatDuration,
} from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import { can } from 'koroku-console/utils/roles';
import type { StatusKind } from 'koroku-console/utils/status';
import type { App, DbCopy, Snapshot } from 'koroku-console/utils/types';
import Button from './button';
import ConfirmDialog from './confirm-dialog';
import DbCopyDetail from './db-copy-detail';
import EmptyState from './empty-state';
import ErrorMessage from './error-message';
import FormField from './form-field';
import Icon from './icon';
import RelativeTime from './relative-time';
import RoleNote from './role-note';
import RowList from './row-list';
import StatusIndicator from './status-indicator';

/** A running restore is followed closely. */
const RUNNING_INTERVAL = 3000;

const RESTORE_STATUS: Record<
  DbCopy['status'],
  { kind: StatusKind; label: string }
> = {
  running: { kind: 'deploying', label: '戻しています' },
  succeeded: { kind: 'running', label: '完了' },
  failed: { kind: 'failed', label: '失敗' },
};

function statusKind(copy: DbCopy): StatusKind {
  return RESTORE_STATUS[copy.status].kind;
}

function statusLabel(copy: DbCopy): string {
  return RESTORE_STATUS[copy.status].label;
}

function shortId(id: string | undefined): string {
  return (id ?? '').slice(0, 8);
}

function size(copy: DbCopy): string {
  return copy.bytes > 0 ? formatBytes(copy.bytes) : '—';
}

function duration(copy: DbCopy): string {
  return formatDuration(copy.started_at, copy.finished_at) || '—';
}

/** `{{#each (single x) key="id"}}` re-creates its block when x changes. */
function single<T>(item: T): T[] {
  return [item];
}

interface Signature {
  Args: {
    model: AppContext;
    /** The dump snapshot the user chose to restore; null when none. */
    requested: Snapshot | null;
    /** The dialog for `requested` was closed. */
    onClose: () => unknown;
  };
}

/**
 * Restoring the app's own dump snapshots (ARCHITECTURE 10.8): what it
 * needs, the confirmation for the dump chosen in the snapshot list, and the
 * history with the log of each restore.
 */
export default class DumpRestorePanel extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  restores = new Loadable<DbCopy[]>(async () =>
    (await this.api.listDbCopies(this.slug)).filter(isRestore),
  );

  @tracked starting = false;
  @tracked startError: unknown = null;
  /** The restore whose log is open; the newest by default. */
  @tracked openId: number | null = null;

  /** The load hook, edited here on production apps only (see below). */
  @tracked form: CopySettingsForm;
  @tracked saving = false;
  @tracked saved = false;
  @tracked saveError: unknown = null;
  @tracked errors: Record<string, string> = {};

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.form = new CopySettingsForm(this.app);
    if (!this.form.available) return;
    void this.restores.load();
    this.poller.every(this, RUNNING_INTERVAL, () => this.follow());
  }

  get slug(): string {
    return this.args.model.slug;
  }

  get app(): App {
    return this.args.model.app;
  }

  get readOnly(): boolean {
    return !can(this.app, 'manage');
  }

  get isProduction(): boolean {
    return this.args.model.isProduction;
  }

  /** What keeps the app from being restored; '' when nothing does. */
  get problem(): string {
    return restoreProblem(this.app);
  }

  /** Where a staging app sets its load hook. */
  get loadHookHint(): string {
    if (this.isProduction) return '';
    if (this.app.load_hook_service && this.app.load_hook_command) return '';
    return 'ロードフックは、下の「本番のデータをコピー」のコピーの設定で設定します。';
  }

  /**
   * The load hook is set in the copy settings of a staging app. A
   * production app has no copy settings, so it is set here.
   */
  get editsLoadHook(): boolean {
    return this.isProduction && !this.readOnly;
  }

  get running(): DbCopy | undefined {
    return this.restores.value?.find((copy) => copy.status === 'running');
  }

  get openRestore(): DbCopy | undefined {
    const list = this.restores.value ?? [];
    return list.find((copy) => copy.id === this.openId) ?? list[0];
  }

  get noRestores(): boolean {
    return this.restores.loaded && (this.restores.value?.length ?? 0) === 0;
  }

  /** The services a restore stops: all but the load hook's. */
  get loadService(): string {
    return this.app.load_hook_service ?? '';
  }

  private async follow(): Promise<void> {
    if (!this.running) return;
    await this.restores.load({ quiet: true });
    // The restore stopped and started the app's services.
    if (!this.running) await this.args.model.reload();
  }

  openLog = (copy: DbCopy) => {
    this.openId = copy.id;
  };

  isOpen = (copy: DbCopy): boolean => this.openRestore?.id === copy.id;

  confirm = async () => {
    const snapshot = this.args.requested;
    if (!snapshot) return;
    this.starting = true;
    this.startError = null;
    try {
      const copy = await this.api.startDbRestore(this.slug, {
        snapshot_id: snapshot.id,
        confirm: this.slug,
      });
      this.restores.set([copy, ...(this.restores.value ?? [])]);
      this.openId = copy.id;
    } catch (error) {
      this.startError = error;
    } finally {
      this.starting = false;
      this.args.onClose();
    }
  };

  /** The log ended: show the final status. */
  restoreEnded = async () => {
    await this.restores.load({ quiet: true });
    await this.args.model.reload();
  };

  applyPreset = (event: Event) => {
    const preset = findPreset(inputValue(event));
    if (preset) this.form.applyPreset(preset);
    (event.target as HTMLSelectElement).value = '';
    this.saved = false;
  };

  touch = () => {
    this.saved = false;
  };

  save = async () => {
    this.saved = false;
    this.saveError = null;
    this.errors = this.form.validate();
    if (Object.keys(this.errors).length > 0) return;
    const patch = this.form.patch(this.app);
    if (Object.keys(patch).length === 0) {
      this.saved = true;
      return;
    }
    this.saving = true;
    try {
      const app = await this.api.updateApp(this.slug, patch);
      this.args.model.set(app);
      this.form = new CopySettingsForm(app);
      this.saved = true;
    } catch (error) {
      this.saveError = error;
      this.errors = fieldErrors(error);
    } finally {
      this.saving = false;
    }
  };

  <template>
    {{#if this.form.available}}
      <section
        class="pane dump-restore"
        aria-labelledby="dump-restore-title"
        data-test-dump-restore
      >
        <div class="pane-header">
          <h2 class="pane-title" id="dump-restore-title">ダンプから戻す</h2>
        </div>
        <p class="backup-note">
          スナップショットの一覧のダンプの行にある「この時点に戻す」で、DB
          をそのダンプの状態に戻します。戻す前に、いまの DB
          のダンプを取って残します。
        </p>

        <div aria-live="polite">
          <ErrorMessage @error={{this.startError}} />
        </div>

        {{#if this.problem}}
          <p class="muted" data-test-restore-problem>
            {{this.problem}}
            {{this.loadHookHint}}
          </p>
        {{/if}}

        {{#if this.editsLoadHook}}
          <form
            class="box restore-settings"
            novalidate
            aria-labelledby="restore-settings-title"
            data-test-restore-settings
            {{on "submit" (preventDefault this.save)}}
            {{on "input" this.touch}}
          >
            <div class="form-group">
              <div>
                <h3 class="form-group-title" id="restore-settings-title">
                  ロードフック
                </h3>
                <p class="form-group-description">
                  指定したサービスのコンテナでコマンドを実行し、標準入力からダンプを読ませます。ダンプフックと同じ種類の
                  DB にしてください。
                </p>
              </div>
              <div class="form-group-body">
                <FormField @label="種類から入力" as |field|>
                  <select
                    id={{field.id}}
                    data-test-restore-load-preset
                    {{on "change" this.applyPreset}}
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
                    @error={{this.errors.load_hook_service}}
                    data-test-field-group="load_hook_service"
                    as |field|
                  >
                    <input
                      id={{field.id}}
                      type="text"
                      spellcheck="false"
                      autocomplete="off"
                      placeholder="db"
                      value={{this.form.loadHookService}}
                      aria-describedby={{field.describedBy}}
                      aria-invalid={{field.invalid}}
                      data-test-field="load_hook_service"
                      {{on "input" (setValue this.form "loadHookService")}}
                    />
                  </FormField>
                  <FormField
                    class="field-wide"
                    @label="コマンド"
                    @error={{this.errors.load_hook_command}}
                    data-test-field-group="load_hook_command"
                    as |field|
                  >
                    <input
                      id={{field.id}}
                      type="text"
                      class="mono"
                      spellcheck="false"
                      autocomplete="off"
                      placeholder="pg_restore --clean --if-exists --no-owner -d app"
                      value={{this.form.loadHookCommand}}
                      aria-describedby={{field.describedBy}}
                      aria-invalid={{field.invalid}}
                      data-test-field="load_hook_command"
                      {{on "input" (setValue this.form "loadHookCommand")}}
                    />
                  </FormField>
                </div>
              </div>
            </div>
            <div aria-live="polite">
              <ErrorMessage @error={{this.saveError}} />
            </div>
            <div class="form-footer">
              <Button
                @type="submit"
                @variant="primary"
                @busy={{this.saving}}
                @busyLabel="保存中…"
                data-test-restore-settings-save
              >保存</Button>
              <p class="form-status" aria-live="polite">
                {{#if this.saved}}
                  <span class="form-saved" data-test-restore-settings-saved>
                    <Icon @name="check" />
                    保存しました。
                  </span>
                {{/if}}
              </p>
            </div>
          </form>
        {{else if this.isProduction}}
          {{#if this.readOnly}}
            <RoleNote @needed="manage" @what="ダンプから戻す操作と設定" />
          {{/if}}
        {{/if}}

        <h3 class="subsection-title">履歴</h3>
        {{#if this.noRestores}}
          <div class="box" data-test-restore-empty>
            <EmptyState
              @title="ダンプから戻したことはまだありません"
              @description="戻すと、進み具合とログがここに並びます。"
            />
          </div>
        {{else if this.restores.value}}
          <RowList class="restore-rows" @label="ダンプから戻した履歴">
            <:header>
              <span>状態</span>
              <span>戻した先</span>
              <span>サイズ</span>
              <span>開始</span>
              <span>所要時間</span>
            </:header>
            <:rows as |Row|>
              {{#each this.restores.value key="id" as |copy|}}
                <Row
                  class={{if (this.isOpen copy) "is-selected"}}
                  data-test-restore={{copy.id}}
                >
                  <div class="restore-status">
                    <StatusIndicator
                      @kind={{statusKind copy}}
                      @label={{statusLabel copy}}
                      @quiet={{eq copy.status "succeeded"}}
                    />
                  </div>
                  <div class="restore-source">
                    <span class="cell-label">戻した先</span>
                    <span>ダンプ
                      <code>{{shortId copy.source_snapshot_id}}</code></span>
                    {{#if copy.actor}}
                      <span
                        class="deploy-actor restore-actor"
                        title="戻した人"
                        data-test-restore-actor
                      >実行: {{copy.actor.login}}</span>
                    {{/if}}
                  </div>
                  <div class="restore-size">
                    <span class="cell-label">サイズ</span>
                    <span>{{size copy}}</span>
                  </div>
                  <div class="restore-time">
                    <span class="cell-label">開始</span>
                    <RelativeTime @time={{copy.started_at}} />
                  </div>
                  <div class="restore-duration">
                    <span class="cell-label">所要時間</span>
                    <span>{{duration copy}}</span>
                  </div>
                  <div class="restore-action">
                    <Button
                      @size="small"
                      aria-label="ダンプからの復旧 #{{copy.id}} のログを表示"
                      aria-pressed={{if (this.isOpen copy) "true" "false"}}
                      data-test-restore-open
                      {{on "click" (fn this.openLog copy)}}
                    >ログ</Button>
                  </div>
                </Row>
              {{/each}}
            </:rows>
          </RowList>
          {{#if this.openRestore}}
            {{! A new detail per restore, so that its log stream starts over. }}
            {{#each (single this.openRestore) key="id" as |shown|}}
              <DbCopyDetail @copy={{shown}} @onLogEnd={{this.restoreEnded}} />
            {{/each}}
          {{/if}}
        {{else if this.restores.error}}
          <ErrorMessage @error={{this.restores.error}} />
        {{/if}}
      </section>
    {{/if}}

    {{#if @requested}}
      <ConfirmDialog
        @title="DB をこのダンプの時点に戻しますか？"
        @confirmLabel="この時点に戻す"
        @danger={{true}}
        @requireText={{if this.isProduction this.slug}}
        @busy={{this.starting}}
        @onConfirm={{this.confirm}}
        @onCancel={{@onClose}}
      >
        <p data-test-restore-dialog-target>
          サービス
          <strong>{{@requested.target}}</strong>
          の DB を、{{formatDateTimeJst @requested.time}}
          に取ったダンプ
          <code>{{shortId @requested.id}}</code>
          の内容に戻します。
        </p>
        <ul>
          <li>
            戻す前に、いまの DB
            のダンプを取ってスナップショットとして残します。取れなければ、何も変えずに中止します。
          </li>
          <li>
            戻す間、{{if this.isProduction "本番アプリ" "アプリ"}}
            <strong>{{this.slug}}</strong>
            の
            <strong>{{this.loadService}}</strong>
            以外のサービスを止めます。アクセスには 503 のページが返ります。
          </li>
          <li>
            流し込みに失敗したら、取っておいたダンプを流し込み直してから起動します。
          </li>
        </ul>
      </ConfirmDialog>
    {{/if}}
  </template>
}
