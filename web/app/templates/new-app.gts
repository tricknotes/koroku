import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import type RouterService from '@ember/routing/router-service';
import { pageTitle } from 'ember-page-title';
import Banner from 'paas-console/components/banner';
import Button, { buttonClass } from 'paas-console/components/button';
import ErrorMessage from 'paas-console/components/error-message';
import FormField from 'paas-console/components/form-field';
import Icon from 'paas-console/components/icon';
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
    } else if (this.slug === 'new') {
      // `/apps/new` is this page, so an app of that name could not be opened.
      errors['slug'] = 'この slug は使えません。別の名前にしてください。';
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
      <h1>アプリを作成</h1>
    </div>

    <form
      class="new-app"
      novalidate
      data-test-new-app-form
      {{on "submit" (preventDefault this.submit)}}
    >
      <div class="form-group">
        <div>
          <h2 class="form-group-title">1. リポジトリ</h2>
          <p class="form-group-description">
            デプロイするコードのあるリポジトリです。同じリポジトリから複数のアプリ（本番とステージングなど）を作れます。
          </p>
        </div>
        <div class="form-group-body">
          {{#if this.manualRepo}}
            <Banner data-test-unavailable={{this.repos.unavailable}}>
              GitHub
              連携がこのサーバーに設定されていないため、リポジトリを一覧から選べません。公開リポジトリの
              URL を直接指定します。
            </Banner>
            <FormField
              @label="公開リポジトリの URL"
              @error={{this.errors.repo_url}}
              data-test-field-group="repo_url"
              as |field|
            >
              <input
                id={{field.id}}
                type="url"
                class="mono"
                placeholder="https://github.com/octo/blog"
                autocomplete="off"
                value={{this.repoUrl}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="repo_url"
                {{on "input" this.setRepoUrl}}
              />
            </FormField>
          {{else if this.repos.error}}
            <Banner
              @tone="error"
              @title="リポジトリの一覧を取得できませんでした"
              data-test-error
            >
              <:default>{{this.repos.errorMessage}}</:default>
              <:actions>
                <Button @size="small" {{on "click" this.repos.reload}}>
                  再読み込み
                </Button>
              </:actions>
            </Banner>
          {{else if this.repos.loaded}}
            <FormField
              @label="リポジトリ"
              @hint="GitHub App をインストールしたリポジトリが並びます。"
              @error={{this.errors.repo_full_name}}
              data-test-field-group="repo_full_name"
              as |field|
            >
              <select
                id={{field.id}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
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
            </FormField>
          {{else}}
            <p class="new-app-progress" aria-live="polite" data-test-loading>
              <Icon @name="spinner" />
              リポジトリを読み込み中…
            </p>
          {{/if}}

          {{#if this.repoChosen}}
            <FormField
              class="field-narrow"
              @label="既定のブランチ"
              @hint="デプロイの画面で最初に選ばれるブランチです。"
              @error={{this.errors.default_branch}}
              data-test-field-group="default_branch"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                placeholder="main"
                spellcheck="false"
                autocomplete="off"
                value={{this.defaultBranch}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="default_branch"
                {{on "input" (setValue this "defaultBranch")}}
              />
            </FormField>
          {{/if}}
        </div>
      </div>

      {{#if this.repoChosen}}
        <div class="form-group">
          <div>
            <h2 class="form-group-title">2. Compose と公開サービス</h2>
            <p class="form-group-description">
              Compose
              ファイルのサービスのうち、外からのリクエストを受けるものと、そのコンテナ側のポートです。
            </p>
          </div>
          <div class="form-group-body">
            <div aria-live="polite">
              {{#if this.compose.loading}}
                <p class="new-app-progress" data-test-compose-loading>
                  <Icon @name="spinner" />
                  Compose ファイルを検出中…
                </p>
              {{else if this.compose.unavailable}}
                <Banner data-test-compose-manual>
                  Compose
                  ファイルの検出は利用できません。公開するサービスとポートを入力してください。
                </Banner>
              {{else if this.composeError}}
                <Banner
                  @tone="error"
                  @title="Compose ファイルを検出できませんでした"
                  data-test-compose-error
                >
                  {{this.composeError}}
                </Banner>
              {{else if this.compose.loaded}}
                {{#if this.files.length}}
                  <dl class="readout">
                    <dt>検出した Compose ファイル</dt>
                    <dd>
                      <span class="mono" data-test-compose-path>
                        {{this.composePath}}
                      </span>
                    </dd>
                  </dl>
                {{else}}
                  <Banner @tone="warning" data-test-compose-missing>
                    リポジトリ直下に Compose
                    ファイルが見つかりませんでした。パスは作成後に「設定」で指定できます。
                  </Banner>
                {{/if}}
                {{#if this.manualService}}
                  <Banner data-test-compose-manual>
                    サービスを読み取れなかったため、公開するサービスとポートを入力してください。
                  </Banner>
                {{/if}}
              {{/if}}
            </div>

            {{#if this.hasManyFiles}}
              <FormField
                @label="Compose ファイル"
                @hint="候補が複数あります。使うものを選んでください。"
                @error={{this.errors.compose_path}}
                data-test-field-group="compose_path"
                as |field|
              >
                <select
                  id={{field.id}}
                  class="mono"
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="compose_path"
                  {{on "change" this.selectComposePath}}
                >
                  {{#each this.files as |file|}}
                    <option
                      value={{file}}
                      selected={{eq file this.composePath}}
                    >{{file}}</option>
                  {{/each}}
                </select>
              </FormField>
            {{/if}}

            <div class="form-grid">
              <FormField
                @label="公開するサービス"
                @error={{this.errors.web_service}}
                data-test-field-group="web_service"
                as |field|
              >
                {{#if this.manualService}}
                  <input
                    id={{field.id}}
                    type="text"
                    placeholder="web"
                    spellcheck="false"
                    autocomplete="off"
                    value={{this.webService}}
                    aria-describedby={{field.describedBy}}
                    aria-invalid={{field.invalid}}
                    data-test-field="web_service"
                    {{on "input" (setValue this "webService")}}
                  />
                {{else}}
                  <select
                    id={{field.id}}
                    aria-describedby={{field.describedBy}}
                    aria-invalid={{field.invalid}}
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
                  list="new-web-port-options"
                  placeholder="3000"
                  value={{this.webPort}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="web_port"
                  {{on "input" (setValue this "webPort")}}
                />
                <datalist id="new-web-port-options">
                  {{#each this.detectedPorts as |port|}}
                    <option value={{port}}></option>
                  {{/each}}
                </datalist>
              </FormField>
            </div>

            <FormField
              class="field-narrow"
              @label="ヘルスチェックのパス"
              @hint="デプロイのあと、このパスが HTTP で応答するかを確かめます。"
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
                value={{this.healthcheckPath}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="healthcheck_path"
                {{on "input" (setValue this "healthcheckPath")}}
              />
            </FormField>
          </div>
        </div>

        <div class="form-group">
          <div>
            <h2 class="form-group-title">3. 名前と環境</h2>
            <p class="form-group-description">
              名前は URL
              の一部になり、作成後は変えられません。環境は、本番とステージングを取り違えないための目印です。
            </p>
          </div>
          <div class="form-group-body">
            <FormField
              @label="アプリ名（slug）"
              @hint="英小文字で始まる英小文字・数字・ハイフン、40 文字以内。"
              @error={{this.errors.slug}}
              data-test-field-group="slug"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                class="mono"
                maxlength="40"
                spellcheck="false"
                autocapitalize="off"
                autocomplete="off"
                value={{this.slug}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="slug"
                {{on "input" this.setSlug}}
              />
            </FormField>

            <fieldset class="option-group" data-test-field-group="environment">
              <legend>環境</legend>
              <label class="option">
                <input
                  type="radio"
                  name="environment"
                  value="staging"
                  checked={{eq this.environment "staging"}}
                  data-test-environment-option="staging"
                  {{on "change" this.setEnvironment}}
                />
                <span class="option-body">
                  <span class="option-label">ステージング</span>
                  <span class="option-hint">
                    デプロイは確認なしで始まります。
                  </span>
                </span>
              </label>
              <label class="option">
                <input
                  type="radio"
                  name="environment"
                  value="production"
                  checked={{eq this.environment "production"}}
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
              {{#if this.errors.environment}}
                <p class="field-error" data-test-field-error>
                  {{this.errors.environment}}
                </p>
              {{/if}}
            </fieldset>
          </div>
        </div>
      {{/if}}

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
        {{#if this.repoChosen}}
          <Button
            @type="submit"
            @variant="primary"
            @busy={{this.submitting}}
            @busyLabel="作成中…"
            data-test-submit
          >作成</Button>
        {{/if}}
        <LinkTo @route="index" class={{buttonClass "default"}} data-test-cancel>
          キャンセル
        </LinkTo>
      </div>
    </form>
  </template>
}
