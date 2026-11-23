import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { concat, fn } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import { modifier } from 'ember-modifier';
import AsyncView from 'koroku-console/components/async-view';
import Badge from 'koroku-console/components/badge';
import Banner from 'koroku-console/components/banner';
import Button, { buttonClass } from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import CopyButton from 'koroku-console/components/copy-button';
import EmptyState from 'koroku-console/components/empty-state';
import EnvRequirementsPanel from 'koroku-console/components/env-requirements-panel';
import EnvSourceSettings from 'koroku-console/components/env-source-settings';
import UnsavedSettingsGuard from 'koroku-console/components/unsaved-settings-guard';
import ErrorMessage from 'koroku-console/components/error-message';
import Icon from 'koroku-console/components/icon';
import RowList from 'koroku-console/components/row-list';
import TabSection from 'koroku-console/components/tab-section';
import type ApiService from 'koroku-console/services/api';
import type SessionService from 'koroku-console/services/session';
import type AppContext from 'koroku-console/utils/app-context';
import type AppEnvController from 'koroku-console/controllers/app/env';
import SettingsEditor, {
  ENV_SOURCE_FIELDS,
} from 'koroku-console/utils/settings-editor';
import { preventDefault, setValue } from 'koroku-console/utils/dom';
import {
  formatDotenv,
  parseDotenv,
  type DotenvResult,
} from 'koroku-console/utils/dotenv';
import { unlistedKeys } from 'koroku-console/utils/env-requirements';
import { errorMessage, fieldErrors } from 'koroku-console/utils/errors';
import { isValidEnvKey, shortSha } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import { can } from 'koroku-console/utils/roles';
import type {
  Deployment,
  EnvList,
  EnvPending,
  EnvRequirements,
  EnvVar,
} from 'koroku-console/utils/types';

const PENDING_LABELS: Record<EnvPending, string> = {
  added: '未反映（追加）',
  changed: '未反映（変更）',
};

/** What stands in for a hidden value: the same for every length. */
const MASK = '••••••••';

let nextRowId = 0;

/**
 * One variable on screen. `key` and `value` are what is saved; while the
 * row is being edited the drafts hold what is typed, and only "保存" sends
 * them. A fresh row is one being added: nothing of it is saved yet.
 */
class EnvRow {
  readonly id = `env-${nextRowId++}`;
  @tracked key: string;
  @tracked value: string;
  @tracked pending: EnvPending | null;
  @tracked fresh: boolean;
  @tracked editing: boolean;
  @tracked draftKey: string;
  @tracked draftValue: string;
  /** The value is shown as text. Until then it is not in the DOM at all. */
  @tracked revealed = false;
  /** Asking whether to delete, in place of the row's actions. */
  @tracked confirmingRemove = false;
  @tracked working: 'save' | 'remove' | null = null;
  @tracked error: unknown = null;
  /**
   * The field that takes the focus when the row turns into a form: the key
   * of a row added just now, the value of one that is being changed.
   */
  focus: 'key' | 'value' | null;

  constructor(saved: EnvVar | null, draftKey = '') {
    this.key = saved?.key ?? '';
    this.value = saved?.value ?? '';
    this.pending = saved?.pending ?? null;
    this.fresh = saved === null;
    this.editing = this.fresh;
    this.draftKey = draftKey || this.key;
    this.draftValue = this.value;
    // A key given up front (added from the repository's list) leaves only
    // the value to type.
    this.focus = this.fresh ? (draftKey ? 'value' : 'key') : null;
  }

  get focusKey(): boolean {
    return this.focus === 'key';
  }

  get focusValue(): boolean {
    return this.focus === 'value';
  }

  get trimmedKey(): string {
    return this.draftKey.trim();
  }

  /** The drafts differ from what is saved. */
  get changed(): boolean {
    if (this.fresh) return this.trimmedKey !== '';
    return this.trimmedKey !== this.key || this.draftValue !== this.value;
  }

  get valueRows(): number {
    return Math.min(8, this.draftValue.split('\n').length);
  }

  get pendingLabel(): string {
    return this.pending ? PENDING_LABELS[this.pending] : '';
  }

