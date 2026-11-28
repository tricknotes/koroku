import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { concat, eq } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import type RouterService from '@ember/routing/router-service';
import { pageTitle } from 'ember-page-title';
import Banner from 'koroku-console/components/banner';
import Button, { buttonClass } from 'koroku-console/components/button';
import ComposeEditor from 'koroku-console/components/compose-editor';
import EnvPresetsEditor from 'koroku-console/components/env-presets-editor';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import type ApiService from 'koroku-console/services/api';
import type SessionService from 'koroku-console/services/session';
import {
  composeContentProblem,
  normalizeCompose,
} from 'koroku-console/utils/compose-source';
import { inputValue, preventDefault, setValue } from 'koroku-console/utils/dom';
import { EnvDraft } from 'koroku-console/utils/env-presets';
import { errorMessage, fieldErrors } from 'koroku-console/utils/errors';
import { isValidSlug, suggestSlug } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import type {
  ComposeDetection,
  ComposeService,
  ComposeSource,
  EnvPresets,
  Environment,
  NewApp,
  Repo,
} from 'koroku-console/utils/types';

const KNOWN_FIELDS = new Set([
  'slug',
  'repo_full_name',
  'repo_url',
  'default_branch',
  'compose_path',
  'compose_content',
  'web_service',
  'web_port',
  'healthcheck_path',
  'environment',
  'env',
]);

/**
 * The `owner/name` of a github.com repository URL, or '' for any other URL.
 * Compose detection can read those without the App when they are public.
 */
export function githubFullName(url: string): string {
  const match =
    /^https:\/\/github\.com\/([\w.-]+)\/([\w.-]+?)(?:\.git)?\/?$/i.exec(
      url.trim(),
    );
  return match ? `${match[1]}/${match[2]}` : '';
}

