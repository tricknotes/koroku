import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import type Owner from '@ember/owner';
import type RouterService from '@ember/routing/router-service';
import AppGroupSettings from 'koroku-console/components/app-group-settings';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import StatusIndicator from 'koroku-console/components/status-indicator';
import type ApiService from 'koroku-console/services/api';
import type AppContext from 'koroku-console/utils/app-context';
import {
  inputValue,
  preventDefault,
  setChecked,
  setValue,
} from 'koroku-console/utils/dom';
import { fieldErrors, policyViolations } from 'koroku-console/utils/errors';
import SettingsForm, { LimitRow } from 'koroku-console/utils/settings-form';
import {
  APP_STATE_HINTS,
  APP_STATE_KINDS,
  APP_STATE_LABELS,
  deriveAppState,
} from 'koroku-console/utils/status';
import type { App, EnvSource, Environment } from 'koroku-console/utils/types';

const KNOWN_FIELDS = new Set([
  'default_branch',
  'compose_path',
  'web_service',
  'web_port',
  'healthcheck_path',
  'environment',
  'memory_limit',
  'service_memory_limits',
  'env_source',
  'env_example_path',
]);

type ControlAction = 'start' | 'stop' | 'restart';

const CONTROL_DONE: Record<ControlAction, string> = {
  start: '起動しました。',
  stop: '停止しました。',
  restart: '再起動しました。',
};

function isChecked(current: Environment, value: Environment): boolean {
  return current === value;
}

function isSource(current: EnvSource, value: EnvSource): boolean {
  return current === value;
}

const VAR_PLAIN = '${VAR}';
const VAR_REQUIRED = '${VAR:?説明}';
const VAR_OPTIONAL = '${VAR:-既定値}';

function isBusy(current: ControlAction | null, action: ControlAction): boolean {
  return current === action;
}

/** Where the repository can be looked at, when that is a web address. */
function repositoryUrl(app: App): string | null {
  if (app.repo_full_name) return `https://github.com/${app.repo_full_name}`;
  return /^https?:\/\//.test(app.repo_url) ? app.repo_url : null;
}

