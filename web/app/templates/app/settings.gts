import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn, hash } from '@ember/helper';
import type Owner from '@ember/owner';
import { LinkTo } from '@ember/routing';
import type RouterService from '@ember/routing/router-service';
import Banner from 'koroku-console/components/banner';
import Button, { buttonClass } from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import DomainList from 'koroku-console/components/domain-list';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import RoleNote from 'koroku-console/components/role-note';
import RegistryCredential from 'koroku-console/components/registry-credential';
import SettingsConfirm from 'koroku-console/components/settings-confirm';
import UnsavedSettingsGuard from 'koroku-console/components/unsaved-settings-guard';
import type AppSettingsController from 'koroku-console/controllers/app/settings';
import focusSection from 'koroku-console/modifiers/focus-section';
import followSections from 'koroku-console/modifiers/follow-sections';
import type ApiService from 'koroku-console/services/api';
import type AppContext from 'koroku-console/utils/app-context';
import {
  inputValue,
  preventDefault,
  setChecked,
  setValue,
} from 'koroku-console/utils/dom';
import { serviceValues, valueText } from 'koroku-console/utils/memory-settings';
import { can } from 'koroku-console/utils/roles';
import SettingsEditor, {
  SETTINGS_TAB_FIELDS,
} from 'koroku-console/utils/settings-editor';
import {
  ExemptRow,
  MAX_EXEMPT_PATHS,
  exemptPathProblem,
} from 'koroku-console/utils/settings-form';
import type { App, Environment } from 'koroku-console/utils/types';

/** One entry of the section index (ARCHITECTURE 9.1). */
interface SettingsSection {
  id: string;
  title: string;
}

/** `web 1g、worker 256m`: per-service values in one line. */
function perService(app: App, kind: 'limit' | 'reservation'): string {
  return serviceValues(app, kind)
    .map((v) => `${v.service} ${valueText(v.value)}`)
    .join('、');
}

const KNOWN_FIELDS = new Set<string>(SETTINGS_TAB_FIELDS);

function isChecked(current: Environment, value: Environment): boolean {
  return current === value;
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
  Args: { model: AppContext; controller: AppSettingsController };
}

/**
 * The settings that belong to no other tab (ARCHITECTURE 9.1): the source,
 * how the app is published (with its domains), its environment and access
 * protection, the container registry credential and deleting the app.
 * Settings used where their results show live on those tabs.
 */
