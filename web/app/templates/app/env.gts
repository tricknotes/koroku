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
import ErrorMessage from 'koroku-console/components/error-message';
import Icon from 'koroku-console/components/icon';
import RowList from 'koroku-console/components/row-list';
import TabSection from 'koroku-console/components/tab-section';
import type ApiService from 'koroku-console/services/api';
import type SessionService from 'koroku-console/services/session';
import type AppContext from 'koroku-console/utils/app-context';
import { preventDefault, setValue } from 'koroku-console/utils/dom';
import {
  formatDotenv,
  parseDotenv,
  type DotenvResult,
} from 'koroku-console/utils/dotenv';
import { errorMessage, fieldErrors } from 'koroku-console/utils/errors';
import { isValidEnvKey, shortSha } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import type { Deployment, EnvVar } from 'koroku-console/utils/types';

/** How a row differs from what is saved. */
type RowStatus = 'added' | 'changed';

const ROW_STATUS_LABELS: Record<RowStatus, string> = {
  added: '新規',
  changed: '変更',
};

/** What stands in for a hidden value: the same for every length. */
const MASK = '••••••••';

let nextRowId = 0;

class EnvRow {
  readonly id = `env-${nextRowId++}`;
  @tracked key: string;
  @tracked value: string;
  /** The value is shown as text. Until then it is not in the DOM at all. */
  @tracked revealed = false;
  /** Key and value are form controls. */
  @tracked editing: boolean;
  /**
   * The field that takes the focus when the row turns into a form: the key
   * of a row added just now, the value of one that is being changed.
   */
  focus: 'key' | 'value' | null;

  constructor(key: string, value: string, fresh = false) {
    this.key = key;
    this.value = value;
    this.editing = fresh;
    this.focus = fresh ? 'key' : null;
  }

  get focusKey(): boolean {
    return this.focus === 'key';
  }

  get focusValue(): boolean {
    return this.focus === 'value';
  }

  get blank(): boolean {
    return this.key.trim() === '' && this.value === '';
  }

  get valueRows(): number {
    return Math.min(8, this.value.split('\n').length);
  }
}

/** What changed between the saved variables and the ones on screen. */
interface Changes {
  added: number;
  changed: number;
  removed: number;
}

function serialize(vars: EnvVar[]): string {
  return JSON.stringify(vars.map((v) => [v.key, v.value]));
}

function statusLabel(status: RowStatus): string {
  return ROW_STATUS_LABELS[status];
}

/** Moves the focus to a control when it is rendered, if asked to. */
const focusWhen = modifier<{
  Element: HTMLElement;
  Args: { Positional: [boolean] };
}>((element, [enabled]) => {
  if (enabled) element.focus();
});

interface Signature {
  Args: { model: AppContext };
}