function repositoryName(app: App): string {
  return app.repo_full_name || app.repo_url;
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppSettingsTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare router: RouterService;

  @tracked form: SettingsForm;
  @tracked saving = false;
  @tracked saved = false;
  @tracked nothingToSave = false;
  @tracked error: unknown = null;
  @tracked errors: Record<string, string> = {};

  @tracked controlling: ControlAction | null = null;
  @tracked controlNotice = '';
  @tracked controlError: unknown = null;
  @tracked confirmingStop = false;

  @tracked confirmingDelete = false;
  @tracked deleteVolumes = false;
  @tracked deleting = false;
  @tracked deleteError: unknown = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.form = new SettingsForm(this.app);
  }

  get app(): App {
    return this.args.model.app;
  }

  get state() {
    const state = deriveAppState(this.app);
    return {
      kind: APP_STATE_KINDS[state],
      label: APP_STATE_LABELS[state],
      hint: APP_STATE_HINTS[state],
    };
  }

  /** Start, stop and restart need a release to act on. */
  get neverDeployed(): boolean {
    return this.app.current_deployment === null;
  }

  get stopped(): boolean {
    return this.app.desired_state === 'stopped';
  }

  get controlsDisabled(): boolean {
    return this.neverDeployed || this.controlling !== null;
  }

  get cannotStop(): boolean {
    return this.controlsDisabled || this.stopped;
  }

  get otherErrors(): string[] {
    return Object.entries(this.errors)
      .filter(([name]) => !KNOWN_FIELDS.has(name))
      .map(([name, reason]) => `${name}: ${reason}`);
  }

  /** The environment cannot change while the app is in a group. */
  get inGroup(): boolean {
    return (this.app.group_id ?? null) !== null;
  }

  get violations(): string[] {
    return policyViolations(this.controlError);
  }

  setEnvironment = (event: Event) => {
    this.form.environment = inputValue(event) as Environment;
  };

  setEnvSource = (event: Event) => {
    this.form.envSource = inputValue(event) as EnvSource;
  };

  addLimit = () => {
    this.form.limits = [...this.form.limits, new LimitRow()];
  };

  removeLimit = (row: LimitRow) => {
    this.form.limits = this.form.limits.filter((r) => r !== row);
    this.touch();
  };

  touch = () => {
    this.saved = false;
    this.nothingToSave = false;
  };

  save = async () => {
    this.saved = false;
    this.nothingToSave = false;
    this.error = null;
    this.errors = this.form.validate();
    if (Object.keys(this.errors).length > 0) return;

    const patch = this.form.patch(this.app);
    if (Object.keys(patch).length === 0) {
      this.nothingToSave = true;
      return;
    }

    this.saving = true;
    try {
      const app = await this.api.updateApp(this.app.slug, patch);
      this.args.model.set(app);
      this.form = new SettingsForm(app);
      this.saved = true;
    } catch (error) {
      this.error = error;
      this.errors = fieldErrors(error);
    } finally {
      this.saving = false;
    }
  };

  control = async (action: ControlAction) => {
    if (this.controlling) return;
    this.controlling = action;
    this.controlNotice = '';
    this.controlError = null;
    try {
      const app = await this.api.controlApp(this.app.slug, action);
      this.args.model.set(app);
      this.controlNotice = CONTROL_DONE[action];
    } catch (error) {
      this.controlError = error;
    } finally {
      this.controlling = null;
      this.confirmingStop = false;
    }
  };

  requestStop = () => {
    this.controlNotice = '';
    this.controlError = null;
    this.confirmingStop = true;
  };

  cancelStop = () => {
    this.confirmingStop = false;
  };

  confirmStop = () => this.control('stop');

  requestDelete = () => {
    this.deleteError = null;
    this.deleteVolumes = false;
    this.confirmingDelete = true;
  };

  cancelDelete = () => {
    this.confirmingDelete = false;
  };

  confirmDelete = async () => {
    this.deleting = true;
    try {
      await this.api.deleteApp(this.app.slug, { volumes: this.deleteVolumes });
      this.router.transitionTo('index');
    } catch (error) {
      this.deleteError = error;
      this.confirmingDelete = false;
    } finally {
      this.deleting = false;
    }
  };

  <template>
    <form
      novalidate
      data-test-settings-form
      {{on "submit" (preventDefault this.save)}}
      {{on "input" this.touch}}
    >
      <div class="form-group">
        <div>
          <h2 class="form-group-title">ソース</h2>
          <p class="form-group-description">
            デプロイするコードと、その中の Compose ファイルです。
          </p>
        </div>
        <div class="form-group-body">
          <dl class="readout">
            <dt>リポジトリ</dt>
            <dd data-test-repository>
              {{#if (repositoryUrl this.app)}}
                <a
                  href={{repositoryUrl this.app}}
                  target="_blank"
                  rel="noopener noreferrer"
                >{{repositoryName this.app}}</a>
              {{else}}
                {{repositoryName this.app}}
              {{/if}}
            </dd>
            <dt>アプリ名</dt>
            <dd>
              <span class="mono">{{this.app.slug}}</span>
              <span class="readout-note">（変更できません）</span>
            </dd>
          </dl>

          <div class="form-grid">
            <FormField
              @label="既定のブランチ"
              @error={{this.errors.default_branch}}
              data-test-field-group="default_branch"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                spellcheck="false"
                autocomplete="off"
                value={{this.form.defaultBranch}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="default_branch"
                {{on "input" (setValue this.form "defaultBranch")}}
              />
            </FormField>

            <FormField
              class="field-wide"
              @label="Compose ファイルのパス"
              @hint="空なら compose.yaml などをリポジトリ直下から自動で探します。"
              @error={{this.errors.compose_path}}
              data-test-field-group="compose_path"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                class="mono"
                spellcheck="false"
                autocomplete="off"
                placeholder="自動検出"
                value={{this.form.composePath}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="compose_path"
                {{on "input" (setValue this.form "composePath")}}
              />
            </FormField>
          </div>
        </div>
      </div>

      <div class="form-group">
        <div>
          <h2 class="form-group-title">公開</h2>
          <p class="form-group-description">
            外からのリクエストを受けるサービスと、デプロイ後に応答を確かめるパスです。
          </p>
        </div>
        <div class="form-group-body">
          <div class="form-grid">
            <FormField
              @label="公開するサービス"
              @error={{this.errors.web_service}}
              data-test-field-group="web_service"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                spellcheck="false"
                autocomplete="off"
                value={{this.form.webService}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="web_service"
                {{on "input" (setValue this.form "webService")}}
              />
            </FormField>

            <FormField
              @label="コンテナ側のポート"
              @error={{this.errors.web_port}}
              data-test-field-group="web_port"
              as |field|
            >
              <input
                id={{field.id}}
                type="number"
                class="mono"
                inputmode="numeric"
                min="1"
                max="65535"
                value={{this.form.webPort}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="web_port"
                {{on "input" (setValue this.form "webPort")}}
              />
            </FormField>

            <FormField
              @label="ヘルスチェックのパス"
              @error={{this.errors.healthcheck_path}}
              data-test-field-group="healthcheck_path"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                class="mono"
                spellcheck="false"
                autocomplete="off"
                value={{this.form.healthcheckPath}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="healthcheck_path"
                {{on "input" (setValue this.form "healthcheckPath")}}
              />
            </FormField>
          </div>

          <dl class="readout">
            <dt>ホスト側のポート</dt>
            <dd>
              <span
                class="mono"
                data-test-host-port
              >127.0.0.1:{{this.app.host_port}}</span>
              <span class="readout-note">（自動で割り当て済み）</span>
            </dd>
          </dl>
        </div>
      </div>

      <div class="form-group">
        <div>
          <h2 class="form-group-title">環境</h2>
          <p class="form-group-description">
            本番とステージングを取り違えないための目印です。アプリの動作は変わりません。
          </p>
        </div>
        <div class="form-group-body">
          <fieldset class="option-group">
            <legend class="visually-hidden">環境</legend>
            <label class="option">
              <input
                type="radio"
                name="environment"
                value="staging"
                checked={{isChecked this.form.environment "staging"}}
                disabled={{this.inGroup}}
                data-test-environment-option="staging"
                {{on "change" this.setEnvironment}}
              />
              <span class="option-body">
                <span class="option-label">ステージング</span>
                <span
                  class="option-hint"
                >デプロイは確認なしで始まります。</span>
              </span>
            </label>
            <label class="option">
              <input
                type="radio"
                name="environment"
                value="production"
                checked={{isChecked this.form.environment "production"}}
                disabled={{this.inGroup}}
                data-test-environment-option="production"
                {{on "change" this.setEnvironment}}
              />
              <span class="option-body">
                <span class="option-label">本番</span>
                <span class="option-hint">
                  バッジで目立たせ、デプロイの前に確認します。
                </span>
              </span>
            </label>
          </fieldset>
          {{#if this.errors.environment}}
            <p class="field-error" data-test-field-error="environment">
              {{this.errors.environment}}
            </p>
          {{/if}}
          {{#if this.inGroup}}
            <p class="option-hint" data-test-environment-group-note>
              グループに入っている間は、環境を変えられません。変えるときは、先に下の「グループ」で外してください。
            </p>
          {{/if}}
        </div>
      </div>

      <div class="form-group" data-test-env-source-group>
        <div>
          <h2 class="form-group-title">必要な環境変数の照合</h2>
          <p class="form-group-description">
            リポジトリが必要とする環境変数を読み、環境変数の画面とデプロイの前に、未設定のものを知らせます。デプロイは止めません。
          </p>
        </div>
        <div class="form-group-body">
          <fieldset class="option-group">
            <legend class="visually-hidden">照合元</legend>
            <label class="option">
              <input
                type="radio"
                name="env_source"
                value="compose"
                checked={{isSource this.form.envSource "compose"}}
                data-test-env-source-option="compose"
                {{on "change" this.setEnvSource}}
              />
              <span class="option-body">
                <span class="option-label">Compose ファイルの
                  <code>{{VAR_PLAIN}}</code>（推奨）</span>
                <span class="option-hint">
                  <code>{{VAR_REQUIRED}}</code>
                  と既定値のない参照は必須、<code>{{VAR_OPTIONAL}}</code>
                  は任意として扱います。アプリのコードだけが読む変数は対象外です。
                </span>
              </span>
            </label>
            <label class="option">
              <input
                type="radio"
                name="env_source"
                value="dotenv"
                checked={{isSource this.form.envSource "dotenv"}}
                data-test-env-source-option="dotenv"
                {{on "change" this.setEnvSource}}
              />
              <span class="option-body">
                <span class="option-label">.env.example</span>
                <span class="option-hint">
                  書かれたキーをすべて必須として扱い、直前のコメントを説明として表示します。
                </span>
              </span>
            </label>
            <label class="option">
              <input
                type="radio"
                name="env_source"
                value="off"
                checked={{isSource this.form.envSource "off"}}
                data-test-env-source-option="off"
                {{on "change" this.setEnvSource}}
              />
              <span class="option-body">
                <span class="option-label">照合しない</span>
              </span>
            </label>
          </fieldset>
          {{#if (isSource this.form.envSource "dotenv")}}
            <FormField
              class="field-wide"
              @label=".env.example のパス"
              @hint="空ならリポジトリ直下の .env.example を読みます。"
              @error={{this.errors.env_example_path}}
              data-test-field-group="env_example_path"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                class="mono"
                spellcheck="false"
                autocomplete="off"
                placeholder=".env.example"
                value={{this.form.envExamplePath}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="env_example_path"
                {{on "input" (setValue this.form "envExamplePath")}}
              />
            </FormField>
          {{/if}}
          {{#if this.errors.env_source}}
            <p class="field-error">{{this.errors.env_source}}</p>
          {{/if}}
        </div>
      </div>

      <div class="form-group">
        <div>
          <h2 class="form-group-title">メモリ上限</h2>
          <p class="form-group-description">
            サービスごとの上限です。数字に単位 b・k・m・g
            のどれかを付けて書きます（例: 512m、1g）。
          </p>
        </div>
        <div class="form-group-body">
          <FormField
            class="field-narrow"
            @label="既定（全サービス共通）"
            @hint="空なら基盤の既定値を使います。"
            @error={{this.errors.memory_limit}}
            data-test-field-group="memory_limit"
            as |field|
          >
            <input
              id={{field.id}}
              type="text"
              spellcheck="false"
              autocomplete="off"
              placeholder="基盤の既定値"
              value={{this.form.memoryLimit}}
              aria-describedby={{field.describedBy}}
              aria-invalid={{field.invalid}}
              data-test-field="memory_limit"
              {{on "input" (setValue this.form "memoryLimit")}}
            />
          </FormField>

          <div
            class="field"
            role="group"
            aria-labelledby="settings-limits-label"
            data-test-field-group="service_memory_limits"
          >
            <span class="field-label" id="settings-limits-label">
              サービス別
            </span>
            {{#if this.form.limits.length}}
              <ul class="limit-rows">
                {{#each this.form.limits key="id" as |row|}}
                  <li class="limit-row" data-test-limit-row>
                    <input
                      type="text"
                      placeholder="サービス名"
                      spellcheck="false"
                      autocomplete="off"
                      aria-label="サービス名"
                      value={{row.service}}
                      data-test-limit-service
                      {{on "input" (setValue row "service")}}
                    />
                    <input
                      type="text"
                      placeholder="1g"
                      spellcheck="false"
                      autocomplete="off"
                      aria-label="{{if
                        row.service
                        row.service
                        'サービス'
                      }} の上限"
                      aria-invalid={{if
                        this.errors.service_memory_limits
                        "true"
                      }}
                      value={{row.limit}}
                      data-test-limit-value
                      {{on "input" (setValue row "limit")}}
                    />
                    <Button
                      @size="small"
                      aria-label="{{if
                        row.service
                        row.service
                        'この行'
                      }} の上限を外す"
                      data-test-limit-remove
                      {{on "click" (fn this.removeLimit row)}}
                    >外す</Button>
                  </li>
                {{/each}}
              </ul>
            {{/if}}
            <div>
              <Button
                @size="small"
                data-test-limit-add
                {{on "click" this.addLimit}}
              >サービス別の上限を追加</Button>
            </div>
            {{#if this.errors.service_memory_limits}}
              <p class="field-error" data-test-field-error>
                {{this.errors.service_memory_limits}}
              </p>
            {{/if}}
          </div>
        </div>
      </div>

      <div class="form-group">
        <div>
          <h2 class="form-group-title">デプロイの安全装置</h2>
          <p class="form-group-description">
            危険な構成や、起動しないリリースからアプリを守るしくみです。
          </p>
        </div>
        <div class="form-group-body">
          <dl class="readout">
            <dt>自動ロールバック</dt>
            <dd data-test-auto-rollback>
              有効
              <span class="readout-note">
                （起動やヘルスチェックに失敗したら、直前に成功したリリースへ自動で戻します。常に有効です）
              </span>
            </dd>
          </dl>

          <label class="option">
            <input
              type="checkbox"
              checked={{this.form.relaxPolicy}}
              aria-describedby="settings-relax-hint"
              data-test-field="relax_policy"
              {{on "change" (setChecked this.form "relaxPolicy")}}
            />
            <span class="option-body">
              <span class="option-label">Compose のポリシー検査を緩める</span>
              <span class="option-hint" id="settings-relax-hint">
                特権コンテナ、ホストのネットワーク、Docker
                ソケットやリリース外の bind mount
                など、通常は拒否する指定を通します。項目ごとには選べません。
              </span>
            </span>
          </label>
          {{#if this.form.relaxPolicy}}
            <Banner
              @tone="warning"
              @title="このアプリには保護の多くが効きません"
              data-test-relax-warning
            >
              緩めたアプリのコンテナは、ホストを root
              相当で操作できる構成でも起動します。中身を自分で確かめたリポジトリにだけ使ってください。ポートの公開に関する検査は緩みません。
            </Banner>
          {{/if}}
        </div>
      </div>

      <div aria-live="polite">
        <ErrorMessage @error={{this.error}} />
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
          data-test-settings-save
        >保存</Button>
        <p class="form-status" aria-live="polite">
          {{#if this.saved}}
            <span class="form-saved" data-test-settings-saved>
              <Icon @name="check" />
              保存しました。Compose
              やメモリ上限の変更は、次のデプロイから反映されます。
            </span>
          {{else if this.nothingToSave}}
            <span data-test-settings-unchanged>変更はありません。</span>
          {{else}}
            Compose やメモリ上限の変更は、次のデプロイから反映されます。
          {{/if}}
        </p>
      </div>
    </form>

    <AppGroupSettings @model={{@model}} />

    <section
      class="pane settings-operations"
      aria-labelledby="operations-title"
    >
      <div class="pane-header">
        <h2 class="pane-title" id="operations-title">起動と再起動</h2>
        <StatusIndicator
          @kind={{this.state.kind}}
          @label={{this.state.label}}
          title={{this.state.hint}}
          data-test-settings-state
        />
      </div>

      <div class="pane-notices" aria-live="polite">
        {{#if this.controlNotice}}
          <Banner data-test-notice>{{this.controlNotice}}</Banner>
        {{/if}}
        {{#if this.controlError}}
          <ErrorMessage @error={{this.controlError}} />
          {{#if this.violations.length}}
            <ul class="form-problems" data-test-violations>
              {{#each this.violations as |line|}}
                <li>{{line}}</li>
              {{/each}}
            </ul>
          {{/if}}
        {{/if}}
      </div>

      <ul class="action-rows">
        {{#if this.stopped}}
          <li class="action-row">
            <div class="action-row-text">
              <p class="action-row-title">起動</p>
              <p class="action-row-description">
                稼働中だったリリースでコンテナを起動します。
              </p>
            </div>
            <Button
              @disabled={{this.controlsDisabled}}
              @busy={{isBusy this.controlling "start"}}
              @busyLabel="起動中…"
              data-test-control="start"
              {{on "click" (fn this.control "start")}}
            >起動</Button>
          </li>
        {{else}}
          <li class="action-row">
            <div class="action-row-text">
              <p class="action-row-title">再起動</p>
              <p class="action-row-description">
                {{#if this.neverDeployed}}
                  まだデプロイされていないため、起動や停止はできません。
                {{else}}
                  いまのリリースのままコンテナを再起動します。数秒のあいだ応答が止まります。
                {{/if}}
              </p>
            </div>
            <Button
              @disabled={{this.controlsDisabled}}
              @busy={{isBusy this.controlling "restart"}}
              @busyLabel="再起動中…"
              data-test-control="restart"
              {{on "click" (fn this.control "restart")}}
            >再起動</Button>
          </li>
        {{/if}}
      </ul>
    </section>

    <section
      class="danger-zone"
      aria-labelledby="danger-title"
      data-test-danger-zone
    >
      <h2 class="danger-zone-title" id="danger-title">
        <Icon @name="warning" />
        危険な操作
      </h2>

      <div class="pane-notices" aria-live="polite">
        <ErrorMessage @error={{this.deleteError}} />
      </div>

      <ul class="action-rows">
        <li class="action-row">
          <div class="action-row-text">
            <p class="action-row-title">アプリを停止</p>
            <p class="action-row-description">
              {{#if this.stopped}}
                すでに停止しています。
              {{else}}
                コンテナを止めます。起動するまで、アクセスには 503
                のページが返ります。データは消えません。
              {{/if}}
            </p>
          </div>
          <Button
            class="button-danger-outline"
            @disabled={{this.cannotStop}}
            data-test-control="stop"
            {{on "click" this.requestStop}}
          >停止…</Button>
        </li>
        <li class="action-row">
          <div class="action-row-text">
            <p class="action-row-title">アプリを削除</p>
            <p class="action-row-description">
              コンテナを止め、アプリとその設定、デプロイの履歴を削除します。元に戻せません。
            </p>
          </div>
          <Button
            @variant="danger"
            data-test-delete
            {{on "click" this.requestDelete}}
          >削除…</Button>
        </li>
      </ul>
    </section>

    {{#if this.confirmingStop}}
      <ConfirmDialog
        @title={{if
          @model.isProduction
          "本番アプリを停止しますか？"
          "アプリを停止しますか？"
        }}
        @confirmLabel="停止する"
        @danger={{true}}
        @busy={{isBusy this.controlling "stop"}}
        @onConfirm={{this.confirmStop}}
        @onCancel={{this.cancelStop}}
      >
        <p>
          {{if @model.isProduction "本番アプリ" "アプリ"}}
          <strong>{{this.app.slug}}</strong>
          のコンテナを止めます。起動するまで、アクセスには 503
          のページが返ります。
        </p>
      </ConfirmDialog>
    {{/if}}

    {{#if this.confirmingDelete}}
      <ConfirmDialog
        @title={{if
          @model.isProduction
          "本番アプリを削除しますか？"
          "アプリを削除しますか？"
        }}
        @confirmLabel="削除する"
        @danger={{true}}
        @requireText={{this.app.slug}}
        @busy={{this.deleting}}
        @onConfirm={{this.confirmDelete}}
        @onCancel={{this.cancelDelete}}
      >
        <p>
          {{if @model.isProduction "本番アプリ" "アプリ"}}
          <strong>{{this.app.slug}}</strong>
          を削除します。元に戻せません。
        </p>
        <label class="option dialog-option">
          <input
            type="checkbox"
            checked={{this.deleteVolumes}}
            disabled={{this.deleting}}
            data-test-delete-volumes
            {{on "change" (setChecked this "deleteVolumes")}}
          />
          <span class="option-body">
            <span class="option-label">volume も削除する</span>
            <span class="option-hint">
              DB やアップロードファイルなど、アプリのデータが消えます。
            </span>
          </span>
        </label>
        <p aria-live="polite">
          {{#if this.deleteVolumes}}
            <strong data-test-delete-volumes-warning>
              volume も削除します。データは残りません。
            </strong>
          {{else}}
            volume は残します。同じ名前でアプリを作り直すと、残した volume
            がそのまま使われます。
          {{/if}}
        </p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