export default class AppSettingsTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare router: RouterService;

  editor: SettingsEditor;

  @tracked confirmingDelete = false;
  @tracked deleteVolumes = false;
  @tracked deleting = false;
  @tracked deleteError: unknown = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.editor = new SettingsEditor(this.api, args.model, SETTINGS_TAB_FIELDS);
  }

  /** The sections in page order, as the index lists them (ARCHITECTURE 9.1). */
  get sections(): SettingsSection[] {
    const sections: SettingsSection[] = [
      { id: 'source', title: 'ソース' },
      { id: 'publish', title: '公開とドメイン' },
      { id: 'environment', title: '環境' },
      { id: 'protection', title: 'アクセスの保護' },
      { id: 'memory', title: 'メモリの上限と予約' },
      { id: 'registry', title: 'コンテナレジストリの認証' },
    ];
    if (this.canDelete) sections.push({ id: 'danger', title: '危険な操作' });
    return sections;
  }

  /** The section scrolled to the top of the page, marked in the index. */
  @tracked visibleSection: string | null = null;

  /** The section of the index entry: the domains are part of 公開. */
  get currentSection(): string {
    const id = this.visibleSection ?? this.args.controller.section;
    if (id === 'domains') return 'publish';
    return this.sections.some((s) => s.id === id) ? id : 'source';
  }

  /** The section a link led to: brought into view and focused. */
  focuses = (id: string): boolean => this.args.controller.section === id;

  sectionUrl = (id: string): string =>
    this.router.urlFor('app.settings', this.app.slug, {
      queryParams: { section: id },
    });

  /** Follows the page as it scrolls (modifiers/follow-sections). */
  setVisibleSection = (id: string) => {
    this.visibleSection = id;
  };

  jumpFromLink = (id: string, event: MouseEvent) => {
    // Leave "open in a new tab" and the like to the browser.
    if (
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey
    ) {
      return;
    }
    event.preventDefault();
    this.jump(id);
  };

  jumpFromSelect = (event: Event) => {
    this.jump(inputValue(event));
  };

  /**
   * Brings a section into view and focuses it, and keeps it in the URL
   * (the route replaces the history entry).
   */
  jump(id: string): void {
    const section = document.getElementById(`settings-${id}`);
    section?.scrollIntoView({ block: 'start' });
    section?.focus({ preventScroll: true });
    this.visibleSection = id;
    if (this.args.controller.section !== id) {
      void this.router.transitionTo({ queryParams: { section: id } });
    }
  }

  /** `512m（全サービス共通）、web 1g`, or the default. */
  get limitSummary(): string {
    const all = this.app.memory_limit
      ? `${this.app.memory_limit}（全サービス共通）`
      : '基盤の既定値（全サービス共通）';
    const services = perService(this.app, 'limit');
    return services ? `${all}、${services}` : all;
  }

  get reservationSummary(): string {
    const value = this.app.memory_reservation ?? '';
    const all = value
      ? `${valueText(value)}（全サービス共通）`
      : '各サービスの上限の半分（全サービス共通）';
    const services = perService(this.app, 'reservation');
    return services ? `${all}、${services}` : all;
  }

  get showsReservationSummary(): boolean {
    return (
      this.isProductionForm ||
      Boolean(this.app.memory_reservation) ||
      serviceValues(this.app, 'reservation').length > 0
    );
  }

  get isProductionForm(): boolean {
    return this.editor.form.environment === 'production';
  }

  get app(): App {
    return this.args.model.app;
  }

  /** The settings need manage; some of them the owner (ARCHITECTURE 10.7). */
  get readOnly(): boolean {
    return !can(this.app, 'manage');
  }

  get isOwner(): boolean {
    return can(this.app, 'owner');
  }

  get canManage(): boolean {
    return !this.readOnly;
  }

  get otherErrors(): string[] {
    return this.editor.otherErrors(KNOWN_FIELDS);
  }

  setEnvironment = (event: Event) => {
    this.editor.form.environment = inputValue(event) as Environment;
  };

  /** Staging apps get `noindex` from the proxy even when they are public. */
  get publicStaging(): boolean {
    const form = this.editor.form;
    return !form.accessProtection && form.environment === 'staging';
  }

  addExemptPath = () => {
    const form = this.editor.form;
    form.exemptPaths = [...form.exemptPaths, new ExemptRow()];
  };

  /** Marks the rows behind a reported problem, not every row. */
  exemptRowInvalid = (row: ExemptRow): boolean => {
    if (!this.editor.errors['protection_exempt_paths']) return false;
    const path = row.path.trim();
    return (
      path !== '' &&
      (exemptPathProblem(path) !== null ||
        this.editor.form.exemptList.length > MAX_EXEMPT_PATHS)
    );
  };

  removeExemptPath = (row: ExemptRow) => {
    const form = this.editor.form;
    form.exemptPaths = form.exemptPaths.filter((r) => r !== row);
    this.editor.touch();
  };

  save = async () => {
    await this.editor.save();
  };

  /**
   * The owner deletes apps; a review app can also be deleted with manage
   * (ARCHITECTURE 9.9).
   */
  get canDelete(): boolean {
    return (
      this.isOwner || (Boolean(this.app.review) && can(this.app, 'manage'))
    );
  }

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
      await this.api.deleteApp(this.app.slug, {
        volumes: this.deleteVolumes || Boolean(this.app.review),
      });
      this.router.transitionTo('index');
    } catch (error) {
      this.deleteError = error;
      this.confirmingDelete = false;
    } finally {
      this.deleting = false;
    }
  };

  <template>
    <div class="settings-layout" data-test-settings>
      <nav
        class="settings-index"
        aria-label="設定の項目"
        data-test-settings-index
      >
        <ul>
          {{#each this.sections key="id" as |section|}}
            <li>
              <a
                href={{this.sectionUrl section.id}}
                aria-current={{if
                  (eq section.id this.currentSection)
                  "location"
                }}
                data-test-settings-index-link={{section.id}}
                {{on "click" (fn this.jumpFromLink section.id)}}
              >{{section.title}}</a>
            </li>
          {{/each}}
        </ul>
      </nav>

      <div class="settings-jump" data-test-settings-jump>
        <label for="settings-jump-select">項目へ移動</label>
        <select
          id="settings-jump-select"
          data-test-settings-jump-select
          {{on "change" this.jumpFromSelect}}
        >
          {{#each this.sections key="id" as |section|}}
            <option
              value={{section.id}}
              selected={{eq section.id this.currentSection}}
            >{{section.title}}</option>
          {{/each}}
        </select>
      </div>

      <div class="settings-content" {{followSections this.setVisibleSection}}>
        {{#if this.readOnly}}
          <RoleNote @needed="manage" @what="設定の変更" />
        {{/if}}
        <div class="settings-sections" data-test-settings-form>
          <form
            novalidate
            aria-label="ソース"
            {{on "submit" (preventDefault this.save)}}
            {{on "input" this.editor.touch}}
          >
            <fieldset class="role-fieldset" disabled={{this.readOnly}}>
              <div
                class="form-group"
                id="settings-source"
                tabindex="-1"
                data-settings-section="source"
                {{focusSection (this.focuses "source")}}
              >
                <div>
                  <h2
                    class="form-group-title"
                    id="settings-source-title"
                  >ソース</h2>
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
                      @error={{this.editor.errors.default_branch}}
                      data-test-field-group="default_branch"
                      as |field|
                    >
                      <input
                        id={{field.id}}
                        type="text"
                        spellcheck="false"
                        autocomplete="off"
                        value={{this.editor.form.defaultBranch}}
                        aria-describedby={{field.describedBy}}
                        aria-invalid={{field.invalid}}
                        data-test-field="default_branch"
                        {{on
                          "input"
                          (setValue this.editor.form "defaultBranch")
                        }}
                      />
                    </FormField>

                    <FormField
                      class="field-wide"
                      @label="Compose ファイルのパス"
                      @hint="空なら compose.yaml などをリポジトリ直下から自動で探します。"
                      @error={{this.editor.errors.compose_path}}
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
                        value={{this.editor.form.composePath}}
                        aria-describedby={{field.describedBy}}
                        aria-invalid={{field.invalid}}
                        data-test-field="compose_path"
                        {{on "input" (setValue this.editor.form "composePath")}}
                      />
                    </FormField>
                  </div>
                </div>
              </div>
            </fieldset>
          </form>

          <div
            class="form-group"
            id="settings-publish"
            tabindex="-1"
            data-settings-section="publish"
            {{focusSection (this.focuses "publish")}}
          >
            <div>
              <h2 class="form-group-title" id="settings-publish-title">公開</h2>
              <p class="form-group-description">
                外からのリクエストを受けるサービスとホスト名です。
              </p>
            </div>
            <div class="form-group-body">
              <form
                novalidate
                aria-label="公開"
                {{on "submit" (preventDefault this.save)}}
                {{on "input" this.editor.touch}}
              >
                <fieldset class="role-fieldset" disabled={{this.readOnly}}>
                  <div class="form-grid">
                    <FormField
                      @label="公開するサービス"
                      @error={{this.editor.errors.web_service}}
                      data-test-field-group="web_service"
                      as |field|
                    >
                      <input
                        id={{field.id}}
                        type="text"
                        spellcheck="false"
                        autocomplete="off"
                        value={{this.editor.form.webService}}
                        aria-describedby={{field.describedBy}}
                        aria-invalid={{field.invalid}}
                        data-test-field="web_service"
                        {{on "input" (setValue this.editor.form "webService")}}
                      />
                    </FormField>

                    <FormField
                      @label="コンテナ側のポート"
                      @error={{this.editor.errors.web_port}}
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
                        value={{this.editor.form.webPort}}
                        aria-describedby={{field.describedBy}}
                        aria-invalid={{field.invalid}}
                        data-test-field="web_port"
                        {{on "input" (setValue this.editor.form "webPort")}}
                      />
                    </FormField>

                    <FormField
                      @label="ヘルスチェックのパス"
                      @error={{this.editor.errors.healthcheck_path}}
                      data-test-field-group="healthcheck_path"
                      as |field|
                    >
                      <input
                        id={{field.id}}
                        type="text"
                        class="mono"
                        spellcheck="false"
                        autocomplete="off"
                        value={{this.editor.form.healthcheckPath}}
                        aria-describedby={{field.describedBy}}
                        aria-invalid={{field.invalid}}
                        data-test-field="healthcheck_path"
                        {{on
                          "input"
                          (setValue this.editor.form "healthcheckPath")
                        }}
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
                </fieldset>
              </form>
              <DomainList
                @context={{@model}}
                @focus={{this.focuses "domains"}}
              />
            </div>
          </div>
          <form
            novalidate
            aria-label="環境とアクセスの保護"
            {{on "submit" (preventDefault this.save)}}
            {{on "input" this.editor.touch}}
          >
            <fieldset class="role-fieldset" disabled={{this.readOnly}}>

              <div
                class="form-group"
                id="settings-environment"
                tabindex="-1"
                data-settings-section="environment"
                {{focusSection (this.focuses "environment")}}
              >
                <div>
                  <h2
                    class="form-group-title"
                    id="settings-environment-title"
                  >環境</h2>
                  <p class="form-group-description">
                    取り違えないための目印です。メモリの優先度は次のデプロイから変わります。
                  </p>
                </div>
                <div class="form-group-body">
                  {{#unless this.isOwner}}
                    {{#unless this.readOnly}}
                      <RoleNote
                        @needed="owner"
                        @what="環境"
                        data-test-environment-owner-note
                      />
                    {{/unless}}
                  {{/unless}}
                  <fieldset class="option-group">
                    <legend class="visually-hidden">環境</legend>
                    <label class="option">
                      <input
                        type="radio"
                        name="environment"
                        value="staging"
                        checked={{isChecked
                          this.editor.form.environment
                          "staging"
                        }}
                        disabled={{if this.isOwner false true}}
                        data-test-environment-option="staging"
                        {{on "change" this.setEnvironment}}
                      />
                      <span class="option-body">
                        <span class="option-label">ステージング</span>
                        <span
                          class="option-hint"
                        >確認なしでデプロイします。メモリが足りないと先に止まります。</span>
                      </span>
                    </label>
                    <label class="option">
                      <input
                        type="radio"
                        name="environment"
                        value="production"
                        checked={{isChecked
                          this.editor.form.environment
                          "production"
                        }}
                        disabled={{if this.isOwner false true}}
                        data-test-environment-option="production"
                        {{on "change" this.setEnvironment}}
                      />
                      <span class="option-body">
                        <span class="option-label">本番</span>
                        <span class="option-hint">
                          デプロイの前に確認します。メモリを予約し、最後まで止まりません。
                        </span>
                      </span>
                    </label>
                  </fieldset>
                </div>
              </div>

              <div
                class="form-group"
                data-test-protection-group
                id="settings-protection"
                tabindex="-1"
                data-settings-section="protection"
                {{focusSection (this.focuses "protection")}}
              >
                <div>
                  <h2
                    class="form-group-title"
                    id="settings-protection-title"
                  >アクセスの保護</h2>
                  <p class="form-group-description">
                    アプリを開けるのを、管理画面にログインしている人だけにします。保存するとすぐ反映されます。
                  </p>
                </div>
                <div class="form-group-body">
                  <label class="option">
                    <input
                      type="checkbox"
                      checked={{this.editor.form.accessProtection}}
                      aria-describedby="settings-protection-hint"
                      data-test-field="access_protection"
                      {{on
                        "change"
                        (setChecked this.editor.form "accessProtection")
                      }}
                    />
                    <span class="option-body">
                      <span
                        class="option-label"
                      >管理画面にログインしている人だけに公開する</span>
                      <span class="option-hint" id="settings-protection-hint">
                        ブラウザー以外（API
                        の呼び出しや監視サービス）は通れません。
                      </span>
                    </span>
                  </label>

                  {{#if this.editor.form.accessProtection}}
                    <div
                      class="field protection-exempt"
                      role="group"
                      aria-labelledby="settings-exempt-label"
                      aria-describedby="settings-exempt-hint"
                      data-test-field-group="protection_exempt_paths"
                    >
                      <span class="field-label" id="settings-exempt-label">
                        保護を外すパス
                      </span>
                      <p class="field-hint" id="settings-exempt-hint">
                        Webhook などのためのパスです（前方一致。例
                        <code
                        >/webhooks/</code>）。外したパスは、アプリ自身で認証してください。
                      </p>
                      {{#if this.editor.form.exemptPaths.length}}
                        <ul class="exempt-rows">
                          {{#each
                            this.editor.form.exemptPaths key="id"
                            as |row|
                          }}
                            <li class="exempt-row" data-test-exempt-row>
                              <input
                                type="text"
                                class="mono"
                                placeholder="/webhooks/"
                                spellcheck="false"
                                autocomplete="off"
                                aria-label="保護を外すパス"
                                aria-invalid={{if
                                  (this.exemptRowInvalid row)
                                  "true"
                                }}
                                value={{row.path}}
                                data-test-exempt-path
                                {{on "input" (setValue row "path")}}
                              />
                              <Button
                                @size="small"
                                aria-label="{{if
                                  row.path
                                  row.path
                                  'この行'
                                }} を一覧から外す"
                                data-test-exempt-remove
                                {{on "click" (fn this.removeExemptPath row)}}
                              >外す</Button>
                            </li>
                          {{/each}}
                        </ul>
                      {{/if}}
                      <div>
                        <Button
                          @size="small"
                          data-test-exempt-add
                          {{on "click" this.addExemptPath}}
                        >パスを追加</Button>
                      </div>
                      {{#if this.editor.errors.protection_exempt_paths}}
                        <p class="field-error" data-test-field-error>
                          {{this.editor.errors.protection_exempt_paths}}
                        </p>
                      {{/if}}
                    </div>
                  {{else if this.publicStaging}}
                    <Banner
                      class="protection-public"
                      data-test-protection-public
                    >
                      このステージングのアプリは誰でも開けます。検索エンジンに載らないよう、応答には
                      <code>X-Robots-Tag: noindex</code>
                      を付けます。
                    </Banner>
                  {{/if}}
                </div>
              </div>
              <div
                class="form-group"
                id="settings-memory"
                tabindex="-1"
                data-settings-section="memory"
                data-test-memory-summary
                {{focusSection (this.focuses "memory")}}
              >
                <div>
                  <h2
                    class="form-group-title"
                    id="settings-memory-title"
                  >メモリの上限と予約</h2>
                  <p class="form-group-description">
                    変えるのはリソースタブです。
                  </p>
                </div>
                <div class="form-group-body">
                  <dl class="readout memory-summary">
                    <dt>上限</dt>
                    <dd data-test-memory-summary-limit>
                      {{this.limitSummary}}
                    </dd>
                    {{#if this.showsReservationSummary}}
                      <dt>予約</dt>
                      <dd data-test-memory-summary-reservation>
                        {{this.reservationSummary}}
                        {{#unless this.isProductionForm}}
                          <span
                            class="readout-note"
                          >（ステージングのアプリには付きません）</span>
                        {{/unless}}
                      </dd>
                    {{/if}}
                  </dl>
                  <LinkTo
                    @route="app.resources"
                    @model={{this.app.slug}}
                    @query={{hash section="memory"}}
                    class={{buttonClass "default" "small"}}
                    data-test-settings-resources-link
                  >リソースで変える</LinkTo>
                </div>
              </div>
              <div aria-live="polite">
                <ErrorMessage @error={{this.editor.error}} />
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
                  @busy={{this.editor.saving}}
                  @busyLabel="保存中…"
                  data-test-settings-save
                >保存</Button>
                <p class="form-status" aria-live="polite">
                  {{#if this.editor.saved}}
                    <span class="form-saved" data-test-settings-saved>
                      <Icon @name="check" />
                      保存しました。Compose
                      と、環境による優先度の変更は、次のデプロイから反映されます。
                    </span>
                  {{else if this.editor.nothingToSave}}
                    <span data-test-settings-unchanged>変更はありません。</span>
                  {{else}}
                    このタブの項目をまとめて保存します。Compose
                    と、環境による優先度の変更は、次のデプロイから反映されます。
                  {{/if}}
                </p>
              </div>
            </fieldset>
          </form>
        </div>

        <RegistryCredential
          id="settings-registry"
          tabindex="-1"
          data-settings-section="registry"
          @slug={{this.app.slug}}
          @canEdit={{this.canManage}}
          {{focusSection (this.focuses "registry")}}
        />
        {{#if this.canDelete}}
          <section
            class="danger-zone"
            id="settings-danger"
            tabindex="-1"
            aria-labelledby="danger-title"
            data-settings-section="danger"
            data-test-danger-zone
            {{focusSection (this.focuses "danger")}}
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
        {{/if}}
      </div>
    </div>

    <UnsavedSettingsGuard @editor={{this.editor}} @what="設定" />
    <SettingsConfirm @editor={{this.editor}} />

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
        {{#if this.app.review}}
          <p data-test-delete-review>
            レビューアプリなので、volume
            も削除します（データは親アプリの写しです）。
          </p>
        {{else}}
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
        {{/if}}
        <p data-test-delete-leftovers>
          ログとバックアップはこの名前で残り、同じ名前で作ったアプリのものになります。
        </p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
