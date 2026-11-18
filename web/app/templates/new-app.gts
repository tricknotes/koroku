import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import type RouterService from '@ember/routing/router-service';
import { pageTitle } from 'ember-page-title';
import ErrorMessage from 'paas-console/components/error-message';
import FeatureUnavailable from 'paas-console/components/feature-unavailable';
import type ApiService from 'paas-console/services/api';
import { inputValue, preventDefault, setValue } from 'paas-console/utils/dom';
import { errorMessage, fieldErrors } from 'paas-console/utils/errors';
import { isValidSlug, suggestSlug } from 'paas-console/utils/format';
import Loadable from 'paas-console/utils/loadable';
import type {
  ComposeDetection,
  ComposeService,
  Environment,
  NewApp,
  Repo,
} from 'paas-console/utils/types';

const KNOWN_FIELDS = new Set([
  'slug',
  'repo_full_name',
  'repo_url',
  'default_branch',
  'compose_path',
  'web_service',
  'web_port',
  'healthcheck_path',
  'environment',
]);

/** The service most likely to be the public one. */
function guessWebService(services: ComposeService[]): ComposeService | null {
  return (
    services.find((s) => s.name === 'web') ??
    services.find((s) => s.name === 'app') ??
    services.find((s) => (s.ports ?? []).length > 0) ??
    services[0] ??
    null
  );
}

export default class NewAppTemplate extends Component {
  @service declare api: ApiService;
  @service declare router: RouterService;

  repos = new Loadable<Repo[]>(() => this.api.listRepos());
  compose = new Loadable<ComposeDetection>(() =>
    this.api.detectCompose(this.repoFullName, {
      ref: this.defaultBranch,
      path: this.requestedComposePath,
    }),
  );

  @tracked repoFullName = '';
  @tracked repoUrl = '';
  @tracked defaultBranch = '';
  @tracked composePath = '';
  @tracked webService = '';
  @tracked webPort = '';
  @tracked slug = '';
  @tracked environment: Environment = 'staging';
  @tracked healthcheckPath = '/';

  @tracked submitting = false;
  @tracked error: unknown = null;
  @tracked errors: Record<string, string> = {};

