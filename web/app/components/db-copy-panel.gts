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
  copyPlan,
  findPreset,
  HOOK_PRESETS,
  sourceChoices,
  targetProblem,
  liveProblem,
  type PlanStep,
  type SourceChoice,
} from 'koroku-console/utils/db-copy';
import { inputValue, preventDefault, setValue } from 'koroku-console/utils/dom';
import { fieldErrors } from 'koroku-console/utils/errors';
import { formatBytes, formatDuration } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import type { StatusKind } from 'koroku-console/utils/status';
import type { App, CopySourceKind, DbCopy } from 'koroku-console/utils/types';
import Banner from './banner';
import Button from './button';
import ConfirmDialog from './confirm-dialog';
import DbCopyDetail from './db-copy-detail';
import EmptyState from './empty-state';
import ErrorMessage from './error-message';
import FormField from './form-field';
import Icon from './icon';
import RelativeTime from './relative-time';
import RowList from './row-list';
import StatusIndicator from './status-indicator';

/** A running copy is followed closely. */
const RUNNING_INTERVAL = 3000;

const COPY_STATUS: Record<
  DbCopy['status'],
  { kind: StatusKind; label: string }
> = {
  running: { kind: 'deploying', label: 'コピー中' },
  succeeded: { kind: 'running', label: '完了' },
  failed: { kind: 'failed', label: '失敗' },
};

function statusKind(copy: DbCopy): StatusKind {
  return COPY_STATUS[copy.status].kind;
}

function statusLabel(copy: DbCopy): string {
  return COPY_STATUS[copy.status].label;
}