  get errorText(): string {
    return this.error ? errorMessage(this.error) : '';
  }

  get errorLines(): string[] {
    return Object.entries(fieldErrors(this.error)).map(
      ([name, reason]) => `${name}: ${reason}`,
    );
  }
}

function serialize(vars: EnvVar[]): string {
  return JSON.stringify(vars.map((v) => [v.key, v.value]));
}

/** `追加 1 件・変更 2 件`: counts only, never keys or values. */
function countSummary(counts: {
  added: number;
  changed: number;
  removed: number;
}): string {
  const parts: string[] = [];
  if (counts.added) parts.push(`追加 ${counts.added} 件`);
  if (counts.changed) parts.push(`変更 ${counts.changed} 件`);
  if (counts.removed) parts.push(`削除 ${counts.removed} 件`);
  return parts.join('・');
}

/** Moves the focus to a control when it is rendered, if asked to. */
const focusWhen = modifier<{
  Element: HTMLElement;
  Args: { Positional: [boolean] };
}>((element, [enabled]) => {
  if (enabled) element.focus();
});

interface Signature {
  Args: { model: AppContext; controller: AppEnvController };
}

export default class AppEnvTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare session: SessionService;

  /** Every (re)load starts over from what is saved. */
  list = new Loadable<EnvList>(async () => {
    const list = await this.api.getEnv(this.slug);
    this.apply(list, true);
    return list;
  });

  /**
   * What the repository needs at the head of the deploy branch
   * (ARCHITECTURE 9.4). Compared with the rows on screen, so saving a
   * variable needs no new request.
   */
  requirements = new Loadable<EnvRequirements>(() =>
    this.api.getEnvRequirements(this.slug),
  );

  /**
   * The login is too old to see values (ARCHITECTURE 10.3): only the keys
   * are listed, and nothing can be edited until the user logs in again.
   */
  @tracked hidden = false;
  /**
   * The values stay hidden because of the user's role (below manage), not
   * because of an old login (ARCHITECTURE 10.7).
   */
  @tracked forbidden = false;
  /** Keys deleted since the running release was deployed. */
  @tracked removed: string[] = [];
  /** The saved variables could be compared with the running release. */
  @tracked compared = false;

  @tracked rows: EnvRow[] = [];
  /** The table, or one text field in `.env` format. */
  @tracked mode: 'table' | 'bulk' = 'table';
  @tracked bulkText = '';
  @tracked bulkSaving = false;
  @tracked bulkError: unknown = null;
  /** The `.env` text replaced everything just now. */
  @tracked bulkSaved = false;

  @tracked redeploying = false;
  /** The deployment started from the "not deployed yet" banner. */
  @tracked redeployed: Deployment | null = null;
  @tracked deployError: unknown = null;
  /** A production app asks before it redeploys. */
  @tracked confirming = false;

  /** Read out by screen readers after a row is saved or deleted. */
  @tracked announcement = '';

  /** Where the requirements check reads from (ARCHITECTURE 9.1, 9.4). */
  sourceEditor: SettingsEditor;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.sourceEditor = new SettingsEditor(
      this.api,
      args.model,
      ENV_SOURCE_FIELDS,
      () => void this.requirements.load(),
    );
    void this.list.load();
    void this.requirements.load();
  }

  get focusSource(): boolean {
    return this.args.controller.section === 'env-requirements';
  }

  get slug(): string {
    return this.args.model.slug;
  }

  /**
   * Takes in what the server answered. Rows being edited keep their drafts,
   * and rows being added stay at the end; everything else follows the
   * server, in its order.
   */
  private apply(list: EnvList, reset = false): void {
    this.hidden = list.valuesHidden;
    this.forbidden = list.valuesForbidden;
    this.removed = list.removed;
    this.compared = list.compared;
    if (reset) {
      this.rows = list.vars.map((v) => new EnvRow(v));
      return;
    }
    const byKey = new Map(
      this.rows.filter((row) => !row.fresh).map((row) => [row.key, row]),
    );
    const saved = list.vars.map((v) => {
      const row = byKey.get(v.key) ?? new EnvRow(v);
      row.value = v.value;
      row.pending = v.pending ?? null;
      return row;
    });
    this.rows = [...saved, ...this.rows.filter((row) => row.fresh)];
  }

  get savedVars(): EnvVar[] {
    return this.rows
      .filter((row) => !row.fresh)
      .map((row) => ({ key: row.key, value: row.value }));
  }

  get savedKeys(): string[] {
    return this.rows.filter((row) => !row.fresh).map((row) => row.key);
  }

  /** Saved keys a `.env.example` no longer lists. */
  get unlisted(): Set<string> {
    return new Set(unlistedKeys(this.requirements.value, this.savedKeys));
  }

  isUnlisted = (row: EnvRow): boolean =>
    !row.fresh && this.unlisted.has(row.key);

  get isBulk(): boolean {
    return this.mode === 'bulk';
  }

  get parsed(): DotenvResult {
    return parseDotenv(this.bulkText);
  }

  get bulkRows(): number {
    return Math.min(24, Math.max(8, this.bulkText.split('\n').length + 1));
  }

  get bulkInvalid(): boolean {
    return this.parsed.issues.length > 0;
  }

  get bulkDirty(): boolean {
    return serialize(this.parsed.vars) !== serialize(this.savedVars);
  }

  get cannotSaveBulk(): boolean {
    return this.bulkSaving || this.bulkInvalid || !this.bulkDirty;
  }

  get bulkErrorLines(): string[] {
    return Object.entries(fieldErrors(this.bulkError)).map(
      ([name, reason]) => `${name}: ${reason}`,
    );
  }

  /** The `.env` editor replaces everything, so no row may be half edited. */
  get anyEditing(): boolean {
    return this.rows.some((row) => row.editing || row.confirmingRemove);
  }

  get pendingCounts(): { added: number; changed: number; removed: number } {
    const counts = { added: 0, changed: 0, removed: this.removed.length };
    for (const row of this.rows) {
      if (row.fresh) continue;
      if (row.pending === 'added') counts.added++;
      else if (row.pending === 'changed') counts.changed++;
    }
    return counts;
  }

  get pendingSummary(): string {
    return countSummary(this.pendingCounts);
  }

  /** Saved changes the running release does not have yet. */
  get hasPending(): boolean {
    return this.compared && this.pendingSummary !== '';
  }

  get removedKeys(): string {
    return this.removed.join('、');
  }

  reauthenticate = () => this.session.reauthenticate();

  /** There is a running release to deploy again. */
  get canRedeploy(): boolean {
    return (
      this.args.model.app.current_deployment !== null &&
      can(this.args.model.app, 'deploy')
    );
  }

  get runningSha(): string {
    return shortSha(this.args.model.app.current_deployment?.commit_sha);
  }

  get allRevealed(): boolean {
    const saved = this.rows.filter((row) => !row.fresh);
    return saved.length > 0 && saved.every((row) => row.revealed);
  }

  get canAddFromRepo(): boolean {
    return !this.hidden && !this.isBulk;
  }

  get hasSavedRows(): boolean {
    return this.rows.some((row) => !row.fresh);
  }

  get deployErrorMessage(): string {
    return this.deployError ? errorMessage(this.deployError) : '';
  }

  /** Problem with the key a row is about to save (the API's rules). */
  keyErrorOf = (row: EnvRow): string | undefined => {
    if (!row.editing) return undefined;
    const key = row.trimmedKey;
    if (key === '') return undefined;
    if (!isValidEnvKey(key)) {
      return '英字かアンダースコアで始まる英数字とアンダースコアだけが使えます。';
    }
    const taken = this.rows.some(
      (other) =>
        other !== row &&
        (other.fresh ? other.trimmedKey === key : other.key === key),
    );
    return taken ? 'キーが重複しています。' : undefined;
  };

  canSave = (row: EnvRow): boolean =>
    row.working === null &&
    row.trimmedKey !== '' &&
    row.changed &&
    this.keyErrorOf(row) === undefined;

  cannotSave = (row: EnvRow): boolean => !this.canSave(row);

  isSaving = (row: EnvRow): boolean => row.working === 'save';

  isRemoving = (row: EnvRow): boolean => row.working === 'remove';

  /** A change makes the last result (saved, redeployed) a thing of the past. */
  private touch(): void {
    this.bulkSaved = false;
    this.redeployed = null;
    this.deployError = null;
  }

  add = () => {
    this.rows = [...this.rows, new EnvRow(null)];
  };

  /** Opens a new row for a key the repository asks for. */
  addKey = (key: string) => {
    const open = this.rows.find((row) => row.fresh && row.trimmedKey === key);
    if (open) {
      document.getElementById(`${open.id}-value`)?.focus();
      return;
    }
    this.rows = [...this.rows, new EnvRow(null, key)];
  };

  edit = (row: EnvRow) => {
    row.draftKey = row.key;
    row.draftValue = row.value;
    row.error = null;
    row.focus = 'value';
    row.editing = true;
  };

  cancel = (row: EnvRow) => {
    if (row.working) return;
    if (row.fresh) {
      this.rows = this.rows.filter((r) => r !== row);
      return;
    }
    row.editing = false;
    row.error = null;
    row.draftKey = row.key;
    row.draftValue = row.value;
  };

  save = async (row: EnvRow) => {
    if (!this.canSave(row)) return;
    const key = row.trimmedKey;
    const value = row.draftValue;
    const change: { key?: string; value: string } = { value };
    if (!row.fresh && key !== row.key) change.key = key;
    row.working = 'save';
    row.error = null;
    try {
      const list = await this.api.putEnvVar(
        this.slug,
        row.fresh ? key : row.key,
        change,
      );
      row.key = key;
      row.value = value;
      row.fresh = false;
      row.editing = false;
      row.revealed = false;
      this.apply(list);
      this.touch();
      this.announcement = `${key} を保存しました。次のデプロイで反映されます。`;
    } catch (error) {
      row.error = error;
    } finally {
      row.working = null;
    }
  };

  /** Enter saves from the key; Ctrl/⌘+Enter from the (multi-line) value. */
  keydown = (row: EnvRow, field: 'key' | 'value', event: KeyboardEvent) => {
    if (event.isComposing) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      this.cancel(row);
    } else if (
      event.key === 'Enter' &&
      (field === 'key' || event.ctrlKey || event.metaKey)
    ) {
      event.preventDefault();
      void this.save(row);
    }
  };

  askRemove = (row: EnvRow) => {
    row.error = null;
    row.confirmingRemove = true;
  };

  cancelRemove = (row: EnvRow) => {
    if (row.working) return;
    row.confirmingRemove = false;
    row.error = null;
  };

  remove = async (row: EnvRow) => {
    if (row.working) return;
    row.working = 'remove';
    row.error = null;
    try {
      const list = await this.api.deleteEnvVar(this.slug, row.key);
      this.rows = this.rows.filter((r) => r !== row);
      this.apply(list);
      this.touch();
      this.announcement = `${row.key} を削除しました。次のデプロイで反映されます。`;
    } catch (error) {
      row.error = error;
    } finally {
      row.working = null;
    }
  };

  toggleReveal = (row: EnvRow) => {
    row.revealed = !row.revealed;
  };

  toggleAll = () => {
    const reveal = !this.allRevealed;
    for (const row of this.rows) row.revealed = reveal;
  };

  enterBulk = () => {
    if (this.anyEditing) return;
    this.bulkText = formatDotenv(this.savedVars);
    this.bulkError = null;
    this.bulkSaved = false;
    this.mode = 'bulk';
  };

  leaveBulk = () => {
    if (this.bulkSaving) return;
    this.bulkError = null;
    this.mode = 'table';
  };

  saveBulk = async () => {
    if (this.cannotSaveBulk) return;
    this.bulkSaving = true;
    this.bulkError = null;
    try {
      const list = await this.api.putEnv(this.slug, this.parsed.vars);
      this.list.set(list);
      this.apply(list, true);
      this.touch();
      this.bulkSaved = true;
      this.mode = 'table';
    } catch (error) {
      this.bulkError = error;
    } finally {
      this.bulkSaving = false;
    }
  };

  /** Deploys the running commit again, so it starts with the saved values. */
  private async deployRunning(): Promise<void> {
    const running = this.args.model.app.current_deployment;
    if (!running) return;
    this.redeploying = true;
    this.deployError = null;
    try {
      const body: { ref?: string; sha: string } = { sha: running.commit_sha };
      if (running.ref) body.ref = running.ref;
      this.redeployed = await this.api.createDeployment(this.slug, body);
      this.bulkSaved = false;
      await this.args.model.reload();
    } catch (error) {
      this.deployError = error;
    } finally {
      this.redeploying = false;
    }
  }

  /** Production apps go through the confirmation dialog first. */
  requestRedeploy = () => {
    if (this.redeploying) return;
    if (this.args.model.isProduction) {
      this.confirming = true;
    } else {
      void this.deployRunning();
    }
  };

  confirm = async () => {
    await this.deployRunning();
    this.confirming = false;
  };

  cancelConfirm = () => {
    this.confirming = false;
  };

  <template>
    <TabSection @title="環境変数" data-test-env>
      <:actions>
        {{#if this.hidden}}
          {{! Nothing to reveal or edit until the user logs in again. }}
        {{else if this.list.loaded}}
          {{#unless this.isBulk}}
            {{#if this.hasSavedRows}}
              <Button
                @size="small"
                aria-pressed={{if this.allRevealed "true" "false"}}
                data-test-env-toggle-all
                {{on "click" this.toggleAll}}
              >{{if this.allRevealed "すべて隠す" "すべて表示"}}</Button>
            {{/if}}
            <Button
              @size="small"
              @disabled={{this.anyEditing}}
              title={{if
                this.anyEditing
                "編集中の行を保存するか取り消してから使えます"
              }}
              data-test-env-bulk
              {{on "click" this.enterBulk}}
            >.env 形式で編集</Button>
          {{/unless}}
        {{/if}}
      </:actions>

      <:default>
        <p class="pane-description" data-test-env-notice>
          コンテナに渡す環境変数です。1
          件ずつ保存できます。保存しただけでは反映されず、<strong
          >次のデプロイで反映されます</strong>。
        </p>

        {{#if this.forbidden}}
          <div class="pane-notices">
            <Banner @title="キーだけを表示しています" data-test-env-forbidden>
              値の表示と編集には、このアプリの「管理」のロールが必要です。オーナーにロールの変更を頼んでください。
            </Banner>
          </div>
        {{else if this.hidden}}
          <div class="pane-notices">
            <Banner
              @tone="warning"
              @title="値の表示と編集には再ログインが必要です"
              data-test-env-hidden
            >
              <:default>
                秘密の値を守るため、直近 10
                分以内にログインしていない間は、キーだけを表示しています。
              </:default>
              <:actions>
                <Button
                  @size="small"
                  data-test-env-reauth
                  {{on "click" this.reauthenticate}}
                >GitHub で再ログイン</Button>
              </:actions>
            </Banner>
          </div>
        {{/if}}

        <AsyncView
          @data={{this.list}}
          @errorTitle="環境変数を取得できませんでした"
        >
          <div class="env-feedback" aria-live="polite">
            {{#if this.redeployed}}
              <Banner @tone="info" data-test-env-redeployed>
                <:default>
                  再デプロイを開始しました。完了すると保存した値で動きます。
                </:default>
                <:actions>
                  <LinkTo
                    @route="app.deploy"
                    @model={{this.slug}}
                    class={{buttonClass "default" "small"}}
                  >進行状況を見る</LinkTo>
                </:actions>
              </Banner>
            {{else if this.hasPending}}
              <Banner
                @tone="info"
                @title="未反映の変更があります"
                data-test-env-pending-banner
              >
                <:default>
                  <span data-test-env-pending-summary>
                    {{this.pendingSummary}}
                  </span>
                  が稼働中のアプリにまだ反映されていません。
                  {{#if this.removed.length}}
                    <span class="env-removed" data-test-env-removed>
                      削除したキー:
                      <span class="mono">{{this.removedKeys}}</span>
                    </span>
                  {{/if}}
                </:default>
                <:actions>
                  {{#if this.canRedeploy}}
                    <Button
                      @size="small"
                      @variant="primary"
                      @busy={{this.redeploying}}
                      @busyLabel="実行中…"
                      data-test-env-redeploy
                      {{on "click" this.requestRedeploy}}
                    >再デプロイ</Button>
                  {{/if}}
                </:actions>
              </Banner>
            {{else if this.bulkSaved}}
              <Banner @tone="info" data-test-env-saved>
                保存しました。次のデプロイで反映されます。
              </Banner>
            {{/if}}
            {{#if this.deployError}}
              <Banner
                @tone="error"
                @title="再デプロイを開始できませんでした"
                data-test-env-deploy-error
              >{{this.deployErrorMessage}}</Banner>
            {{/if}}
          </div>

          {{#unless this.isBulk}}
            <EnvRequirementsPanel
              @slug={{this.slug}}
              @requirements={{this.requirements}}
              @savedKeys={{this.savedKeys}}
              @canAdd={{this.canAddFromRepo}}
              @onAdd={{this.addKey}}
            />
          {{/unless}}

          {{#if this.isBulk}}
            <form
              class="env-bulk"
              autocomplete="off"
              novalidate
              data-test-env-bulk-form
              {{on "submit" (preventDefault this.saveBulk)}}
            >
              <label class="field-label" for="env-bulk-text">
                .env 形式
              </label>
              <textarea
                id="env-bulk-text"
                class="mono env-bulk-text"
                rows={{this.bulkRows}}
                spellcheck="false"
                autocapitalize="off"
                autocomplete="off"
                wrap="off"
                value={{this.bulkText}}
                aria-invalid={{if this.bulkInvalid "true"}}
                aria-describedby="env-bulk-hint env-bulk-issues"
                data-test-env-bulk-text
                {{on "input" (setValue this "bulkText")}}
              ></textarea>
              <p class="field-hint" id="env-bulk-hint">
                1 行に 1 つ、<code>KEY=VALUE</code>
                の形で書きます。<code>#</code>
                で始まる行と空行は無視されます。空白や改行を含む値は引用符で囲みます。値はそのまま表示されています。<strong
                >保存すると、ここに書いた内容で全体を置き換えます</strong>。
              </p>
              <div id="env-bulk-issues" aria-live="polite">
                {{#if this.parsed.issues.length}}
                  <ul class="env-issues" data-test-env-issues>
                    {{#each this.parsed.issues key="line" as |issue|}}
                      <li data-test-env-issue={{issue.line}}>
                        <Icon @name="error" />
                        <span class="env-issue-line">{{issue.line}}
                          行目</span>
                        <span>{{issue.message}}</span>
                      </li>
                    {{/each}}
                  </ul>
                {{/if}}
              </div>
              <div class="form-footer env-footer">
                <Button
                  @type="submit"
                  @variant="primary"
                  @disabled={{this.cannotSaveBulk}}
                  @busy={{this.bulkSaving}}
                  @busyLabel="保存中…"
                  data-test-env-bulk-save
                >すべて置き換えて保存</Button>
                <Button
                  @variant="ghost"
                  @disabled={{this.bulkSaving}}
                  data-test-env-bulk-cancel
                  {{on "click" this.leaveBulk}}
                >キャンセル</Button>
              </div>
              <ErrorMessage @error={{this.bulkError}} />
              {{#if this.bulkErrorLines.length}}
                <ul
                  class="form-problems env-problems"
                  data-test-env-bulk-errors
                >
                  {{#each this.bulkErrorLines as |line|}}
                    <li>{{line}}</li>
                  {{/each}}
                </ul>
              {{/if}}
            </form>
          {{else if this.rows.length}}
            <RowList class="env-vars" @label="環境変数" data-test-env-list>
              <:header>
                <span>キー</span>
                <span>値</span>
              </:header>
              <:rows as |Row|>
                {{#each this.rows key="id" as |row|}}
                  {{#let (this.keyErrorOf row) as |keyError|}}
                    <Row
                      class={{if row.editing "is-editing"}}
                      data-test-env-row
                      data-pending={{row.pending}}
                    >
                      <div class="env-var-key">
                        {{#if row.editing}}
                          <label
                            class="visually-hidden"
                            for="{{row.id}}-key"
                          >キー</label>
                          <input
                            id="{{row.id}}-key"
                            type="text"
                            class="mono"
                            placeholder="KEY"
                            spellcheck="false"
                            autocapitalize="off"
                            autocomplete="off"
                            value={{row.draftKey}}
                            disabled={{if row.working true}}
                            aria-invalid={{if keyError "true"}}
                            aria-describedby={{if
                              keyError
                              (concat row.id "-key-error")
                            }}
                            data-test-env-key-input
                            {{on "input" (setValue row "draftKey")}}
                            {{on "keydown" (fn this.keydown row "key")}}
                            {{focusWhen row.focusKey}}
                          />
                        {{else}}
                          <span class="env-var-name-line">
                            <span class="visually-hidden">キー</span>
                            <span class="env-var-name" data-test-env-key>
                              {{row.key}}
                            </span>
                            {{#if row.pending}}
                              <Badge data-test-env-pending>
                                {{row.pendingLabel}}
                              </Badge>
                            {{/if}}
                            {{#if (this.isUnlisted row)}}
                              <Badge
                                title="リポジトリの .env.example に書かれていない変数です"
                                data-test-env-unlisted
                              >例にない</Badge>
                            {{/if}}
                          </span>
                        {{/if}}
                        {{#if keyError}}
                          <p
                            class="field-error"
                            id="{{row.id}}-key-error"
                            data-test-env-key-error
                          >{{keyError}}</p>
                        {{/if}}
                      </div>

                      <div class="env-var-value">
                        {{#if row.editing}}
                          <label
                            class="visually-hidden"
                            for="{{row.id}}-value"
                          >値</label>
                          <textarea
                            id="{{row.id}}-value"
                            class="mono"
                            rows={{row.valueRows}}
                            spellcheck="false"
                            autocapitalize="off"
                            autocomplete="off"
                            value={{row.draftValue}}
                            disabled={{if row.working true}}
                            aria-describedby="{{row.id}}-hint"
                            data-test-env-value-input
                            {{on "input" (setValue row "draftValue")}}
                            {{on "keydown" (fn this.keydown row "value")}}
                            {{focusWhen row.focusValue}}
                          ></textarea>
                          <p
                            class="field-hint env-row-hint"
                            id="{{row.id}}-hint"
                          >
                            Ctrl+Enter（⌘+Enter）で保存、Esc で取り消し
                          </p>
                        {{else if row.revealed}}
                          <span class="visually-hidden">値</span>
                          {{#if row.value}}
                            {{! No white space around the value: it is
                                shown as written, line breaks included. }}
                            <span
                              class="env-var-text"
                              data-test-env-value
                            >{{row.value}}</span>
                          {{else}}
                            <span class="env-var-empty" data-test-env-value>
                              （空）
                            </span>
                          {{/if}}
                        {{else}}
                          <span
                            class="env-var-masked"
                            aria-hidden="true"
                            data-test-env-masked
                          >{{MASK}}</span>
                          <span class="visually-hidden">
                            値は隠されています
                          </span>
                        {{/if}}
                      </div>

                      <div class="env-var-actions">
                        {{#if this.hidden}}
                          {{! Keys only: no value to show, copy or edit. }}
                        {{else if row.confirmingRemove}}
                          <span class="env-confirm-text">削除しますか？</span>
                          <Button
                            @variant="danger"
                            @size="small"
                            @busy={{this.isRemoving row}}
                            @busyLabel="削除中…"
                            aria-label="{{row.key}} を削除する"
                            data-test-env-remove-confirm
                            {{on "click" (fn this.remove row)}}
                          >削除する</Button>
                          <Button
                            @variant="ghost"
                            @size="small"
                            @disabled={{if row.working true}}
                            data-test-env-remove-cancel
                            {{on "click" (fn this.cancelRemove row)}}
                          >キャンセル</Button>
                        {{else if row.editing}}
                          <Button
                            @variant="primary"
                            @size="small"
                            @disabled={{this.cannotSave row}}
                            @busy={{this.isSaving row}}
                            @busyLabel="保存中…"
                            aria-label={{if
                              row.fresh
                              "追加する変数を保存"
                              (concat row.key " を保存")
                            }}
                            data-test-env-save
                            {{on "click" (fn this.save row)}}
                          >保存</Button>
                          <Button
                            @variant="ghost"
                            @size="small"
                            @disabled={{if row.working true}}
                            aria-label={{if
                              row.fresh
                              "変数の追加を取り消す"
                              (concat row.key " の編集を取り消す")
                            }}
                            data-test-env-cancel
                            {{on "click" (fn this.cancel row)}}
                          >キャンセル</Button>
                        {{else}}
                          <button
                            type="button"
                            class="button button-ghost button-icon"
                            aria-pressed={{if row.revealed "true" "false"}}
                            aria-label="{{row.key}} の値を表示"
                            title={{if row.revealed "値を隠す" "値を表示"}}
                            data-test-env-reveal
                            {{on "click" (fn this.toggleReveal row)}}
                          >
                            <Icon @name={{if row.revealed "eye-off" "eye"}} />
                          </button>
                          <CopyButton
                            @text={{row.value}}
                            @label="{{row.key}} の値"
                          />
                          <Button
                            @variant="ghost"
                            @size="small"
                            aria-label="{{row.key}} を編集"
                            data-test-env-edit
                            {{on "click" (fn this.edit row)}}
                          >編集</Button>
                          <Button
                            @variant="ghost"
                            @size="small"
                            aria-label="{{row.key}} を削除"
                            data-test-env-remove
                            {{on "click" (fn this.askRemove row)}}
                          >削除</Button>
                        {{/if}}
                      </div>

                      {{#if row.error}}
                        <div class="env-row-error" data-test-env-row-error>
                          <p class="field-error">{{row.errorText}}</p>
                          {{#each row.errorLines as |line|}}
                            <p class="field-error">{{line}}</p>
                          {{/each}}
                        </div>
                      {{/if}}
                    </Row>
                  {{/let}}
                {{/each}}
              </:rows>
            </RowList>

            {{#unless this.hidden}}
              <div class="env-add">
                <Button data-test-env-add {{on "click" this.add}}>
                  変数を追加
                </Button>
              </div>
            {{/unless}}
          {{else if this.forbidden}}
            <div class="row-list" data-test-env-empty>
              <EmptyState @title="環境変数はまだありません" />
            </div>
          {{else}}
            <div class="row-list" data-test-env-empty>
              <EmptyState
                @title="環境変数はまだありません"
                @description="コンテナに渡す設定や秘密の値をここで管理します。まとめて登録するときは「.env 形式で編集」に貼り付けます。"
              >
                <:action>
                  <Button
                    @variant="primary"
                    data-test-env-add
                    {{on "click" this.add}}
                  >変数を追加</Button>
                </:action>
              </EmptyState>
            </div>
          {{/if}}

          <p class="visually-hidden" aria-live="polite" data-test-env-announce>
            {{this.announcement}}
          </p>
        </AsyncView>
      </:default>
    </TabSection>

    <EnvSourceSettings
      @editor={{this.sourceEditor}}
      @focus={{this.focusSource}}
    />
    <UnsavedSettingsGuard @editor={{this.sourceEditor}} @what="照合元" />

    {{#if this.confirming}}
      <ConfirmDialog
        @title="本番アプリを再デプロイしますか？"
        @confirmLabel="再デプロイする"
        @busy={{this.redeploying}}
        @onConfirm={{this.confirm}}
        @onCancel={{this.cancelConfirm}}
      >
        <p>本番アプリ
          <strong>{{this.slug}}</strong>
          で、稼働中のコミット
          <strong data-test-dialog-target>{{this.runningSha}}</strong>
          を保存済みの環境変数で再デプロイします。</p>
        <p
        >コンテナの入れ替えで数秒の断があります。起動に失敗した場合は、直前のリリースに以前の環境変数で戻ります。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
