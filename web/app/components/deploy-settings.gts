import Component from '@glimmer/component';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq } from '@ember/helper';
import type Owner from '@ember/owner';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import CopyButton from 'koroku-console/components/copy-button';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import RoleNote from 'koroku-console/components/role-note';
import SettingsConfirm from 'koroku-console/components/settings-confirm';
import focusSection from 'koroku-console/modifiers/focus-section';
import type ApiService from 'koroku-console/services/api';
import {
  inputValue,
  preventDefault,
  setChecked,
  setValue,
} from 'koroku-console/utils/dom';
import Loadable from 'koroku-console/utils/loadable';
import { can } from 'koroku-console/utils/roles';
import type SettingsEditor from 'koroku-console/utils/settings-editor';
import { DEPLOY_SETTINGS_FIELDS } from 'koroku-console/utils/settings-editor';
import type { App, SystemInfo } from 'koroku-console/utils/types';

const KNOWN_FIELDS = new Set<string>(DEPLOY_SETTINGS_FIELDS);

/** Pushes reach koroku through the GitHub App (ARCHITECTURE 5.2). */
function usesGitHubApp(app: App): boolean {
  return app.repo_full_name !== '' && !app.repo_url;
}

/** Whether two apps deploy from the same repository. */
function sameRepository(a: App, b: App): boolean {
  const repo = (app: App) =>
    app.repo_full_name
      ? `https://github.com/${app.repo_full_name}.git`.toLowerCase()
      : app.repo_url.toLowerCase();
  return repo(a) !== '' && repo(a) === repo(b);
}

interface Signature {
  Element: HTMLElement;
  Args: {
    editor: SettingsEditor;
    /** The section a link led to (`?section=`), or ''. */
    section: string;
  };
}

/**
 * 「デプロイの設定」 on the deploy tab (ARCHITECTURE 9.1): whether pushes
 * deploy by themselves (5.2), which staging app a production app promotes
 * from (9.7), and the safeguards of a deployment (6, 7). One form with its
 * own save; the tab asks before deploying while it has unsaved changes.
 */
export default class DeploySettings extends Component<Signature> {
  @service declare api: ApiService;

  /** The webhook URL and whether its secret is set (ARCHITECTURE 5.2). */
  system = new Loadable<SystemInfo>(() => this.api.getSystem());