function kindLabel(copy: DbCopy): string {
  return copy.source_kind === 'snapshot' ? 'バックアップから' : '稼働中から';
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

function choiceLabel(choice: SourceChoice): string {
  const env = choice.app.environment === 'production' ? '本番' : 'staging';
  return `${choice.app.slug}（${env}${choice.sameRepo ? '・同じリポジトリ' : ''}）`;
}

interface Signature {
  Args: {
    model: AppContext;
    /** Whether backups are configured on this host. */
    backupsConfigured: boolean;
  };
}

/**
 * Copying another app's database into this staging app (ARCHITECTURE
 * 10.5): the source, a preview of the steps, the typed confirmation, the
 * progress and history, and the load hook settings.
 */
export default class DbCopyPanel extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  apps = new Loadable<App[]>(() => this.api.listApps());
  copies = new Loadable<DbCopy[]>(() => this.api.listDbCopies(this.slug));

  @tracked chosenSource = '';
  @tracked kind: CopySourceKind = 'live';
  @tracked confirming = false;
  @tracked starting = false;
  @tracked startError: unknown = null;
  /** The copy whose log is open; the newest by default. */
  @tracked openId: number | null = null;

  @tracked form: CopySettingsForm;
  @tracked saving = false;
  @tracked saved = false;
  @tracked saveError: unknown = null;
  @tracked errors: Record<string, string> = {};

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.form = new CopySettingsForm(this.app);
    // A backend without copies has nothing to list.
    if (!this.form.available) return;
    void this.apps.load();
    void this.copies.load();
    this.poller.every(this, RUNNING_INTERVAL, () => this.follow());
  }

  get slug(): string {
    return this.args.model.slug;
  }

  get app(): App {
    return this.args.model.app;
  }

  get choices(): SourceChoice[] {
    return this.apps.value ? sourceChoices(this.apps.value, this.app) : [];
  }

  get source(): SourceChoice | undefined {
    return (
      this.choices.find((choice) => choice.app.slug === this.chosenSource) ??
      this.choices[0]
    );
  }

  get sourceSlug(): string {
    return this.source?.app.slug ?? '';
  }

  get targetProblem(): string {
    return targetProblem(this.app);
  }

  get sourceProblem(): string {
    if (!this.source) return '';
    if (this.kind === 'snapshot') return '';
    return liveProblem(this.source.app);
  }

  get running(): DbCopy | undefined {
    return this.copies.value?.find((copy) => copy.status === 'running');
  }

  get canStart(): boolean {
    return (
      Boolean(this.source) &&
      !this.targetProblem &&
      !this.sourceProblem &&
      !this.running &&
      !this.starting
    );
  }

  get plan(): PlanStep[] {
    if (!this.source) return [];
    return copyPlan({
      source: this.source.app,
      target: this.app,
      kind: this.kind,
      backupsConfigured: this.args.backupsConfigured,
    });
  }

  get openCopy(): DbCopy | undefined {
    const list = this.copies.value ?? [];
    return list.find((copy) => copy.id === this.openId) ?? list[0];
  }

  get noCopies(): boolean {
    return this.copies.loaded && (this.copies.value?.length ?? 0) === 0;
  }

  /** Follows a running copy until it ends. */
  private async follow(): Promise<void> {
    if (!this.running) return;
    await this.copies.load({ quiet: true });
    // The copy stopped and started the app's services.
    if (!this.running) await this.args.model.reload();
  }

  chooseSource = (event: Event) => {
    this.chosenSource = inputValue(event);
  };

  chooseKind = (event: Event) => {
    this.kind = inputValue(event) as CopySourceKind;
  };

  openLog = (copy: DbCopy) => {
    this.openId = copy.id;
  };

  isOpen = (copy: DbCopy): boolean => this.openCopy?.id === copy.id;

  requestStart = () => {
    this.startError = null;
    this.confirming = true;
  };

  cancelStart = () => {
    this.confirming = false;
  };

  confirmStart = async () => {
    if (!this.source) return;
    this.starting = true;
    try {
      const copy = await this.api.startDbCopy(this.slug, {
        source: this.source.app.slug,
        source_kind: this.kind,
        confirm: this.slug,
      });
      this.copies.set([copy, ...(this.copies.value ?? [])]);
      this.openId = copy.id;
    } catch (error) {
      this.startError = error;
    } finally {
      this.starting = false;
      this.confirming = false;
    }
  };

  /** The log ended: show the final status. */
  copyEnded = async () => {
    await this.copies.load({ quiet: true });
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
    <section
      class="pane db-copy"
      aria-labelledby="db-copy-title"
      data-test-db-copy
    >
      <div class="pane-header">
        <h2 class="pane-title" id="db-copy-title">本番のデータをコピー</h2>
      </div>
      <p class="backup-note">
        ほかのアプリ（ふつうは同じリポジトリの本番アプリ）の DB
        を、このアプリへ流し込みます。このアプリのいまのデータは置き換わります。DB
        のメジャーバージョンは揃えてください。
      </p>

      {{#if this.form.available}}
        <div class="box copy-start" data-test-copy-start>
          {{#if this.apps.loaded}}
            {{#if this.choices.length}}
              <div class="form-grid">
                <FormField @label="コピー元" as |field|>
                  <select
                    id={{field.id}}
                    data-test-copy-source
                    {{on "change" this.chooseSource}}
                  >
                    {{#each this.choices key="app.slug" as |choice|}}
                      <option
                        value={{choice.app.slug}}
                        selected={{eq choice.app.slug this.sourceSlug}}
                      >{{choiceLabel choice}}</option>
                    {{/each}}
                  </select>
                </FormField>
                <fieldset class="option-group copy-kind">
                  <legend>取り出し方</legend>
                  <label class="option">
                    <input
                      type="radio"
                      name="copy-kind"
                      value="live"
                      checked={{eq this.kind "live"}}
                      data-test-copy-kind="live"
                      {{on "change" this.chooseKind}}
                    />
                    <span class="option-body">
                      <span class="option-label">稼働中のアプリから</span>
                      <span
                        class="option-hint"
                      >最新のデータ。コピー元にダンプの負荷がかかります。</span>
                    </span>
                  </label>
                  <label class="option">
                    <input
                      type="radio"
                      name="copy-kind"
                      value="snapshot"
                      checked={{eq this.kind "snapshot"}}
                      disabled={{if @backupsConfigured false true}}
                      data-test-copy-kind="snapshot"
                      {{on "change" this.chooseKind}}
                    />
                    <span class="option-body">
                      <span class="option-label">最新のバックアップから</span>
                      <span class="option-hint">
                        {{#if @backupsConfigured}}
                          コピー元に負荷をかけません。データは最後のバックアップの時点のものです。
                        {{else}}
                          バックアップが設定されていないため選べません。
                        {{/if}}
                      </span>
                    </span>
                  </label>
                </fieldset>
              </div>

              <ol
                class="copy-plan"
                aria-label="コピーで行うこと"
                data-test-copy-plan
              >
                {{#each this.plan as |step|}}
                  <li>
                    <span class="copy-plan-title">{{step.title}}</span>
                    <span class="copy-plan-detail">
                      {{step.detail}}
                      {{#if step.command}}
                        <code class="copy-plan-command">{{step.command}}</code>
                      {{/if}}
                    </span>
                  </li>
                {{/each}}
              </ol>

              <div aria-live="polite">
                {{#if this.targetProblem}}
                  <Banner @tone="warning" data-test-copy-blocked>
                    {{this.targetProblem}}
                  </Banner>
                {{else if this.sourceProblem}}
                  <Banner @tone="warning" data-test-copy-blocked>
                    {{this.sourceProblem}}
                  </Banner>
                {{/if}}
                <ErrorMessage @error={{this.startError}} />
              </div>

              <div class="form-footer">
                <Button
                  @variant="danger"
                  @busy={{this.starting}}
                  @busyLabel="開始中…"
                  @disabled={{if this.canStart false true}}
                  data-test-copy-run
                  {{on "click" this.requestStart}}
                >コピーを開始</Button>
                <p class="form-status">
                  {{#if this.running}}
                    コピーの実行中です。終わるまで次のコピーは始められません。
                  {{else}}
                    開始の前に、このアプリの名前の入力を求めます。
                  {{/if}}
                </p>
              </div>
            {{else}}
              <p class="muted" data-test-copy-no-sources>
                コピー元にできるアプリがありません。
              </p>
            {{/if}}
          {{else if this.apps.error}}
            <ErrorMessage @error={{this.apps.error}} />
          {{else}}
            <p class="muted" role="status">
              <Icon @name="spinner" />
              アプリを読み込み中…
            </p>
          {{/if}}
        </div>

        <h3 class="subsection-title">履歴</h3>
        {{#if this.noCopies}}
          <div class="box" data-test-copy-empty>
            <EmptyState
              @title="コピーはまだありません"
              @description="コピーを始めると、進み具合とログがここに並びます。"
            />
          </div>
        {{else if this.copies.value}}
          <RowList class="copy-rows" @label="コピーの履歴">
            <:header>
              <span>状態</span>
              <span>コピー元</span>
              <span>取り出し方</span>
              <span>サイズ</span>
              <span>開始</span>
              <span>所要時間</span>
            </:header>
            <:rows as |Row|>
              {{#each this.copies.value key="id" as |copy|}}
                <Row
                  class={{if (this.isOpen copy) "is-selected"}}
                  data-test-copy={{copy.id}}
                >
                  <div class="copy-status">
                    <StatusIndicator
                      @kind={{statusKind copy}}
                      @label={{statusLabel copy}}
                      @quiet={{eq copy.status "succeeded"}}
                    />
                  </div>
                  <div class="copy-source">
                    <span class="cell-label">コピー元</span>
                    <span class="mono">{{copy.source_app}}</span>
                  </div>
                  <div class="copy-kind-cell">
                    <span class="cell-label">取り出し方</span>
                    <span>{{kindLabel copy}}</span>
                  </div>
                  <div class="copy-size">
                    <span class="cell-label">サイズ</span>
                    <span>{{size copy}}</span>
                  </div>
                  <div class="copy-time">
                    <span class="cell-label">開始</span>
                    <RelativeTime @time={{copy.started_at}} />
                  </div>
                  <div class="copy-duration">
                    <span class="cell-label">所要時間</span>
                    <span>{{duration copy}}</span>
                  </div>
                  <div class="copy-action">
                    <Button
                      @size="small"
                      aria-label="コピー #{{copy.id}} のログを表示"
                      aria-pressed={{if (this.isOpen copy) "true" "false"}}
                      data-test-copy-open
                      {{on "click" (fn this.openLog copy)}}
                    >ログ</Button>
                  </div>
                </Row>
              {{/each}}
            </:rows>
          </RowList>
          {{#if this.openCopy}}
            {{! A new detail per copy, so that its log stream starts over. }}
            {{#each (single this.openCopy) key="id" as |shown|}}
              <DbCopyDetail @copy={{shown}} @onLogEnd={{this.copyEnded}} />
            {{/each}}
          {{/if}}
        {{else if this.copies.error}}
          <ErrorMessage @error={{this.copies.error}} />
        {{/if}}

        <h3 class="subsection-title" id="copy-settings-title">コピーの設定</h3>
        <form
          class="box copy-settings"
          novalidate
          aria-labelledby="copy-settings-title"
          data-test-copy-settings
          {{on "submit" (preventDefault this.save)}}
          {{on "input" this.touch}}
        >
          <div class="form-group">
            <div>
              <h4 class="form-group-title">ロードフック</h4>
              <p class="form-group-description">
                指定したサービスのコンテナでコマンドを実行し、標準入力からダンプを読ませます。コピー元のダンプフックと同じ種類の
                DB にしてください。
              </p>
            </div>
            <div class="form-group-body">
              <FormField @label="種類から入力" as |field|>
                <select
                  id={{field.id}}
                  data-test-load-preset
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

          <div class="form-group">
            <div>
              <h4 class="form-group-title">コピー後のコマンド</h4>
              <p class="form-group-description">
                流し込んだあと、止めたサービスを起動する前に実行します。個人情報を消すのに使います。止めたサービスでは
                1 回限りのコンテナで実行します。使わないときは両方を空にします。
              </p>
            </div>
            <div class="form-group-body">
              <div class="form-grid">
                <FormField
                  @label="サービス"
                  @error={{this.errors.post_load_service}}
                  data-test-field-group="post_load_service"
                  as |field|
                >
                  <input
                    id={{field.id}}
                    type="text"
                    spellcheck="false"
                    autocomplete="off"
                    placeholder="web"
                    value={{this.form.postLoadService}}
                    aria-describedby={{field.describedBy}}
                    aria-invalid={{field.invalid}}
                    data-test-field="post_load_service"
                    {{on "input" (setValue this.form "postLoadService")}}
                  />
                </FormField>
                <FormField
                  class="field-wide"
                  @label="コマンド"
                  @error={{this.errors.post_load_command}}
                  data-test-field-group="post_load_command"
                  as |field|
                >
                  <input
                    id={{field.id}}
                    type="text"
                    class="mono"
                    spellcheck="false"
                    autocomplete="off"
                    placeholder="bin/rails db:anonymize"
                    value={{this.form.postLoadCommand}}
                    aria-describedby={{field.describedBy}}
                    aria-invalid={{field.invalid}}
                    data-test-field="post_load_command"
                    {{on "input" (setValue this.form "postLoadCommand")}}
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
              data-test-copy-settings-save
            >保存</Button>
            <p class="form-status" aria-live="polite">
              {{#if this.saved}}
                <span class="form-saved" data-test-copy-settings-saved>
                  <Icon @name="check" />
                  保存しました。
                </span>
              {{/if}}
            </p>
          </div>
        </form>
      {{else}}
        <p class="muted" data-test-copy-unavailable>
          DB のコピーは、このサーバーではまだ利用できません。
        </p>
      {{/if}}
    </section>

    {{#if this.confirming}}
      <ConfirmDialog
        @title="データをコピーしますか？"
        @confirmLabel="コピーする"
        @danger={{true}}
        @requireText={{this.slug}}
        @busy={{this.starting}}
        @onConfirm={{this.confirmStart}}
        @onCancel={{this.cancelStart}}
      >
        <p>
          <strong>{{this.sourceSlug}}</strong>
          の DB を
          <strong>{{this.slug}}</strong>
          へ流し込みます。{{this.slug}}
          のいまのデータは<strong>置き換わります</strong>。
        </p>
        <ul>
          {{#each this.plan as |step|}}
            <li>{{step.detail}}</li>
          {{/each}}
        </ul>
        <p>コピー元の個人情報を含むデータが staging
          に入ります。公開範囲に注意してください。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