/** The option of the candidates that switches to typing a path. */
const OTHER_PATH = '\u0000other';

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
  @service declare session: SessionService;

  /**
   * Asks for the recent login before the form (ARCHITECTURE 9.8): saving
   * variables with the app needs one, and logging in after typing would
   * lose the form. Decided once, when the screen opens.
   */
  @tracked loginFirst = false;

  repos = new Loadable<Repo[]>(() => this.api.listRepos());
  compose = new Loadable<ComposeDetection>(() =>
    this.api.detectCompose(this.detectRepo, {
      ref: this.detectedBranch,
      path: this.requestedComposePath,
    }),
  );

  /** Variables to prefill, read on the branch of `presetsBranch`. */
  presets = new Loadable<EnvPresets>(() =>
    this.api.getEnvPresets(this.detectRepo, {
      ref: this.presetsBranch,
      // A Compose file written here is the one that runs: the repository's
      // says nothing about what it sets (ARCHITECTURE 6.3).
      path: this.inlineCompose ? undefined : this.requestedComposePath,
      compose_source: this.inlineCompose ? 'inline' : undefined,
    }),
  );
  draft = new EnvDraft();
  /** The last detection that could read the repository, or null. */
  @tracked presetResult: EnvPresets | null = null;
  @tracked presetsBranch = '';

  @tracked repoFullName = '';
  @tracked repoUrl = '';
  /** The repository is given by URL although the App lists repositories. */
  @tracked byUrl = false;
  @tracked defaultBranch = '';
  /** The branch the shown Compose detection was made on. */
  @tracked detectedBranch = '';
  @tracked composePath = '';
  /** Candidates found at the repository root by the last detection. */
  @tracked knownFiles: string[] = [];
  /** The Compose file is typed in instead of picked from the candidates. */
  @tracked typingPath = false;
  @tracked typedPath = '';
  /** The repository's Compose file, or one written here (ARCHITECTURE 6.3). */
  @tracked composeSource: ComposeSource = 'repository';
  @tracked composeContent = '';
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
    this.loginFirst = !this.session.recentlyAuthenticated;
    void this.repos.load();
  }

  reauthenticate = () => this.session.reauthenticate();

  /** Fills the form without logging in; variables then wait for later. */
  skipLogin = () => {
    this.loginFirst = false;
  };

  /** No GitHub App on this server: repositories cannot be listed. */
  get appMissing(): boolean {
    return this.repos.unavailable !== null;
  }

  /** The repository is given as a clone URL instead of picked from a list. */
  get manualRepo(): boolean {
    return this.appMissing || this.byUrl;
  }

  /** The `owner/name` Compose detection reads, or '' when it cannot. */
  get detectRepo(): string {
    return this.manualRepo ? githubFullName(this.repoUrl) : this.repoFullName;
  }

  /** A URL outside github.com: nothing can be read before deploying. */
  get undetectableUrl(): boolean {
    return (
      this.manualRepo && this.repoUrl.trim() !== '' && this.detectRepo === ''
    );
  }

  get repoChosen(): boolean {
    return this.manualRepo
      ? this.repoUrl.trim() !== ''
      : this.repoFullName !== '';
  }

  get files(): string[] {
    return this.knownFiles;
  }

  /** The path is typed: chosen so, or there is no candidate to pick. */
  get showsPathInput(): boolean {
    return this.typingPath || this.knownFiles.length === 0;
  }

  /** Whether the create request names the file (else the server detects). */
  get chosenComposePath(): string {
    if (this.showsPathInput) return this.typedPath.trim();
    return this.knownFiles.length > 1 ? this.composePath : '';
  }

  get services(): ComposeService[] {
    return this.compose.value?.services ?? [];
  }

  get inlineCompose(): boolean {
    return this.composeSource === 'inline';
  }

  /**
   * No services could be read, or the Compose file is written here (its
   * services are not the repository's): service and port are typed by hand.
   */
  get manualService(): boolean {
    return this.inlineCompose || this.services.length === 0;
  }

  get detectedPorts(): number[] {
    if (this.inlineCompose) return [];
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
    this.resetComposeChoice();
    this.resetPresets();
    if (repo) {
      void this.detect();
      void this.loadPresets();
    }
  };

  setRepoUrl = (event: Event) => {
    this.repoUrl = inputValue(event);
    this.suggestSlugFrom(this.repoUrl.trim());
  };

  /** Detects once the URL field is left, for github.com URLs. */
  changeRepoUrl = () => {
    this.resetComposeChoice();
    this.compose.reset();
    this.resetPresets();
    if (this.detectRepo !== '') {
      void this.detect();
      void this.loadPresets();
    }
  };

  /** Switches between the App's list and a typed URL. */
  useUrl = () => {
    this.byUrl = true;
    this.repoFullName = '';
    this.resetDetection();
  };

  useList = () => {
    this.byUrl = false;
    this.repoUrl = '';
    this.resetDetection();
  };

  private resetDetection(): void {
    this.defaultBranch = '';
    this.detectedBranch = '';
    this.resetComposeChoice();
    this.webService = '';
    this.webPort = '';
    this.compose.reset();
    this.resetPresets();
  }

  /** Another repository: candidates and a typed path start afresh. */
  private resetComposeChoice(): void {
    this.requestedComposePath = '';
    this.composePath = '';
    this.knownFiles = [];
    this.typingPath = false;
    this.typedPath = '';
  }

  /** Another repository: its suggestions and the rows start afresh. */
  private resetPresets(): void {
    this.presets.reset();
    this.presetsBranch = '';
    this.presetResult = null;
    this.draft.reset();
  }

  /**
   * Detects again on the branch typed in, once the field is left (not on
   * every keystroke). Another branch can have other files, so the chosen
   * file is forgotten. The presets follow the same branch; rows the user
   * touched stay as they are.
   */
  changeBranch = () => {
    if (this.detectRepo === '') return;
    if (this.defaultBranch.trim() === this.detectedBranch) return;
    // A typed path stays: it names a file the user knows about.
    if (!this.showsPathInput) this.requestedComposePath = '';
    void this.detect();
    void this.loadPresets();
  };

  setComposeSource = (event: Event) => {
    this.chooseComposeSource(inputValue(event) as ComposeSource);
  };

  writeCompose = () => this.chooseComposeSource('inline');

  private chooseComposeSource(source: ComposeSource): void {
    if (source === this.composeSource) return;
    this.composeSource = source;
    delete this.errors['compose_content'];
    this.errors = { ...this.errors };
    // Which presets the Compose file already sets depends on the file.
    if (this.detectRepo !== '') void this.loadPresets();
  }

  setComposeContent = (value: string) => {
    this.composeContent = value;
  };

  selectComposePath = (event: Event) => {
    const value = inputValue(event);
    if (value === OTHER_PATH) {
      this.typingPath = true;
      this.typedPath = this.composePath;
      return;
    }
    this.requestedComposePath = value;
    void this.detect();
    // The Compose file decides which presets it already sets.
    void this.loadPresets();
  };

  setTypedPath = (event: Event) => {
    this.typedPath = inputValue(event);
  };

  /** Reads the typed file once the field is left, where it can be read. */
  changeTypedPath = () => {
    const path = this.typedPath.trim();
    this.composePath = path;
    if (this.detectRepo === '' || path === this.requestedComposePath) return;
    this.requestedComposePath = path;
    void this.detect();
    void this.loadPresets();
  };

  /** Back to the detected candidates. */
  useCandidates = () => {
    this.typingPath = false;
    this.typedPath = '';
    this.requestedComposePath = '';
    void this.detect();
    void this.loadPresets();
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
    const repo = this.detectRepo;
    const branch = this.defaultBranch.trim();
    this.detectedBranch = branch;
    await this.compose.load();
    if (repo !== this.detectRepo || branch !== this.detectedBranch) return;

    // A failed read (a typed path that is not there) keeps what was found.
    const detection = this.compose.error ? null : this.compose.value;
    if (!detection) return;
    this.knownFiles = detection.files ?? [];
    this.composePath = detection.path;
    const guess = guessWebService(detection?.services ?? []);
    this.webService = guess?.name ?? '';
    const port = guess?.ports?.[0];
    this.webPort = port === undefined ? '' : String(port);
  }

  /**
   * The environment step: shown while reading the repository and when it
   * could be read. For a URL outside github.com, or when the files cannot
   * be read, there is nothing to suggest and the step stays out of the way.
   */
  get showEnv(): boolean {
    return (
      this.detectRepo !== '' &&
      (this.presets.loading ||
        this.presetResult !== null ||
        this.draft.rows.length > 0)
    );
  }

  private async loadPresets(): Promise<void> {
    const repo = this.detectRepo;
    const branch = this.defaultBranch.trim();
    this.presetsBranch = branch;
    await this.presets.load();
    if (repo !== this.detectRepo || branch !== this.presetsBranch) return;
    const result = this.presets.error ? null : this.presets.value;
    if (result?.status === 'ok') {
      this.presetResult = result;
      this.draft.applyPresets(result.presets);
    } else {
      this.presetResult = null;
    }
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
    if (!this.draft.valid) {
      errors['env'] = '環境変数のキーを直してください。';
    }
    if (this.inlineCompose) {
      const compose = composeContentProblem(this.composeContent);
      if (compose) errors['compose_content'] = compose;
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
      // The ID is the repository's identity: deployments fetch only from
      // the repository picked here, whatever its name denotes later
      // (ARCHITECTURE 5).
      const picked = (this.repos.value ?? []).find(
        (r) => r.full_name === this.repoFullName,
      );
      if (picked) body.repo_id = picked.id;
    }
    if (this.defaultBranch.trim()) {
      body.default_branch = this.defaultBranch.trim();
    }
    if (this.inlineCompose) {
      body.compose_source = 'inline';
      body.compose_content = normalizeCompose(this.composeContent);
    } else if (this.chosenComposePath) {
      // With a single candidate the server's auto-detection is kept.
      body.compose_path = this.chosenComposePath;
    }
    if (this.webService.trim()) body.web_service = this.webService.trim();
    if (this.healthcheckPath.trim()) {
      body.healthcheck_path = this.healthcheckPath.trim();
    }
    const env = this.draft.vars;
    if (this.detectRepo !== '' && env.length > 0) {
      // The recent login ran out while the form was filled in. Sending
      // would be refused and logging in from the dialog would lose the
      // form, so say so next to the variables instead.
      if (!this.session.recentlyAuthenticated) {
        this.session.expireReauth();
        this.errors = {
          env: '再ログインの期限（10 分）が切れました。再ログインするか、変数をすべて外して作成してください。',
        };
        return;
      }
      body.env = env;
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

    {{#if this.loginFirst}}
      <section
        class="pane new-app-login"
        aria-labelledby="new-app-login-title"
        data-test-new-app-login
      >
        <h2 class="pane-title" id="new-app-login-title">
          先に GitHub で再ログインしてください
        </h2>
        <p class="pane-description">
          環境変数を一緒に保存するには、直近 10
          分以内のログインが要ります。入力が消えないよう、先に再ログインしてください。
        </p>
        <div class="new-app-login-actions">
          <Button
            @variant="primary"
            data-test-new-app-login-button
            {{on "click" this.reauthenticate}}
          >GitHub で再ログイン</Button>
          <Button
            data-test-new-app-login-skip
            {{on "click" this.skipLogin}}
          >再ログインせずに入力する</Button>
        </div>
        <p class="field-hint">
          再ログインしない場合、環境変数は作成のあとに「環境変数」タブで追加します。
        </p>
      </section>
    {{else}}
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
              {{#if this.appMissing}}
                <Banner data-test-unavailable={{this.repos.unavailable}}>
                  GitHub
                  連携がこのサーバーに設定されていないため、リポジトリを一覧から選べません。公開リポジトリの
                  URL を直接指定します。
                </Banner>
              {{/if}}
              <FormField
                @label="公開リポジトリの URL"
                @hint="GitHub App をインストールしていない公開リポジトリ（OSS など）を指定します。private リポジトリは、App をそのアカウントか org にインストールして一覧から選びます。"
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
                  {{on "change" this.changeRepoUrl}}
                />
              </FormField>
              {{#unless this.appMissing}}
                <p class="new-app-repo-switch">
                  <Button
                    @size="small"
                    data-test-repo-use-list
                    {{on "click" this.useList}}
                  >インストール済みの一覧から選ぶ</Button>
                </p>
              {{/unless}}
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
                    >{{repo.full_name}}{{if
                        repo.private
                        "（private）"
                      }}</option>
                  {{/each}}
                </select>
              </FormField>
              <p class="new-app-repo-switch">
                <Button
                  @size="small"
                  data-test-repo-use-url
                  {{on "click" this.useUrl}}
                >一覧にない公開リポジトリを URL で指定</Button>
                <LinkTo
                  @route="system.admin"
                  class="new-app-repo-help"
                  data-test-repo-other-org
                >別の org のリポジトリを使うには</LinkTo>
              </p>
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
                @hint="デプロイの画面で最初に選ばれるブランチです。Compose ファイルもこのブランチから検出します。"
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
                  {{on "change" this.changeBranch}}
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
              <fieldset class="option-group" data-test-compose-source>
                <legend>Compose の置き場所</legend>
                <label class="option">
                  <input
                    type="radio"
                    name="compose_source"
                    value="repository"
                    checked={{eq this.composeSource "repository"}}
                    data-test-compose-source-option="repository"
                    {{on "change" this.setComposeSource}}
                  />
                  <span class="option-body">
                    <span class="option-label">リポジトリのファイル</span>
                    <span class="option-hint">リポジトリにある Compose
                      ファイルでデプロイします。</span>
                  </span>
                </label>
                <label class="option">
                  <input
                    type="radio"
                    name="compose_source"
                    value="inline"
                    checked={{eq this.composeSource "inline"}}
                    data-test-compose-source-option="inline"
                    {{on "change" this.setComposeSource}}
                  />
                  <span class="option-body">
                    <span class="option-label">koroku に書く</span>
                    <span class="option-hint">
                      リポジトリに Compose
                      ファイルがないときや、そのままでは動かせないときに、ここに書いたものでデプロイします。
                    </span>
                  </span>
                </label>
              </fieldset>

              {{#if this.inlineCompose}}
                <ComposeEditor
                  @value={{this.composeContent}}
                  @error={{this.errors.compose_content}}
                  @onInput={{this.setComposeContent}}
                />
              {{else}}
                <div aria-live="polite">
                  {{#if this.undetectableUrl}}
                    <Banner data-test-compose-manual>
                      GitHub 以外の URL では Compose
                      ファイルを検出できません。公開するサービスとポートを入力してください。
                    </Banner>
                  {{else if this.compose.loading}}
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
                    {{#unless this.files.length}}
                      <Banner @tone="warning" data-test-compose-missing>
                        {{#if this.detectedBranch}}ブランチ
                          <span class="mono">{{this.detectedBranch}}</span>
                          の{{/if}}リポジトリ直下に Compose
                        ファイルが見つかりませんでした。別の場所にあれば、下にパスを入力してください。リポジトリにないときは、koroku
                        に書けます。
                      </Banner>
                      <p class="new-app-compose-write">
                        <Button
                          @size="small"
                          data-test-compose-write
                          {{on "click" this.writeCompose}}
                        >koroku に Compose を書く</Button>
                      </p>
                    {{/unless}}
                    {{#if this.manualService}}
                      <Banner data-test-compose-manual>
                        サービスを読み取れなかったため、公開するサービスとポートを入力してください。
                      </Banner>
                    {{/if}}
                  {{/if}}
                </div>

                {{#if this.showsPathInput}}
                  <FormField
                    @label="Compose ファイル"
                    @hint="リポジトリのルートからのパスです（例: deploy/compose.production.yml）。空なら、ルートの compose.yaml などを探します。"
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
                      placeholder="deploy/compose.production.yml"
                      value={{this.typedPath}}
                      aria-describedby={{field.describedBy}}
                      aria-invalid={{field.invalid}}
                      data-test-field="compose_path_input"
                      {{on "input" this.setTypedPath}}
                      {{on "change" this.changeTypedPath}}
                    />
                  </FormField>
                  {{#if this.files.length}}
                    <p class="new-app-compose-switch">
                      <Button
                        @size="small"
                        data-test-compose-use-candidates
                        {{on "click" this.useCandidates}}
                      >検出した候補から選ぶ</Button>
                    </p>
                  {{/if}}
                {{else}}
                  <FormField
                    @label="Compose ファイル"
                    @hint={{if
                      this.detectedBranch
                      (concat
                        "ブランチ "
                        this.detectedBranch
                        " のリポジトリ直下で見つかったファイルです。"
                      )
                      "リポジトリ直下で見つかったファイルです。"
                    }}
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
                      <option value={{OTHER_PATH}}>その他のパスを入力…</option>
                    </select>
                  </FormField>
                {{/if}}

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

              <fieldset
                class="option-group"
                data-test-field-group="environment"
              >
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

          {{#if this.showEnv}}
            <div class="form-group" data-test-env-step>
              <div>
                <h2 class="form-group-title">4. 環境変数</h2>
                <p class="form-group-description">
                  リポジトリのファイルから推し量った、本番で動かすのにたいてい要る変数です。アプリと一緒に保存され、最初のデプロイから使われます。作成後は「環境変数」タブで変えられます。
                </p>
              </div>
              <div class="form-group-body">
                <EnvPresetsEditor
                  @draft={{this.draft}}
                  @presets={{this.presetResult}}
                  @loading={{this.presets.loading}}
                  @error={{this.errors.env}}
                />
              </div>
            </div>
          {{/if}}
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
          <LinkTo
            @route="index"
            class={{buttonClass "default"}}
            data-test-cancel
          >
            キャンセル
          </LinkTo>
        </div>
      </form>
    {{/if}}
  </template>
}