export default class AppEnvTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare session: SessionService;

  /** Every (re)load starts the editor over from what is saved. */
  vars = new Loadable<EnvVar[]>(async () => {
    const { vars, valuesHidden } = await this.api.getEnv(this.slug);
    this.hidden = valuesHidden;
    this.reset(vars);
    return vars;
  });

  /**
   * The login is too old to see values (ARCHITECTURE 10.3): only the keys
   * are listed, and nothing can be edited until the user logs in again.
   */
  @tracked hidden = false;

  @tracked rows: EnvRow[] = [];
  /** The table, or one text field in `.env` format. */
  @tracked mode: 'table' | 'bulk' = 'table';
  @tracked bulkText = '';

  @tracked working: 'save' | 'redeploy' | null = null;
  @tracked saved = false;
  @tracked error: unknown = null;
  @tracked serverErrors: Record<string, string> = {};
  /** The deployment started by "save and redeploy". */
  @tracked redeployed: Deployment | null = null;
  @tracked deployError: unknown = null;
  /** A production app asks before it redeploys; this is what it will do. */
  @tracked confirming: 'save-and-redeploy' | 'redeploy' | null = null;

  @tracked private original: EnvVar[] = [];

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.vars.load();
  }

  get slug(): string {
    return this.args.model.slug;
  }

  private reset(vars: EnvVar[]): void {
    this.original = vars;
    this.rows = vars.map((v) => new EnvRow(v.key, v.value));
    this.bulkText = formatDotenv(vars);
  }

  get isBulk(): boolean {
    return this.mode === 'bulk';
  }

  get parsed(): DotenvResult {
    return parseDotenv(this.bulkText);
  }

  get bulkRows(): number {
    return Math.min(24, Math.max(8, this.bulkText.split('\n').length + 1));
  }

  /** What would be sent: rows left completely empty are ignored. */
  get current(): EnvVar[] {
    if (this.isBulk) return this.parsed.vars;
    return this.rows
      .filter((row) => !row.blank)
      .map((row) => ({ key: row.key.trim(), value: row.value }));
  }

  get dirty(): boolean {
    return serialize(this.current) !== serialize(this.original);
  }

  private get savedValues(): Map<string, string> {
    return new Map(this.original.map((v) => [v.key, v.value]));
  }

  get changes(): Changes {
    const saved = this.savedValues;
    const changes: Changes = { added: 0, changed: 0, removed: 0 };
    const keys = new Set<string>();
    for (const { key, value } of this.current) {
      keys.add(key);
      if (!saved.has(key)) changes.added++;
      else if (saved.get(key) !== value) changes.changed++;
    }
    for (const key of saved.keys()) {
      if (!keys.has(key)) changes.removed++;
    }
    return changes;
  }

  /** `追加 1 件・変更 2 件`: counts only, never keys or values. */
  get changeSummary(): string {
    const { added, changed, removed } = this.changes;
    const parts: string[] = [];
    if (added) parts.push(`追加 ${added} 件`);
    if (changed) parts.push(`変更 ${changed} 件`);
    if (removed) parts.push(`削除 ${removed} 件`);
    return parts.join('・');
  }

  /** Row id → how the row differs from the saved variables. */
  get rowStatuses(): Record<string, RowStatus> {
    const saved = this.savedValues;
    const statuses: Record<string, RowStatus> = {};
    for (const row of this.rows) {
      if (row.blank) continue;
      const key = row.key.trim();
      if (!saved.has(key)) statuses[row.id] = 'added';
      else if (saved.get(key) !== row.value) statuses[row.id] = 'changed';
    }
    return statuses;
  }

  /** Row id → problem with its key (the API's rules, checked up front). */
  get keyErrors(): Record<string, string> {
    const errors: Record<string, string> = {};
    const seen = new Set<string>();
    for (const row of this.rows) {
      if (row.blank) continue;
      const key = row.key.trim();
      if (key === '') {
        errors[row.id] = 'キーを入力してください。';
      } else if (!isValidEnvKey(key)) {
        errors[row.id] =
          '英字かアンダースコアで始まる英数字とアンダースコアだけが使えます。';
      } else if (seen.has(key)) {
        errors[row.id] = 'キーが重複しています。';
      }
      seen.add(key);
    }
    return errors;
  }

  get invalid(): boolean {
    return this.isBulk
      ? this.parsed.issues.length > 0
      : Object.keys(this.keyErrors).length > 0;
  }

  get busy(): boolean {
    return this.working !== null;
  }

  /** Nothing saved and nothing typed: the empty state holds the one action. */
  get showFooter(): boolean {
    if (this.hidden) return false;
    return this.isBulk || this.rows.length > 0 || this.dirty;
  }

  reauthenticate = () => this.session.reauthenticate();

  get cannotSave(): boolean {
    return this.busy || this.invalid || !this.dirty;
  }

  /** There is a running release to deploy again. */
  get canRedeploy(): boolean {
    return this.args.model.app.current_deployment !== null;
  }

  /** Middle of the confirmation sentence: what happens before the deploy. */
  get confirmLead(): string {
    return this.confirming === 'save-and-redeploy'
      ? 'の環境変数を保存して、稼働中のコミット'
      : 'で、稼働中のコミット';
  }

  get runningSha(): string {
    return shortSha(this.args.model.app.current_deployment?.commit_sha);
  }

  get allRevealed(): boolean {
    return this.rows.length > 0 && this.rows.every((row) => row.revealed);
  }

  get serverErrorLines(): string[] {
    return Object.entries(this.serverErrors).map(
      ([name, reason]) => `${name}: ${reason}`,
    );
  }

  get deployErrorMessage(): string {
    return this.deployError ? errorMessage(this.deployError) : '';
  }

  add = () => {
    this.rows = [...this.rows, new EnvRow('', '', true)];
    this.touch();
  };

  remove = (row: EnvRow) => {
    this.rows = this.rows.filter((r) => r !== row);
    this.touch();
  };

  toggleReveal = (row: EnvRow) => {
    row.revealed = !row.revealed;
  };

  toggleEdit = (row: EnvRow) => {
    row.focus = 'value';
    row.editing = !row.editing;
  };

  toggleAll = () => {
    const reveal = !this.allRevealed;
    for (const row of this.rows) row.revealed = reveal;
  };

  /** An edit makes the last result (saved, redeployed) a thing of the past. */
  touch = () => {
    this.saved = false;
    this.redeployed = null;
    this.deployError = null;
  };

  enterBulk = () => {
    this.bulkText = formatDotenv(this.current);
    this.mode = 'bulk';
  };

  leaveBulk = () => {
    if (this.invalid) return;
    this.rows = this.parsed.vars.map((v) => new EnvRow(v.key, v.value));
    this.mode = 'table';
  };

  discard = () => {
    this.reset(this.original);
    this.error = null;
    this.serverErrors = {};
  };

  save = async () => {
    if (this.cannotSave) return;
    this.working = 'save';
    await this.put();
    this.working = null;
  };

  /** Stores the variables; resolves to whether that worked. */
  private async put(): Promise<boolean> {
    this.saved = false;
    this.redeployed = null;
    this.deployError = null;
    this.error = null;
    this.serverErrors = {};
    try {
      const saved = await this.api.putEnv(this.slug, this.current);
      this.vars.set(saved);
      this.reset(saved);
      this.saved = true;
      return true;
    } catch (error) {
      this.error = error;
      this.serverErrors = fieldErrors(error);
      return false;
    }
  }

  /** Deploys the running commit again, so it starts with the new values. */
  private async deployRunning(): Promise<void> {
    const running = this.args.model.app.current_deployment;
    if (!running) return;
    this.deployError = null;
    try {
      const body: { ref?: string; sha: string } = { sha: running.commit_sha };
      if (running.ref) body.ref = running.ref;
      this.redeployed = await this.api.createDeployment(this.slug, body);
      await this.args.model.reload();
    } catch (error) {
      this.deployError = error;
    }
  }

  /** Production apps go through the confirmation dialog first. */
  private request(action: 'save-and-redeploy' | 'redeploy'): void {
    if (this.args.model.isProduction) {
      this.confirming = action;
    } else {
      void this.run(action);
    }
  }

  requestSaveAndRedeploy = () => {
    if (!this.cannotSave) this.request('save-and-redeploy');
  };

  requestRedeploy = () => {
    if (!this.busy) this.request('redeploy');
  };

  private async run(action: 'save-and-redeploy' | 'redeploy'): Promise<void> {
    this.working = 'redeploy';
    const stored = action === 'redeploy' || (await this.put());
    if (stored) await this.deployRunning();
    this.working = null;
  }

  confirm = async () => {
    if (this.confirming) await this.run(this.confirming);
    this.confirming = null;
  };

  cancelConfirm = () => {
    this.confirming = null;
  };

  isWorking = (kind: 'save' | 'redeploy'): boolean => this.working === kind;

  statusOf = (row: EnvRow): RowStatus | undefined => this.rowStatuses[row.id];

  keyErrorOf = (row: EnvRow): string | undefined => this.keyErrors[row.id];

  <template>
    <TabSection @title="環境変数" data-test-env>
      <:actions>
        {{#if this.hidden}}
          {{! Nothing to reveal or edit until the user logs in again. }}
        {{else if this.vars.loaded}}
          {{#if this.isBulk}}
            <Button
              @size="small"
              @disabled={{this.invalid}}
              data-test-env-table
              {{on "click" this.leaveBulk}}
            >表に戻る</Button>
          {{else}}
            {{#if this.rows.length}}
              <Button
                @size="small"
                aria-pressed={{if this.allRevealed "true" "false"}}
                data-test-env-toggle-all
                {{on "click" this.toggleAll}}
              >{{if this.allRevealed "すべて隠す" "すべて表示"}}</Button>
            {{/if}}
            <Button
              @size="small"
              data-test-env-bulk
              {{on "click" this.enterBulk}}
            >.env 形式で編集</Button>
          {{/if}}
        {{/if}}
      </:actions>

      <:default>
        <p class="pane-description" data-test-env-notice>
          コンテナに渡す環境変数です。変更は保存しただけでは反映されず、<strong
          >次のデプロイで反映されます</strong>。
        </p>

        {{#if this.hidden}}
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
          @data={{this.vars}}
          @errorTitle="環境変数を取得できませんでした"
        >
          <form
            class="env-form"
            autocomplete="off"
            novalidate
            data-test-env-form
            {{on "submit" (preventDefault this.save)}}
            {{on "input" this.touch}}
          >
            {{#if this.isBulk}}
              <div class="env-bulk">
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
                  aria-invalid={{if this.invalid "true"}}
                  aria-describedby="env-bulk-hint env-bulk-issues"
                  data-test-env-bulk-text
                  {{on "input" (setValue this "bulkText")}}
                ></textarea>
                <p class="field-hint" id="env-bulk-hint">
                  1 行に 1 つ、<code>KEY=VALUE</code>
                  の形で書きます。<code>#</code>
                  で始まる行と空行は無視されます。空白や改行を含む値は引用符で囲みます。値はそのまま表示されています。
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
              </div>
            {{else if this.rows.length}}
              <RowList class="env-vars" @label="環境変数" data-test-env-list>
                <:header>
                  <span>キー</span>
                  <span>値</span>
                </:header>
                <:rows as |Row|>
                  {{#each this.rows key="id" as |row|}}
                    {{#let
                      (this.statusOf row) (this.keyErrorOf row)
                      as |status keyError|
                    }}
                      <Row
                        class={{if row.editing "is-editing"}}
                        data-test-env-row
                        data-status={{status}}
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
                              value={{row.key}}
                              aria-invalid={{if keyError "true"}}
                              aria-describedby={{if
                                keyError
                                (concat row.id "-key-error")
                              }}
                              data-test-env-key-input
                              {{on "input" (setValue row "key")}}
                              {{focusWhen row.focusKey}}
                            />
                          {{else}}
                            <span class="env-var-name-line">
                              <span class="visually-hidden">キー</span>
                              <span class="env-var-name" data-test-env-key>
                                {{row.key}}
                              </span>
                              {{#if status}}
                                <Badge data-test-env-status>
                                  {{statusLabel status}}
                                </Badge>
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
                              value={{row.value}}
                              data-test-env-value-input
                              {{on "input" (setValue row "value")}}
                              {{focusWhen row.focusValue}}
                            ></textarea>
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
                          {{else}}
                            {{#unless row.editing}}
                              <button
                                type="button"
                                class="button button-ghost button-icon"
                                aria-pressed={{if row.revealed "true" "false"}}
                                aria-label="{{row.key}} の値を表示"
                                title={{if row.revealed "値を隠す" "値を表示"}}
                                data-test-env-reveal
                                {{on "click" (fn this.toggleReveal row)}}
                              >
                                <Icon
                                  @name={{if row.revealed "eye-off" "eye"}}
                                />
                              </button>
                              <CopyButton
                                @text={{row.value}}
                                @label="{{row.key}} の値"
                              />
                            {{/unless}}
                            <Button
                              @variant="ghost"
                              @size="small"
                              aria-label="{{row.key}}
                              {{if row.editing 'の編集を完了' 'を編集'}}"
                              data-test-env-edit
                              {{on "click" (fn this.toggleEdit row)}}
                            >{{if row.editing "完了" "編集"}}</Button>
                            <Button
                              @variant="ghost"
                              @size="small"
                              aria-label="{{row.key}} を削除"
                              data-test-env-remove
                              {{on "click" (fn this.remove row)}}
                            >削除</Button>
                          {{/if}}
                        </div>
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
            {{else}}
              <div class="row-list" data-test-env-empty>
                <EmptyState
                  @title="環境変数はまだありません"
                  @description="コンテナに渡す設定や秘密の値をここで管理します。まとめて登録するときは「.env 形式で編集」に貼り付けます。"
                >
                  <:action>
                    <Button
                      @variant={{if this.showFooter "default" "primary"}}
                      data-test-env-add
                      {{on "click" this.add}}
                    >変数を追加</Button>
                  </:action>
                </EmptyState>
              </div>
            {{/if}}

            {{#if this.showFooter}}
              {{! The same footer as the settings form: the ways to save,
                  then what there is to save. }}
              <div class="form-footer env-footer">
                <Button
                  @type="submit"
                  @variant="primary"
                  @disabled={{this.cannotSave}}
                  @busy={{this.isWorking "save"}}
                  @busyLabel="保存中…"
                  data-test-env-save
                >保存</Button>
                {{#if this.canRedeploy}}
                  <Button
                    @disabled={{this.cannotSave}}
                    @busy={{this.isWorking "redeploy"}}
                    @busyLabel="実行中…"
                    data-test-env-save-deploy
                    {{on "click" this.requestSaveAndRedeploy}}
                  >保存して再デプロイ</Button>
                {{/if}}
                {{#if this.dirty}}
                  <Button
                    @variant="ghost"
                    @disabled={{this.busy}}
                    data-test-env-discard
                    {{on "click" this.discard}}
                  >変更を破棄</Button>
                {{/if}}
                <p class="form-status" aria-live="polite">
                  {{#if this.dirty}}
                    <span class="env-dirty" data-test-env-dirty>
                      <span class="env-dirty-mark" aria-hidden="true"></span>
                      未保存の変更があります（{{this.changeSummary}}）
                    </span>
                  {{else}}
                    <span data-test-env-unchanged>変更はありません。</span>
                  {{/if}}
                </p>
              </div>
            {{/if}}

            <div class="env-feedback" aria-live="polite">
              {{#if this.redeployed}}
                <Banner @tone="info" data-test-env-redeployed>
                  <:default>
                    {{#if this.saved}}
                      保存して、再デプロイを開始しました。
                    {{else}}
                      再デプロイを開始しました。
                    {{/if}}
                    完了すると新しい値で動きます。
                  </:default>
                  <:actions>
                    <LinkTo
                      @route="app.deploy"
                      @model={{this.slug}}
                      class={{buttonClass "default" "small"}}
                    >進行状況を見る</LinkTo>
                  </:actions>
                </Banner>
              {{else if this.saved}}
                <Banner @tone="info" data-test-env-saved>
                  <:default
                  >保存しました。次のデプロイで反映されます。</:default>
                  <:actions>
                    {{#if this.canRedeploy}}
                      <Button
                        @size="small"
                        @busy={{this.isWorking "redeploy"}}
                        @busyLabel="実行中…"
                        data-test-env-redeploy
                        {{on "click" this.requestRedeploy}}
                      >今すぐ再デプロイ</Button>
                    {{/if}}
                  </:actions>
                </Banner>
              {{/if}}
              {{#if this.deployError}}
                <Banner
                  @tone="error"
                  @title="再デプロイを開始できませんでした"
                  data-test-env-deploy-error
                >{{this.deployErrorMessage}}</Banner>
              {{/if}}
              <ErrorMessage @error={{this.error}} />
              {{#if this.serverErrorLines.length}}
                <ul
                  class="form-problems env-problems"
                  data-test-env-server-errors
                >
                  {{#each this.serverErrorLines as |line|}}
                    <li>{{line}}</li>
                  {{/each}}
                </ul>
              {{/if}}
            </div>
          </form>
        </AsyncView>
      </:default>
    </TabSection>

    {{#if this.confirming}}
      <ConfirmDialog
        @title="本番アプリを再デプロイしますか？"
        @confirmLabel="再デプロイする"
        @busy={{this.busy}}
        @onConfirm={{this.confirm}}
        @onCancel={{this.cancelConfirm}}
      >
        <p>本番アプリ
          <strong>{{this.slug}}</strong>
          {{this.confirmLead}}
          <strong data-test-dialog-target>{{this.runningSha}}</strong>
          を再デプロイします。</p>
        <p
        >コンテナの入れ替えで数秒の断があります。起動に失敗した場合は、直前のリリースに以前の環境変数で戻ります。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