  private slugEdited = false;
  private requestedComposePath = '';

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.repos.load();
  }

  /** Without the GitHub App the repository is given as a clone URL. */
  get manualRepo(): boolean {
    return this.repos.unavailable !== null;
  }

  get repoChosen(): boolean {
    return this.manualRepo
      ? this.repoUrl.trim() !== ''
      : this.repoFullName !== '';
  }

  get files(): string[] {
    return this.compose.value?.files ?? [];
  }

  get hasManyFiles(): boolean {
    return this.files.length > 1;
  }

  get services(): ComposeService[] {
    return this.compose.value?.services ?? [];
  }

  /** No services could be read: service and port are typed by hand. */
  get manualService(): boolean {
    return this.manualRepo || this.services.length === 0;
  }

  get detectedPorts(): number[] {
    const service = this.services.find((s) => s.name === this.webService);
    return service?.ports ?? [];
  }

  /** Server-side errors for fields this form has no input for. */
  get otherErrors(): string[] {
    return Object.entries(this.errors)
      .filter(([name]) => !KNOWN_FIELDS.has(name))
      .map(([name, reason]) => `${name}: ${reason}`);
  }

  get composeError(): string {
    return this.compose.error ? errorMessage(this.compose.error) : '';
  }

  selectRepo = (event: Event) => {
    this.repoFullName = inputValue(event);
    const repo = (this.repos.value ?? []).find(
      (r) => r.full_name === this.repoFullName,
    );
    this.defaultBranch = repo?.default_branch ?? '';
    this.suggestSlugFrom(this.repoFullName);
    this.requestedComposePath = '';
    if (repo) void this.detect();
  };

  setRepoUrl = (event: Event) => {
    this.repoUrl = inputValue(event);
    this.suggestSlugFrom(this.repoUrl.trim());
  };

  selectComposePath = (event: Event) => {
    this.requestedComposePath = inputValue(event);
    void this.detect();
  };

  selectService = (event: Event) => {
    this.webService = inputValue(event);
    const port = this.detectedPorts[0];
    if (port !== undefined) this.webPort = String(port);
  };

  setSlug = (event: Event) => {
    this.slug = inputValue(event);
    this.slugEdited = true;
  };

  setEnvironment = (event: Event) => {
    this.environment = inputValue(event) as Environment;
  };

  private suggestSlugFrom(name: string): void {
    if (!this.slugEdited) this.slug = suggestSlug(name);
  }

  private async detect(): Promise<void> {
    const repo = this.repoFullName;
    await this.compose.load();
    if (repo !== this.repoFullName) return;

    const detection = this.compose.value;
    this.composePath = detection?.path ?? '';
    const guess = guessWebService(detection?.services ?? []);
    this.webService = guess?.name ?? '';
    const port = guess?.ports?.[0];
    this.webPort = port === undefined ? '' : String(port);
  }

  private validate(): Record<string, string> {
    const errors: Record<string, string> = {};
    if (!this.repoChosen) {
      errors[this.manualRepo ? 'repo_url' : 'repo_full_name'] =
        'リポジトリを指定してください。';
    }
    if (!isValidSlug(this.slug)) {
      errors['slug'] =
        '英小文字で始まる英小文字・数字・ハイフン 40 文字以内で入力してください。';
    }
    const port = Number(this.webPort);
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      errors['web_port'] = '1〜65535 の整数を入力してください。';
    }
    return errors;
  }

  submit = async () => {
    this.error = null;
    this.errors = this.validate();
    if (Object.keys(this.errors).length > 0) return;

    const body: NewApp = {
      slug: this.slug,
      web_port: Number(this.webPort),
      environment: this.environment,
    };
    if (this.manualRepo) {
      body.repo_url = this.repoUrl.trim();
    } else {
      body.repo_full_name = this.repoFullName;
    }
    if (this.defaultBranch.trim()) {
      body.default_branch = this.defaultBranch.trim();
    }
    // With a single candidate the server's auto-detection is kept.
    if (this.files.length > 1 && this.composePath) {
      body.compose_path = this.composePath;
    }
    if (this.webService.trim()) body.web_service = this.webService.trim();
    if (this.healthcheckPath.trim()) {
      body.healthcheck_path = this.healthcheckPath.trim();
    }

    this.submitting = true;
    try {
      const app = await this.api.createApp(body);
      this.router.transitionTo('app.deploy', app.slug);
    } catch (error) {
      this.errors = fieldErrors(error);
      this.error = error;
    } finally {
      this.submitting = false;
    }
  };

  <template>
    {{pageTitle "アプリを作成"}}

    <div class="page-header">
      <h2>アプリを作成</h2>
      <LinkTo @route="index" class="button">一覧に戻る</LinkTo>
    </div>

    <form
      class="card form"
      novalidate
      data-test-new-app-form
      {{on "submit" (preventDefault this.submit)}}
    >
      <fieldset class="form-section">
        <legend>1. リポジトリ</legend>

        {{#if this.manualRepo}}
          {{#if this.repos.unavailable}}
            <FeatureUnavailable @kind={{this.repos.unavailable}} />
          {{/if}}
          <label class="field" for="new-repo-url">
            <span class="field-label">公開リポジトリの URL</span>
            <input
              id="new-repo-url"
              type="url"
              placeholder="https://github.com/octo/blog"
              value={{this.repoUrl}}
              aria-invalid={{if this.errors.repo_url "true"}}
              data-test-field="repo_url"
              {{on "input" this.setRepoUrl}}
            />
            <span class="field-hint">GitHub 連携が使えないため、公開リポジトリの
              URL を直接指定します。</span>
            {{#if this.errors.repo_url}}
              <span class="field-error" data-test-field-error="repo_url">
                {{this.errors.repo_url}}
              </span>
            {{/if}}
          </label>
        {{else if this.repos.error}}
          <div class="alert alert-danger" role="alert" data-test-error>
            <span>{{this.repos.errorMessage}}</span>
            <button
              type="button"
              class="button button-small"
              {{on "click" this.repos.reload}}
            >再読み込み</button>
          </div>
        {{else if this.repos.loaded}}
          <label class="field" for="new-repo">
            <span class="field-label">リポジトリ</span>
            <select
              id="new-repo"
              aria-invalid={{if this.errors.repo_full_name "true"}}
              data-test-field="repo_full_name"
              {{on "change" this.selectRepo}}
            >
              <option value="" selected={{eq this.repoFullName ""}}>
                選択してください
              </option>
              {{#each this.repos.value key="id" as |repo|}}
                <option
                  value={{repo.full_name}}
                  selected={{eq repo.full_name this.repoFullName}}
                >{{repo.full_name}}{{if repo.private "（private）"}}</option>
              {{/each}}
            </select>
            <span class="field-hint">GitHub App
              をインストールしたリポジトリが並びます。同じリポジトリから複数のアプリ（本番とステージングなど）を作れます。</span>
            {{#if this.errors.repo_full_name}}
              <span class="field-error" data-test-field-error="repo_full_name">
                {{this.errors.repo_full_name}}
              </span>
            {{/if}}
          </label>
        {{else}}
          <p class="muted" aria-live="polite">リポジトリを読み込み中…</p>
        {{/if}}

        {{#if this.repoChosen}}
          <label class="field" for="new-branch">
            <span class="field-label">既定のブランチ</span>
            <input
              id="new-branch"
              type="text"
              value={{this.defaultBranch}}
              placeholder="main"
              spellcheck="false"
              aria-invalid={{if this.errors.default_branch "true"}}
              data-test-field="default_branch"
              {{on "input" (setValue this "defaultBranch")}}
            />
            {{#if this.errors.default_branch}}
              <span class="field-error" data-test-field-error="default_branch">
                {{this.errors.default_branch}}
              </span>
            {{/if}}
          </label>
        {{/if}}
      </fieldset>

      {{#if this.repoChosen}}
        <fieldset class="form-section">
          <legend>2. Compose と公開サービス</legend>

          <div aria-live="polite">
            {{#if this.compose.loading}}
              <p class="muted" data-test-compose-loading>Compose
                ファイルを検出中…</p>
            {{else if this.compose.unavailable}}
              <p class="notice" data-test-compose-manual>Compose
                ファイルの検出は利用できません。公開するサービスとポートを入力してください。</p>
            {{else if this.composeError}}
              <p
                class="alert alert-danger"
                role="alert"
                data-test-compose-error
              >
                Compose ファイルを検出できませんでした。{{this.composeError}}
              </p>
            {{else if this.compose.loaded}}
              {{#if this.files.length}}
                <p data-test-compose-path>検出した Compose ファイル:
                  <code>{{this.composePath}}</code></p>
              {{else}}
                <p class="notice" data-test-compose-missing>リポジトリ直下に
                  Compose
                  ファイルが見つかりませんでした。パスは作成後に「設定」で指定できます。</p>
              {{/if}}
              {{#if this.manualService}}
                <p
                  class="notice"
                  data-test-compose-manual
                >サービスを読み取れなかったため、公開するサービスとポートを入力してください。</p>
              {{/if}}
            {{/if}}
          </div>

          {{#if this.hasManyFiles}}
            <label class="field" for="new-compose-path">
              <span class="field-label">Compose ファイル</span>
              <select
                id="new-compose-path"
                data-test-field="compose_path"
                {{on "change" this.selectComposePath}}
              >
                {{#each this.files as |file|}}
                  <option value={{file}} selected={{eq file this.composePath}}>
                    {{file}}
                  </option>
                {{/each}}
              </select>
              {{#if this.errors.compose_path}}
                <span class="field-error" data-test-field-error="compose_path">
                  {{this.errors.compose_path}}
                </span>
              {{/if}}
            </label>
          {{/if}}

          <div class="field-row">
            <label class="field" for="new-web-service">
              <span class="field-label">公開するサービス</span>
              {{#if this.manualService}}
                <input
                  id="new-web-service"
                  type="text"
                  placeholder="web"
                  spellcheck="false"
                  value={{this.webService}}
                  aria-invalid={{if this.errors.web_service "true"}}
                  data-test-field="web_service"
                  {{on "input" (setValue this "webService")}}
                />
              {{else}}
                <select
                  id="new-web-service"
                  aria-invalid={{if this.errors.web_service "true"}}
                  data-test-field="web_service"
                  {{on "change" this.selectService}}
                >
                  {{#each this.services key="name" as |service|}}
                    <option
                      value={{service.name}}
                      selected={{eq service.name this.webService}}
                    >{{service.name}}</option>
                  {{/each}}
                </select>
              {{/if}}
              {{#if this.errors.web_service}}
                <span class="field-error" data-test-field-error="web_service">
                  {{this.errors.web_service}}
                </span>
              {{/if}}
            </label>

            <label class="field" for="new-web-port">
              <span class="field-label">コンテナ側のポート</span>
              <input
                id="new-web-port"
                type="number"
                inputmode="numeric"
                min="1"
                max="65535"
                list="new-web-port-options"
                placeholder="3000"
                value={{this.webPort}}
                aria-invalid={{if this.errors.web_port "true"}}
                data-test-field="web_port"
                {{on "input" (setValue this "webPort")}}
              />
              <datalist id="new-web-port-options">
                {{#each this.detectedPorts as |port|}}
                  <option value={{port}}></option>
                {{/each}}
              </datalist>
              {{#if this.errors.web_port}}
                <span class="field-error" data-test-field-error="web_port">
                  {{this.errors.web_port}}
                </span>
              {{/if}}
            </label>
          </div>

          <label class="field" for="new-healthcheck">
            <span class="field-label">ヘルスチェックのパス</span>
            <input
              id="new-healthcheck"
              type="text"
              spellcheck="false"
              value={{this.healthcheckPath}}
              aria-invalid={{if this.errors.healthcheck_path "true"}}
              data-test-field="healthcheck_path"
              {{on "input" (setValue this "healthcheckPath")}}
            />
            <span class="field-hint">デプロイ後にこのパスへ HTTP
              で応答を確認します。</span>
            {{#if this.errors.healthcheck_path}}
              <span
                class="field-error"
                data-test-field-error="healthcheck_path"
              >
                {{this.errors.healthcheck_path}}
              </span>
            {{/if}}
          </label>
        </fieldset>

        <fieldset class="form-section">
          <legend>3. 名前と環境</legend>

          <label class="field" for="new-slug">
            <span class="field-label">slug</span>
            <input
              id="new-slug"
              type="text"
              maxlength="40"
              spellcheck="false"
              autocapitalize="off"
              value={{this.slug}}
              aria-invalid={{if this.errors.slug "true"}}
              data-test-field="slug"
              {{on "input" this.setSlug}}
            />
            <span class="field-hint">URL の
              <code>&lt;slug&gt;</code>
              の部分になります。作成後は変えられません。</span>
            {{#if this.errors.slug}}
              <span class="field-error" data-test-field-error="slug">
                {{this.errors.slug}}
              </span>
            {{/if}}
          </label>

          <div class="field" role="radiogroup" aria-labelledby="new-env-label">
            <span class="field-label" id="new-env-label">環境</span>
            <label class="choice">
              <input
                type="radio"
                name="environment"
                value="staging"
                checked={{eq this.environment "staging"}}
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
                checked={{eq this.environment "production"}}
                data-test-environment-option="production"
                {{on "change" this.setEnvironment}}
              />
              本番（デプロイと削除の前に確認します）
            </label>
            {{#if this.errors.environment}}
              <span class="field-error" data-test-field-error="environment">
                {{this.errors.environment}}
              </span>
            {{/if}}
          </div>
        </fieldset>
      {{/if}}

      <div aria-live="polite">
        <ErrorMessage @error={{this.error}} />
        {{#if this.otherErrors.length}}
          <ul class="field-error" data-test-other-errors>
            {{#each this.otherErrors as |line|}}
              <li>{{line}}</li>
            {{/each}}
          </ul>
        {{/if}}
      </div>

      {{#if this.repoChosen}}
        <div class="form-actions">
          <button
            type="submit"
            class="button button-primary"
            disabled={{this.submitting}}
            data-test-submit
          >{{if this.submitting "作成中…" "作成する"}}</button>
        </div>
      {{/if}}
    </form>
  </template>
}