  /** The other apps, for the promotion source (ARCHITECTURE 9.7). */
  apps = new Loadable<App[]>(() => this.api.listApps());

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.system.load();
    if (this.isProduction || this.app.promote_from) void this.apps.load();
  }

  get editor(): SettingsEditor {
    return this.args.editor;
  }

  get app(): App {
    return this.editor.app;
  }

  focuses = (id: string): boolean => this.args.section === id;

  get isProduction(): boolean {
    return this.app.environment === 'production';
  }

  /** The settings need manage; some of them the owner (ARCHITECTURE 10.7). */
  get readOnly(): boolean {
    return !can(this.app, 'manage');
  }

  get isOwner(): boolean {
    return can(this.app, 'owner');
  }

  /** Manages the app but is not the owner: some fields stay read-only. */
  get memberManages(): boolean {
    return !this.readOnly && !this.isOwner;
  }

  get ownerOnlyText(): string {
    return this.isProduction
      ? '本番アプリの自動デプロイ、ポリシー検査と昇格元'
      : 'ポリシー検査と昇格元';
  }

  get canAutoDeploy(): boolean {
    return usesGitHubApp(this.app);
  }

  /** Off and impossible, or not this member's call: nothing to switch. */
  get autoDeployDisabled(): boolean {
    // Making a production app deploy pushes by itself is the owner's call
    // (ARCHITECTURE 10.7).
    if (this.isProduction && !this.isOwner) return true;
    return !this.canAutoDeploy && !this.editor.form.autoDeploy;
  }

  /** The branch fields follow the switch's owner-only rule too. */
  get autoDeployFieldsDisabled(): boolean {
    return this.isProduction && !this.isOwner;
  }

  /** The form names a workflow to wait for before deploying a push. */
  get waitsForWorkflow(): boolean {
    return this.editor.form.autoDeployWorkflow.trim() !== '';
  }

  get webhookSecretMissing(): boolean {
    return this.system.value?.webhook?.secret_configured === false;
  }

  /** Staging apps of the same repository, which this app can promote from. */
  get promotionSources(): App[] {
    return (this.apps.value ?? []).filter(
      (other) =>
        other.slug !== this.app.slug &&
        other.environment === 'staging' &&
        sameRepository(other, this.app),
    );
  }

  /** The saved source, kept as an option even when it no longer fits. */
  get savedSourceMissing(): boolean {
    const saved = this.app.promote_from ?? '';
    return saved !== '' && !this.promotionSources.some((a) => a.slug === saved);
  }

  get otherErrors(): string[] {
    return this.editor.otherErrors(KNOWN_FIELDS);
  }

  setPromoteFrom = (event: Event) => {
    this.editor.form.promoteFrom = inputValue(event);
    this.editor.touch();
  };

  save = async () => {
    await this.editor.save();
  };

  <template>
    <section
      class="pane deploy-settings"
      id="deploy-settings"
      tabindex="-1"
      aria-labelledby="deploy-settings-title"
      data-test-deploy-settings
      {{focusSection (this.focuses "deploy-settings")}}
      ...attributes
    >
      <header class="pane-header">
        <h2 class="pane-title" id="deploy-settings-title">デプロイの設定</h2>
      </header>
      <p class="pane-description">
        push
        で自動でデプロイするか、何を本番へ昇格するか、どのリリースを通すかの設定です。
      </p>
      {{#if this.readOnly}}
        <RoleNote @needed="manage" @what="デプロイの設定の変更" />
      {{else if this.memberManages}}
        <RoleNote
          @needed="owner"
          @what={{this.ownerOnlyText}}
          data-test-owner-only-note
        />
      {{/if}}

      <form
        novalidate
        aria-label="デプロイの設定"
        data-test-deploy-settings-form
        {{on "submit" (preventDefault this.save)}}
        {{on "input" @editor.touch}}
      >
        <fieldset class="role-fieldset" disabled={{this.readOnly}}>
          <div
            class="form-group auto-deploy-group"
            data-test-auto-deploy-group
            id="auto-deploy"
            tabindex="-1"
            {{focusSection (this.focuses "auto-deploy")}}
          >
            <div>
              <h3
                class="form-group-title"
                id="auto-deploy-title"
              >自動デプロイ</h3>
              <p class="form-group-description">
                GitHub にブランチが push
                されたら、そのコミットを自動でデプロイします。結果は手動のデプロイと同じく履歴に残ります。
              </p>
            </div>
            <div class="form-group-body">
              <label class="option">
                <input
                  type="checkbox"
                  checked={{@editor.form.autoDeploy}}
                  disabled={{this.autoDeployDisabled}}
                  aria-describedby="auto-deploy-hint"
                  data-test-field="auto_deploy"
                  {{on "change" (setChecked @editor.form "autoDeploy")}}
                />
                <span class="option-body">
                  <span class="option-label">push されたら自動でデプロイする</span>
                  <span class="option-hint" id="auto-deploy-hint">
                    {{#if this.canAutoDeploy}}
                      続けて push
                      したときは、待っているデプロイを新しいコミットに置き換えます。停止中のアプリはデプロイしません。
                    {{else}}
                      GitHub App
                      で取得するリポジトリ（作成画面の一覧から選んだもの）だけで使えます。URL
                      で指定したリポジトリには push が届きません。
                    {{/if}}
                  </span>
                </span>
              </label>

              {{#if @editor.form.autoDeploy}}
                <FormField
                  class="field-narrow"
                  @label="デプロイするブランチ"
                  @hint="空なら既定のブランチ（{{@editor.form.defaultBranch}}）に従います。"
                  @error={{@editor.errors.auto_deploy_branch}}
                  data-test-field-group="auto_deploy_branch"
                  as |field|
                >
                  <input
                    id={{field.id}}
                    type="text"
                    spellcheck="false"
                    autocomplete="off"
                    placeholder={{@editor.form.defaultBranch}}
                    value={{@editor.form.autoDeployBranch}}
                    disabled={{this.autoDeployFieldsDisabled}}
                    aria-describedby={{field.describedBy}}
                    aria-invalid={{field.invalid}}
                    data-test-field="auto_deploy_branch"
                    {{on "input" (setValue @editor.form "autoDeployBranch")}}
                  />
                </FormField>

                <FormField
                  @label="先に待つワークフロー（任意）"
                  @hint="GitHub Actions でイメージを作るなら、そのワークフローのファイルを入れます。push ではなく、そのワークフローがこのブランチの最新のコミットで成功したときにデプロイします。空なら push の直後にデプロイします。"
                  @error={{@editor.errors.auto_deploy_workflow}}
                  data-test-field-group="auto_deploy_workflow"
                  as |field|
                >
                  <input
                    id={{field.id}}
                    type="text"
                    class="mono"
                    spellcheck="false"
                    autocomplete="off"
                    placeholder=".github/workflows/image.yml"
                    value={{@editor.form.autoDeployWorkflow}}
                    disabled={{this.autoDeployFieldsDisabled}}
                    aria-describedby={{field.describedBy}}
                    aria-invalid={{field.invalid}}
                    data-test-field="auto_deploy_workflow"
                    {{on "input" (setValue @editor.form "autoDeployWorkflow")}}
                  />
                </FormField>

                {{#if this.webhookSecretMissing}}
                  <Banner
                    @tone="warning"
                    @title="Webhook の secret が設定されていません"
                    data-test-webhook-secret-missing
                  >
                    <code>config.toml</code>
                    の
                    <code>[github] webhook_secret</code>
                    を設定して koroku を再起動するまで、push
                    は届いても受け付けません。
                  </Banner>
                {{/if}}

                <div class="auto-deploy-setup" data-test-webhook-setup>
                  <p class="field-hint">
                    GitHub App の設定で、次の
                    {{if this.waitsForWorkflow "4" "3"}}
                    つが必要です（App ごとに一度だけ）。
                  </p>
                  <ol class="auto-deploy-steps">
                    <li>
                      Webhook を
                      <strong>Active</strong>
                      にし、Webhook URL に次のアドレスを入れる。
                      {{#if this.system.value.webhook}}
                        <span class="auto-deploy-url">
                          <code
                            data-test-webhook-url
                          >{{this.system.value.webhook.url}}</code>
                          <CopyButton
                            @text={{this.system.value.webhook.url}}
                            @label="Webhook URL"
                          />
                        </span>
                      {{/if}}
                    </li>
                    <li>
                      Webhook secret に、<code>config.toml</code>
                      の
                      <code>[github] webhook_secret</code>
                      と同じ値を入れる。
                    </li>
                    <li>
                      Permissions &amp; events の Subscribe to events で
                      <strong>Push</strong>
                      を選ぶ。
                    </li>
                    {{#if this.waitsForWorkflow}}
                      <li data-test-workflow-setup>
                        Repository permissions の
                        <strong>Actions</strong>
                        を Read-only にし、Subscribe to events で
                        <strong>Workflow run</strong>
                        も選ぶ（権限を変えたら、インストール先で承認する）。
                      </li>
                    {{/if}}
                  </ol>
                </div>
              {{/if}}
              {{#if @editor.errors.auto_deploy}}
                <p class="field-error" data-test-field-error>
                  {{@editor.errors.auto_deploy}}
                </p>
              {{/if}}
            </div>
          </div>

          {{#if this.isProduction}}
            <div
              class="form-group"
              data-test-promotion-group
              id="promotion"
              tabindex="-1"
              {{focusSection (this.focuses "promotion")}}
            >
              <div>
                <h3 class="form-group-title" id="promotion-title">昇格元</h3>
                <p class="form-group-description">
                  同じリポジトリのステージングアプリを選ぶと、このタブの上の欄から、そのアプリで稼働中のリリースを本番へ昇格できます。ビルド引数が同じなら、ビルドしたイメージも再利用します。
                </p>
              </div>
              <div class="form-group-body">
                <FormField
                  class="field-wide"
                  @label="昇格元のステージングアプリ"
                  @hint={{if
                    this.apps.error
                    "アプリの一覧を取得できませんでした。"
                    ""
                  }}
                  @error={{@editor.errors.promote_from}}
                  data-test-field-group="promote_from"
                  as |field|
                >
                  <select
                    id={{field.id}}
                    aria-describedby={{field.describedBy}}
                    aria-invalid={{field.invalid}}
                    disabled={{if this.isOwner false true}}
                    data-test-field="promote_from"
                    {{on "change" this.setPromoteFrom}}
                  >
                    <option
                      value=""
                      selected={{eq @editor.form.promoteFrom ""}}
                    >
                      なし
                    </option>
                    {{#if this.savedSourceMissing}}
                      <option
                        value={{this.app.promote_from}}
                        selected={{eq
                          @editor.form.promoteFrom
                          this.app.promote_from
                        }}
                      >{{this.app.promote_from}}（条件に合いません）</option>
                    {{/if}}
                    {{#each this.promotionSources key="slug" as |other|}}
                      <option
                        value={{other.slug}}
                        selected={{eq @editor.form.promoteFrom other.slug}}
                      >{{other.slug}}</option>
                    {{/each}}
                  </select>
                </FormField>
                {{#if this.apps.loaded}}
                  {{#unless this.promotionSources.length}}
                    <p class="readout-note" data-test-no-promotion-sources>
                      同じリポジトリのステージングアプリがありません。
                    </p>
                  {{/unless}}
                {{/if}}
              </div>
            </div>
          {{/if}}

          <div
            class="form-group"
            id="safeguards"
            tabindex="-1"
            data-test-safeguards-group
            {{focusSection (this.focuses "safeguards")}}
          >
            <div>
              <h3
                class="form-group-title"
                id="safeguards-title"
              >デプロイの安全装置</h3>
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
                  checked={{@editor.form.relaxPolicy}}
                  aria-describedby="relax-hint"
                  disabled={{if this.isOwner false true}}
                  data-test-field="relax_policy"
                  {{on "change" (setChecked @editor.form "relaxPolicy")}}
                />
                <span class="option-body">
                  <span class="option-label">Compose のポリシー検査を緩める</span>
                  <span class="option-hint" id="relax-hint">
                    特権コンテナ、ホストのネットワーク、Docker
                    ソケットやリリース外の bind mount
                    など、通常は拒否する指定を通します。項目ごとには選べません。
                  </span>
                </span>
              </label>
              {{#if @editor.form.relaxPolicy}}
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
            <ErrorMessage @error={{@editor.error}} />
            {{#if this.otherErrors.length}}
              <ul class="form-problems" data-test-other-errors>
                {{#each this.otherErrors as |line|}}
                  <li>{{line}}</li>
                {{/each}}
              </ul>
            {{/if}}
          </div>

          {{#unless this.readOnly}}
            <div class="form-footer">
              <Button
                @type="submit"
                @variant="primary"
                @busy={{@editor.saving}}
                @busyLabel="保存中…"
                data-test-deploy-settings-save
              >保存</Button>
              <p class="form-status" aria-live="polite">
                {{#if @editor.saved}}
                  <span class="form-saved" data-test-deploy-settings-saved>
                    <Icon @name="check" />
                    保存しました。ポリシー検査の変更は、次のデプロイから反映されます。
                  </span>
                {{else if @editor.nothingToSave}}
                  <span
                    data-test-deploy-settings-unchanged
                  >変更はありません。</span>
                {{else}}
                  この節の項目をまとめて保存します。
                {{/if}}
              </p>
            </div>
          {{/unless}}
        </fieldset>
      </form>
    </section>

    <SettingsConfirm @editor={{@editor}} />
  </template>
}
