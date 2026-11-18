import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn } from '@ember/helper';
import type Owner from '@ember/owner';
import type RouterService from '@ember/routing/router-service';
import ConfirmDialog from 'paas-console/components/confirm-dialog';
import ErrorMessage from 'paas-console/components/error-message';
import type ApiService from 'paas-console/services/api';
import type AppContext from 'paas-console/utils/app-context';
import {
  inputValue,
  preventDefault,
  setChecked,
  setValue,
} from 'paas-console/utils/dom';
import { fieldErrors } from 'paas-console/utils/errors';
import SettingsForm, { LimitRow } from 'paas-console/utils/settings-form';
import type { App, Environment } from 'paas-console/utils/types';

const KNOWN_FIELDS = new Set([
  'default_branch',
  'compose_path',
  'web_service',
  'web_port',
  'healthcheck_path',
  'environment',
  'memory_limit',
  'service_memory_limits',
  'backup_exclude_volumes',
  'dump_hook_service',
  'dump_hook_command',
]);

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

  get otherErrors(): string[] {
    return Object.entries(this.errors)
      .filter(([name]) => !KNOWN_FIELDS.has(name))
      .map(([name, reason]) => `${name}: ${reason}`);
  }

  setEnvironment = (event: Event) => {
    this.form.environment = inputValue(event) as Environment;
  };

  addLimit = () => {
    this.form.limits = [...this.form.limits, new LimitRow()];
  };

  removeLimit = (row: LimitRow) => {
    this.form.limits = this.form.limits.filter((r) => r !== row);
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

  requestDelete = () => {
    this.deleteError = null;
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
      class="card form"
      novalidate
      data-test-settings-form
      {{on "submit" (preventDefault this.save)}}
      {{on "input" this.touch}}
    >
      <h3>設定</h3>
      <p class="notice">変更は<strong
        >次のデプロイから</strong>反映されます。</p>

      <fieldset class="form-section">
        <legend>ソース</legend>
        <dl class="definition">
          <dt>slug</dt>
          <dd><code>{{this.app.slug}}</code>
            <span class="muted">（変更できません）</span></dd>
          <dt>リポジトリ</dt>
          <dd>{{if
              this.app.repo_full_name
              this.app.repo_full_name
              this.app.repo_url
            }}</dd>
        </dl>

        <div class="field-row">
          <label class="field" for="settings-branch">
            <span class="field-label">既定のブランチ</span>
            <input
              id="settings-branch"
              type="text"
              spellcheck="false"
              value={{this.form.defaultBranch}}
              aria-invalid={{if this.errors.default_branch "true"}}
              data-test-field="default_branch"
              {{on "input" (setValue this.form "defaultBranch")}}
            />
            {{#if this.errors.default_branch}}
              <span class="field-error" data-test-field-error="default_branch">
                {{this.errors.default_branch}}
              </span>
            {{/if}}
          </label>

          <label class="field field-grow" for="settings-compose-path">
            <span class="field-label">Compose ファイルのパス</span>
            <input
              id="settings-compose-path"
              type="text"
              spellcheck="false"
              placeholder="自動検出"
              value={{this.form.composePath}}
              aria-invalid={{if this.errors.compose_path "true"}}
              data-test-field="compose_path"
              {{on "input" (setValue this.form "composePath")}}
            />
            <span class="field-hint">空なら
              <code>compose.yaml</code>
              などを自動で探します。</span>
            {{#if this.errors.compose_path}}
              <span class="field-error" data-test-field-error="compose_path">
                {{this.errors.compose_path}}
              </span>
            {{/if}}
          </label>
        </div>
      </fieldset>

      <fieldset class="form-section">
        <legend>公開</legend>
        <div class="field-row">
          <label class="field" for="settings-web-service">
            <span class="field-label">公開するサービス</span>
            <input
              id="settings-web-service"
              type="text"
              spellcheck="false"
              value={{this.form.webService}}
              aria-invalid={{if this.errors.web_service "true"}}
              data-test-field="web_service"
              {{on "input" (setValue this.form "webService")}}
            />
            {{#if this.errors.web_service}}
              <span class="field-error" data-test-field-error="web_service">
                {{this.errors.web_service}}
              </span>
            {{/if}}
          </label>

          <label class="field" for="settings-web-port">
            <span class="field-label">コンテナ側のポート</span>
            <input
              id="settings-web-port"
              type="number"
              inputmode="numeric"
              min="1"
              max="65535"
              value={{this.form.webPort}}
              aria-invalid={{if this.errors.web_port "true"}}
              data-test-field="web_port"
              {{on "input" (setValue this.form "webPort")}}
            />
            {{#if this.errors.web_port}}
              <span class="field-error" data-test-field-error="web_port">
                {{this.errors.web_port}}
              </span>
            {{/if}}
          </label>

          <label class="field" for="settings-healthcheck">
            <span class="field-label">ヘルスチェックのパス</span>
            <input
              id="settings-healthcheck"
              type="text"
              spellcheck="false"
              value={{this.form.healthcheckPath}}
              aria-invalid={{if this.errors.healthcheck_path "true"}}
              data-test-field="healthcheck_path"
              {{on "input" (setValue this.form "healthcheckPath")}}
            />
            {{#if this.errors.healthcheck_path}}
              <span
                class="field-error"
                data-test-field-error="healthcheck_path"
              >
                {{this.errors.healthcheck_path}}
              </span>
            {{/if}}
          </label>
        </div>

        <div
          class="field"
          role="radiogroup"
          aria-labelledby="settings-env-label"
        >
          <span class="field-label" id="settings-env-label">環境</span>
          <label class="choice">
            <input
              type="radio"
              name="environment"
              value="staging"
              checked={{eq this.form.environment "staging"}}
              data-test-environment-option="staging"
              {{on "change" this.setEnvironment}}
            />
            ステージング
          </label>
          <label class="choice">
            <input
              type="radio"
              name="environment"
              value="production"
              checked={{eq this.form.environment "production"}}
              data-test-environment-option="production"
              {{on "change" this.setEnvironment}}
            />
            本番（デプロイと削除の前に確認します）
          </label>
        </div>
      </fieldset>

      <fieldset class="form-section">
        <legend>制限</legend>
        <label class="field" for="settings-memory">
          <span class="field-label">メモリ上限（全サービス共通）</span>
          <input
            id="settings-memory"
            type="text"
            spellcheck="false"
            placeholder="基盤の既定値"
            value={{this.form.memoryLimit}}
            aria-invalid={{if this.errors.memory_limit "true"}}
            data-test-field="memory_limit"
            {{on "input" (setValue this.form "memoryLimit")}}
          />
          <span class="field-hint">数字と単位
            <code>b</code>
            <code>k</code>
            <code>m</code>
            <code>g</code>（例
            <code>512m</code>）。空なら基盤の既定値を使います。</span>
          {{#if this.errors.memory_limit}}
            <span class="field-error" data-test-field-error="memory_limit">
              {{this.errors.memory_limit}}
            </span>
          {{/if}}
        </label>

        <div class="field">
          <span class="field-label">サービス別のメモリ上限</span>
          {{#if this.form.limits.length}}
            <ul class="limit-list">
              {{#each this.form.limits key="id" as |row|}}
                <li class="limit-row" data-test-limit-row>
                  <label class="field" for="{{row.id}}-service">
                    <span class="visually-hidden">サービス名</span>
                    <input
                      id="{{row.id}}-service"
                      type="text"
                      placeholder="db"
                      spellcheck="false"
                      value={{row.service}}
                      data-test-limit-service
                      {{on "input" (setValue row "service")}}
                    />
                  </label>
                  <label class="field" for="{{row.id}}-limit">
                    <span class="visually-hidden">上限</span>
                    <input
                      id="{{row.id}}-limit"
                      type="text"
                      placeholder="1g"
                      spellcheck="false"
                      value={{row.limit}}
                      data-test-limit-value
                      {{on "input" (setValue row "limit")}}
                    />
                  </label>
                  <button
                    type="button"
                    class="button button-small"
                    data-test-limit-remove
                    {{on "click" (fn this.removeLimit row)}}
                  >削除</button>
                </li>
              {{/each}}
            </ul>
          {{/if}}
          <div>
            <button
              type="button"
              class="button button-small"
              data-test-limit-add
              {{on "click" this.addLimit}}
            >サービス別の上限を追加</button>
          </div>
          {{#if this.errors.service_memory_limits}}
            <span
              class="field-error"
              data-test-field-error="service_memory_limits"
            >{{this.errors.service_memory_limits}}</span>
          {{/if}}
        </div>

        <label class="choice">
          <input
            type="checkbox"
            checked={{this.form.relaxPolicy}}
            data-test-field="relax_policy"
            {{on "change" (setChecked this.form "relaxPolicy")}}
          />
          Compose のポリシー検査を緩める
        </label>
        <span class="field-hint">特権コンテナ、ホストのネットワーク、Docker
          ソケットやリリース外の bind mount
          など、通常は拒否する指定を許します。ポートの公開に関する検査は緩みません。</span>
      </fieldset>

      <fieldset class="form-section" data-test-backup-settings>
        <legend>バックアップ</legend>
        {{#if this.form.hasBackupFields}}
          <label class="field" for="settings-exclude-volumes">
            <span class="field-label">バックアップから外す volume</span>
            <input
              id="settings-exclude-volumes"
              type="text"
              spellcheck="false"
              placeholder="pgdata, cache"
              value={{this.form.excludeVolumes}}
              aria-invalid={{if this.errors.backup_exclude_volumes "true"}}
              data-test-field="backup_exclude_volumes"
              {{on "input" (setValue this.form "excludeVolumes")}}
            />
            <span class="field-hint">カンマ区切り。DB
              のデータディレクトリは稼働中のコピーでは整合しないので、ここで外してダンプフックで取ります。</span>
            {{#if this.errors.backup_exclude_volumes}}
              <span
                class="field-error"
                data-test-field-error="backup_exclude_volumes"
              >{{this.errors.backup_exclude_volumes}}</span>
            {{/if}}
          </label>

          <div class="field-row">
            <label class="field" for="settings-hook-service">
              <span class="field-label">ダンプフックのサービス</span>
              <input
                id="settings-hook-service"
                type="text"
                spellcheck="false"
                placeholder="db"
                value={{this.form.dumpHookService}}
                aria-invalid={{if this.errors.dump_hook_service "true"}}
                data-test-field="dump_hook_service"
                {{on "input" (setValue this.form "dumpHookService")}}
              />
              {{#if this.errors.dump_hook_service}}
                <span
                  class="field-error"
                  data-test-field-error="dump_hook_service"
                >{{this.errors.dump_hook_service}}</span>
              {{/if}}
            </label>

            <label class="field field-grow" for="settings-hook-command">
              <span class="field-label">ダンプフックのコマンド</span>
              <input
                id="settings-hook-command"
                type="text"
                class="mono"
                spellcheck="false"
                placeholder="pg_dump -U postgres app"
                value={{this.form.dumpHookCommand}}
                aria-invalid={{if this.errors.dump_hook_command "true"}}
                data-test-field="dump_hook_command"
                {{on "input" (setValue this.form "dumpHookCommand")}}
              />
              <span
                class="field-hint"
              >そのサービスのコンテナで実行し、標準出力をバックアップします。</span>
              {{#if this.errors.dump_hook_command}}
                <span
                  class="field-error"
                  data-test-field-error="dump_hook_command"
                >{{this.errors.dump_hook_command}}</span>
              {{/if}}
            </label>
          </div>

          <label class="choice">
            <input
              type="checkbox"
              checked={{this.form.backupBeforeDeploy}}
              data-test-field="backup_before_deploy"
              {{on "change" (setChecked this.form "backupBeforeDeploy")}}
            />
            デプロイの前にバックアップを取る
          </label>
        {{else}}
          <p
            class="muted"
            data-test-backup-settings-unavailable
          >バックアップの設定はまだ利用できません。</p>
        {{/if}}
      </fieldset>

      <div aria-live="polite">
        {{#if this.saved}}
          <p
            class="notice notice-ok"
            data-test-settings-saved
          >保存しました。次のデプロイから反映されます。</p>
        {{else if this.nothingToSave}}
          <p class="muted" data-test-settings-unchanged>変更はありません。</p>
        {{/if}}
        <ErrorMessage @error={{this.error}} />
        {{#if this.otherErrors.length}}
          <ul class="field-error" data-test-other-errors>
            {{#each this.otherErrors as |line|}}
              <li>{{line}}</li>
            {{/each}}
          </ul>
        {{/if}}
      </div>

      <div class="form-actions">
        <button
          type="submit"
          class="button button-primary"
          disabled={{this.saving}}
          data-test-settings-save
        >{{if this.saving "保存中…" "保存する"}}</button>
      </div>
    </form>

    <section class="card card-danger" aria-labelledby="delete-title">
      <h3 id="delete-title">アプリの削除</h3>
      <p>コンテナを止めてアプリを削除します。元に戻せません。</p>
      <label class="choice">
        <input
          type="checkbox"
          checked={{this.deleteVolumes}}
          data-test-delete-volumes
          {{on "change" (setChecked this "deleteVolumes")}}
        />
        volume も削除する（DB やアップロードファイルなどのデータが消えます）
      </label>
      <div aria-live="polite">
        <ErrorMessage @error={{this.deleteError}} />
      </div>
      <div class="form-actions">
        <button
          type="button"
          class="button button-danger"
          data-test-delete
          {{on "click" this.requestDelete}}
        >このアプリを削除</button>
      </div>
    </section>

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
        <p>{{if @model.isProduction "本番アプリ" "アプリ"}}
          <strong>{{this.app.slug}}</strong>
          を削除します。元に戻せません。</p>
        {{#if this.deleteVolumes}}
          <p class="alert alert-danger" data-test-delete-volumes-warning>
            volume も削除します。データは残りません。
          </p>
        {{else}}
          <p class="muted">volume は残します。</p>
        {{/if}}
      </ConfirmDialog>
    {{/if}}
  </template>
}
