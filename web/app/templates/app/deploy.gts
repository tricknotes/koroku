import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { LinkTo } from '@ember/routing';
import { eq, fn } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from 'koroku-console/components/async-view';
import Badge from 'koroku-console/components/badge';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import DeploySettings from 'koroku-console/components/deploy-settings';
import DeploymentDetail from 'koroku-console/components/deployment-detail';
import EnvRequirementsWarning from 'koroku-console/components/env-requirements-warning';
import EnvironmentBadge from 'koroku-console/components/environment-badge';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import PromotionPanel from 'koroku-console/components/promotion-panel';
import Icon from 'koroku-console/components/icon';
import RelativeTime from 'koroku-console/components/relative-time';
import RoleNote from 'koroku-console/components/role-note';
import RowList from 'koroku-console/components/row-list';
import StatusIndicator from 'koroku-console/components/status-indicator';
import focusSection from 'koroku-console/modifiers/focus-section';
import scrollIntoViewWhen from 'koroku-console/modifiers/scroll-into-view-when';
import ReviewAppsPanel from 'koroku-console/components/review-apps-panel';
import ReviewAppsPermission from 'koroku-console/components/review-apps-permission';
import { hasReviewTab } from 'koroku-console/utils/review';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type AppContext from 'koroku-console/utils/app-context';
import type AppDeployController from 'koroku-console/controllers/app/deploy';
import { inputValue, preventDefault, setValue } from 'koroku-console/utils/dom';
import {
  firstLine,
  formatDuration,
  hostOf,
  isFullSha,
  shortSha,
} from 'koroku-console/utils/format';
import { ApiError } from 'koroku-console/utils/errors';
import Loadable from 'koroku-console/utils/loadable';
import SettingsEditor, {
  DEPLOY_SETTINGS_FIELDS,
} from 'koroku-console/utils/settings-editor';
import { can } from 'koroku-console/utils/roles';
import {
  canRollBack,
  isReleaseGone,
  isRollback,
  releaseId,
  rollbackSummary,
} from 'koroku-console/utils/rollback';
import { isPush, pushTitle } from 'koroku-console/utils/trigger';
import {
  DEPLOYMENT_STATUS_KINDS,
  DEPLOYMENT_STATUS_LABELS,
  isActiveDeployment,
} from 'koroku-console/utils/status';
import type {
  App,
  Branch,
  Commit,
  Deployment,
  EnvRequirements,
  PendingAutoDeploy,
} from 'koroku-console/utils/types';

const POLL_INTERVAL = 3000;

const SKELETON_ROWS = [1, 2, 3];

interface DeployTarget {
  ref?: string;
  sha?: string;
  /** What is shown in the confirmation dialog. */
  label: string;
}

function statusKind(deployment: Deployment) {
  return DEPLOYMENT_STATUS_KINDS[deployment.status] ?? 'stopped';
}

function statusLabel(deployment: Deployment): string {
  return DEPLOYMENT_STATUS_LABELS[deployment.status] ?? deployment.status;
}

/** A successful deployment is the normal case and stays in the background. */
function isUneventful(deployment: Deployment): boolean {
  return deployment.status === 'succeeded';
}

function commitLabel(commit: Commit): string {
  return `${shortSha(commit.sha)} ${firstLine(commit.message)}（${commit.author}）`;
}

/** What a history row is called: its commit message, or its number. */
function rowTitle(deployment: Deployment): string {
  return firstLine(deployment.commit_message) || `デプロイ #${deployment.id}`;
}

function startedAt(deployment: Deployment): string | null {
  return deployment.started_at ?? deployment.queued_at;
}

function duration(deployment: Deployment): string {
  return formatDuration(deployment.started_at, deployment.finished_at);
}

function detailId(deployment: Deployment): string {
  return `deployment-${deployment.id}-detail`;
}

/** "9fceb02 Fix login": the commit of a deployment, in one line. */
function commitText(deployment: Deployment): string {
  return `${shortSha(deployment.commit_sha)} ${firstLine(deployment.commit_message)}`.trim();
}

const GONE_HINT =
  'この版のリリースはもう保持していないため、コミットを取得・ビルドし直してデプロイします。';

interface Signature {
  Args: { model: AppContext; controller: AppDeployController };
}

export default class AppDeployTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  branches = new Loadable<Branch[]>(() =>
    this.api.listBranches(this.app.repo_full_name),
  );
  commits = new Loadable<Commit[]>(() =>
    this.api.listCommits(this.app.repo_full_name, this.branch),
  );
  history = new Loadable<Deployment[]>(async () => {
    const { deployments, pendingAutoDeploy } = await this.api.deploymentHistory(
      this.app.slug,
    );
    this.pendingAutoDeploy = pendingAutoDeploy;
    return deployments;
  });
  /**
   * A push received while a command runs on the app, deployed once the
   * command ends (ARCHITECTURE 5.2).
   */
  @tracked pendingAutoDeploy: PendingAutoDeploy | null = null;
  /** What the commit picked in the form needs (ARCHITECTURE 9.4). */
  targetRequirements = new Loadable<EnvRequirements>(() =>
    this.api.getEnvRequirements(this.app.slug, this.checkedQuery),
  );
  /** What the deployment waiting in the confirmation dialog needs. */
  pendingRequirements = new Loadable<EnvRequirements>(() =>
    this.api.getEnvRequirements(this.app.slug, this.pendingTarget()),
  );
  /** What `targetRequirements` was loaded for: a commit, or a typed ref. */
  private checkedQuery: { ref?: string; sha?: string } = {};

  @tracked branch: string;
  /** Empty means "the head of the branch". */
  @tracked sha = '';
  @tracked manualRef: string;

  @tracked pending: DeployTarget | null = null;
  @tracked deploying = false;
  @tracked canceling: number | null = null;
  @tracked error: unknown = null;

  /** The deployment whose steps are open below its row. */
  @tracked selectedId: number | null = null;
  /** The deployment opened from a link, scrolled into view once. */
  @tracked scrollTargetId: number | null = null;

  isScrollTarget = (deployment: Deployment): boolean =>
    this.scrollTargetId === deployment.id;

  /** The deployment whose release waits in the rollback dialog. */
  @tracked rollbackTarget: Deployment | null = null;
  @tracked rollingBack = false;
  /** Why the rollback in the dialog did not start. */
  @tracked rollbackError: unknown = null;

  /** 「デプロイの設定」, saved on its own (ARCHITECTURE 9.1). */
  settingsEditor: SettingsEditor;
  /** A deployment held back because the deploy settings are unsaved. */
  @tracked unsavedTarget: DeployTarget | null = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.settingsEditor = new SettingsEditor(
      this.api,
      args.model,
      DEPLOY_SETTINGS_FIELDS,
    );
    this.branch = this.app.default_branch;
    this.manualRef = this.app.default_branch;

    void this.loadHistory();
    if (this.app.repo_full_name) {
      // Without the lists (the App cannot see the repository) the ref is
      // typed; check the default one right away.
      void this.branches.load().then(() => {
        if (this.manual) this.checkManual();
      });
      void this.loadCommits();
    } else {
      this.checkManual();
    }

    this.poller.every(this, POLL_INTERVAL, () => this.poll());
  }

  /** Deploying, rolling back, canceling and promoting need deploy. */
  get canDeploy(): boolean {
    return can(this.app, 'deploy');
  }

  get readOnly(): boolean {
    return !this.canDeploy;
  }

  get app(): App {
    return this.args.model.app;
  }

  /**
   * The review apps of pull requests, under the history (ARCHITECTURE
   * 9.9): apps they can be made from, and apps that still have some.
   */
  get showsReviewApps(): boolean {
    return hasReviewTab(this.app);
  }

  get focusReviewApps(): boolean {
    return this.args.controller.section === 'review-apps';
  }

  /**
   * Branch and commit lists need the GitHub App. Without them (an app
   * created from a URL, GitHub not configured, a failed request) the ref
   * or SHA is typed by hand.
   */
  get manual(): boolean {
    return !this.app.repo_full_name || this.branches.error !== null;
  }

  get branchNames(): string[] {
    const names = (this.branches.value ?? []).map((b) => b.name);
    return names.includes(this.branch) ? names : [this.branch, ...names];
  }

  /** The commit the form would deploy, once the list for the branch is in. */
  get targetCommit(): Commit | null {
    if (this.manual || this.commits.loading) return null;
    const commits = this.commits.value ?? [];
    if (!this.sha) return commits[0] ?? null;
    return commits.find((commit) => commit.sha === this.sha) ?? null;
  }

  get targetIsRunning(): boolean {
    const current = this.app.current_deployment;
    return current !== null && current.commit_sha === this.targetCommit?.sha;
  }

  get deployments(): Deployment[] {
    return this.history.value ?? [];
  }

  /** Loaded, and nothing was ever deployed: the form is the one next step. */
  get isEmpty(): boolean {
    return this.history.loaded && this.deployments.length === 0;
  }

  get hasActive(): boolean {
    return this.deployments.some((d) => isActiveDeployment(d));
  }

  /**
   * The header noticed a deployment this list has not seen yet (started in
   * another window, or finished since the last look).
   */
  get behindApp(): boolean {
    const latest = this.app.latest_deployment;
    const head = this.deployments[0];
    if (!latest || !this.history.loaded) return false;
    return head?.id !== latest.id || head.status !== latest.status;
  }

  get appHost(): string {
    return hostOf(this.app.primary_url || this.app.url);
  }

  /** The release that is serving right now. */
  isLive = (deployment: Deployment): boolean =>
    this.app.desired_state === 'running' &&
    this.app.current_deployment?.id === deployment.id;

  isSelected = (deployment: Deployment): boolean =>
    this.selectedId === deployment.id;

  /** "この版に戻す" applies: kept, finished, and not what runs now. */
  canRollBack = (deployment: Deployment): boolean =>
    canRollBack(deployment, this.app.current_deployment);

  /** Once ran, but only a new build of its commit can bring it back. */
  isReleaseGone = (deployment: Deployment): boolean =>
    isReleaseGone(deployment, this.app.current_deployment);

  /** The detail says what can be done with the release of the deployment. */
  hasReleaseActions = (deployment: Deployment): boolean =>
    this.canRollBack(deployment) || this.isReleaseGone(deployment);

  showsReleaseActions = (deployment: Deployment): boolean =>
    this.canDeploy && this.hasReleaseActions(deployment);

  private async loadHistory(): Promise<void> {
    await this.history.load();
    // A link to one deployment (from GitHub's deployments) opens it.
    const linked = Number(this.args.controller.deployment);
    if (
      this.selectedId === null &&
      linked > 0 &&
      this.deployments.some((d) => d.id === linked)
    ) {
      this.selectedId = linked;
      this.scrollTargetId = linked;
    }
    // Otherwise open what most likely brought the reader here: a deployment
    // that is in progress, or the latest one when it failed.
    const head = this.deployments[0];
    if (
      this.selectedId === null &&
      head &&
      (isActiveDeployment(head) || head.status === 'failed')
    ) {
      this.selectedId = head.id;
    }
  }

  /** Keeps the history current while something is queued or running. */
  async poll(): Promise<void> {
    if (!this.hasActive && !this.behindApp && !this.pendingAutoDeploy) return;
    await this.refresh();
  }

  refresh = async (): Promise<void> => {
    const wasActive = this.hasActive;
    await this.history.load({ quiet: true });
    if (wasActive && !this.hasActive) await this.args.model.reload();
  };

  /**
   * Requirements are read from GitHub, with the App or anonymously for
   * public repositories; the server says when it cannot (ARCHITECTURE 9.4).
   */
  get checksRequirements(): boolean {
    return this.app.env_source !== 'off';
  }

  private async loadCommits(): Promise<void> {
    await this.commits.load();
    this.checkTarget();
  }

  /** Compares the commit the form would deploy with the saved variables. */
  private checkTarget(): void {
    const sha = this.targetCommit?.sha ?? '';
    if (!this.checksRequirements || !sha || sha === this.checkedQuery.sha)
      return;
    this.checkedQuery = { sha };
    void this.targetRequirements.load();
  }

  /** The same for a ref or SHA typed by hand, once the field is left. */
  checkManual = () => {
    const value = this.manualRef.trim();
    if (!this.checksRequirements || !value) return;
    const query = isFullSha(value)
      ? { sha: value.toLowerCase() }
      : { ref: value };
    if (
      query.sha === this.checkedQuery.sha &&
      query.ref === this.checkedQuery.ref
    )
      return;
    this.checkedQuery = query;
    void this.targetRequirements.load();
  };

  /** Shown only while it is about the commit still picked. */
  get targetRequirementsValue(): EnvRequirements | null {
    const value = this.targetRequirements.value;
    if (!value || this.targetRequirements.loading) return null;
    if (this.manual) {
      const typed = this.manualRef.trim();
      const matches = isFullSha(typed)
        ? value.commit_sha === typed.toLowerCase()
        : value.ref === typed;
      return matches ? value : null;
    }
    return value.commit_sha === this.targetCommit?.sha ? value : null;
  }

  private pendingTarget(): { ref?: string; sha?: string } {
    const target = this.pending;
    if (!target) return {};
    return target.sha ? { sha: target.sha } : { ref: target.ref };
  }

  get pendingRequirementsValue(): EnvRequirements | null {
    return this.pendingRequirements.loading
      ? null
      : this.pendingRequirements.value;
  }

  selectBranch = (event: Event) => {
    this.branch = inputValue(event);
    this.sha = '';
    void this.loadCommits();
  };

  selectCommit = (event: Event) => {
    this.sha = inputValue(event);
    this.checkTarget();
  };

  submit = () => {
    this.requestDeploy(this.formTarget());
  };

  private formTarget(): DeployTarget {
    if (this.manual) {
      const value = this.manualRef.trim();
      return isFullSha(value)
        ? { sha: value, label: shortSha(value) }
        : { ref: value, label: value };
    }
    if (this.sha) {
      return {
        ref: this.branch,
        sha: this.sha,
        label: `${this.branch}（${shortSha(this.sha)}）`,
      };
    }
    return { ref: this.branch, label: `${this.branch} の先頭` };
  }

  redeploy = (deployment: Deployment) => {
    this.requestDeploy({
      ref: deployment.ref || undefined,
      sha: deployment.commit_sha,
      label: `${shortSha(deployment.commit_sha)} ${firstLine(deployment.commit_message)}`,
    });
  };

  /**
   * A deployment uses the saved settings: with unsaved deploy settings it
   * asks first (ARCHITECTURE 9.1). Production apps then go through the
   * confirmation dialog.
   */
  private requestDeploy(target: DeployTarget): void {
    this.error = null;
    if (!target.ref && !target.sha) return;
    if (this.settingsEditor.dirty) {
      this.unsavedTarget = target;
      return;
    }
    this.proceed(target);
  }

  saveAndDeploy = async () => {
    const target = this.unsavedTarget;
    this.unsavedTarget = null;
    if (target && (await this.settingsEditor.save())) this.proceed(target);
  };

  deployWithoutSaving = () => {
    const target = this.unsavedTarget;
    this.unsavedTarget = null;
    if (target) this.proceed(target);
  };

  cancelUnsaved = () => {
    this.unsavedTarget = null;
  };

  private proceed(target: DeployTarget): void {
    if (this.args.model.isProduction) {
      this.pending = target;
      if (this.checksRequirements) void this.pendingRequirements.load();
    } else {
      void this.deploy(target);
    }
  }

  confirmDeploy = async () => {
    if (this.pending) await this.deploy(this.pending);
    this.pending = null;
  };

  cancelDeploy = () => {
    this.pending = null;
  };

  private async deploy(target: DeployTarget): Promise<void> {
    this.deploying = true;
    this.error = null;
    try {
      const body: { ref?: string; sha?: string } = {};
      if (target.ref) body.ref = target.ref;
      if (target.sha) body.sha = target.sha;
      const deployment = await this.api.createDeployment(this.app.slug, body);
      await this.history.load({ quiet: true });
      this.selectedId = deployment.id;
      await this.args.model.reload();
    } catch (error) {
      this.error = error;
    } finally {
      this.deploying = false;
    }
  }

  cancel = async (deployment: Deployment) => {
    this.canceling = deployment.id;
    this.error = null;
    try {
      await this.api.cancelDeployment(deployment.id);
    } catch (error) {
      this.error = error;
    } finally {
      this.canceling = null;
    }
    await this.refresh();
  };

  /** Opens the confirmation for starting a kept release again. */
  requestRollback = (deployment: Deployment) => {
    this.error = null;
    this.rollbackError = null;
    this.rollbackTarget = deployment;
  };

  cancelRollback = () => {
    this.rollbackTarget = null;
    this.rollbackError = null;
  };

  get rollbackSourceId(): number | null {
    return this.rollbackTarget ? releaseId(this.rollbackTarget) : null;
  }

  get rollbackTitle(): string {
    return this.args.model.isProduction
      ? '本番アプリをこの版に戻しますか？'
      : 'この版に戻しますか？';
  }

  /**
   * Starts the rollback and opens its live log, like a deployment. Errors
   * stay in the dialog, next to what was asked; a re-login is asked for by
   * its own dialog, so this one gives way.
   */
  confirmRollback = async () => {
    const target = this.rollbackTarget;
    if (!target) return;
    this.rollingBack = true;
    this.rollbackError = null;
    try {
      const deployment = await this.api.rollBack(
        this.app.slug,
        releaseId(target),
      );
      this.rollbackTarget = null;
      await this.history.load({ quiet: true });
      this.selectedId = deployment.id;
      await this.args.model.reload();
    } catch (error) {
      if (error instanceof ApiError && error.code === 'reauth_required') {
        this.rollbackTarget = null;
        return;
      }
      this.rollbackError = error;
      // The release is gone since the list was read: show that in it.
      if (error instanceof ApiError && error.code === 'release_unavailable') {
        await this.history.load({ quiet: true });
      }
    } finally {
      this.rollingBack = false;
    }
  };

  /** A promotion was queued from the panel: show it like a deployment. */
  promoted = async (deployment: Deployment) => {
    await this.history.load({ quiet: true });
    this.selectedId = deployment.id;
    await this.args.model.reload();
  };

  /** Opens the steps of a deployment, or closes them again. */
  toggle = (deployment: Deployment) => {
    this.selectedId = this.isSelected(deployment) ? null : deployment.id;
  };

  <template>
    {{#if @model.isProduction}}
      {{#if this.canDeploy}}
        <PromotionPanel @app={{this.app}} @onPromoted={{this.promoted}} />
      {{/if}}
    {{/if}}

    {{#if this.canDeploy}}
      <section
        class="deploy-form-section"
        aria-labelledby="deploy-title"
        data-test-deploy-panel
      >
        {{#if this.isEmpty}}
          <div class="deploy-intro" data-test-no-deployments>
            <h2 id="deploy-title">まだデプロイしていません</h2>
            <p>
              ブランチを選んでデプロイすると、<span
                class="deploy-host"
              >{{this.appHost}}</span>
              で公開されます。
            </p>
          </div>
        {{else}}
          <h2 id="deploy-title" class="visually-hidden">デプロイする</h2>
        {{/if}}

        <form
          class="deploy-form"
          data-test-deploy-form
          {{on "submit" (preventDefault this.submit)}}
        >
          {{#if this.manual}}
            <FormField
              class="deploy-field-ref"
              @label="ブランチ / タグ / コミット SHA"
              @hint="ブランチとコミットの一覧を取得できないため、直接入力します。40 桁の SHA はそのコミットをデプロイします。"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                class="mono"
                spellcheck="false"
                autocomplete="off"
                aria-describedby={{field.describedBy}}
                value={{this.manualRef}}
                data-deploy-focus
                data-test-deploy-ref
                {{on "input" (setValue this "manualRef")}}
                {{on "change" this.checkManual}}
              />
            </FormField>
          {{else}}
            <FormField class="deploy-field-branch" @label="ブランチ" as |field|>
              <select
                id={{field.id}}
                data-deploy-focus
                data-test-deploy-branch
                {{on "change" this.selectBranch}}
              >
                {{#each this.branchNames as |name|}}
                  <option value={{name}} selected={{eq name this.branch}}>
                    {{name}}
                  </option>
                {{/each}}
              </select>
            </FormField>

            <FormField class="deploy-field-commit" @label="コミット" as |field|>
              <select
                id={{field.id}}
                data-test-deploy-commit
                {{on "change" this.selectCommit}}
              >
                <option value="" selected={{eq this.sha ""}}>
                  ブランチの先頭
                </option>
                {{#each this.commits.value key="sha" as |commit|}}
                  <option
                    value={{commit.sha}}
                    selected={{eq commit.sha this.sha}}
                  >
                    {{commitLabel commit}}
                  </option>
                {{/each}}
              </select>
            </FormField>
          {{/if}}

          <Button
            @type="submit"
            @variant="primary"
            @busy={{this.deploying}}
            @busyLabel="開始中…"
            class="deploy-submit"
            data-test-deploy-submit
          >デプロイ</Button>

          <div class="deploy-form-notes">
            {{#if this.commits.error}}
              <p data-test-commits-error>
                コミット一覧を取得できませんでした。{{this.commits.errorMessage}}
              </p>
            {{else if this.targetCommit}}
              <p class="deploy-target" data-test-deploy-target>
                <span class="deploy-target-label">デプロイ対象</span>
                <span class="deploy-sha" title={{this.targetCommit.sha}}>
                  {{shortSha this.targetCommit.sha}}
                </span>
                <span class="deploy-target-message">
                  {{firstLine this.targetCommit.message}}
                </span>
                <span class="deploy-target-meta">
                  {{this.targetCommit.author}}
                  <span class="deploy-target-dot" aria-hidden="true">·</span>
                  <RelativeTime @time={{this.targetCommit.date}} />
                </span>
                {{#if this.targetIsRunning}}
                  <span
                    class="deploy-target-meta"
                    data-test-target-running
                  >（稼働中のコミットと同じです）</span>
                {{/if}}
              </p>
            {{/if}}
            {{#if @model.isProduction}}
              <p data-test-production-hint>
                本番アプリのため、開始前に確認します。
              </p>
            {{/if}}
          </div>
        </form>

        <div class="deploy-env-warning" aria-live="polite">
          <EnvRequirementsWarning
            @slug={{this.app.slug}}
            @requirements={{this.targetRequirementsValue}}
          />
        </div>

        <div class="deploy-form-error" aria-live="polite">
          <ErrorMessage @error={{this.error}} @showDetails={{true}} />
        </div>
      </section>
    {{else}}
      <RoleNote
        @needed="deploy"
        @what="デプロイ、ロールバック、キャンセル"
        class="deploy-readonly"
        data-test-deploy-readonly
      />
    {{/if}}

    {{#if this.pendingAutoDeploy}}
      <Banner
        @title="コマンドの実行が終わったら自動デプロイします"
        class="deploy-pending"
        data-test-pending-auto-deploy
      >
        <:default>
          <p>
            実行中のコマンドがあるため、push
            されたコミットのデプロイを預かっています。実行が終わると、ふつうのデプロイとして積まれます。後から
            push されたときは、新しいコミットに置き換わります。
          </p>
          <p class="deploy-pending-commit">
            <code>{{shortSha this.pendingAutoDeploy.commit_sha}}</code>
            {{firstLine this.pendingAutoDeploy.commit_message}}
          </p>
          <p class="deploy-pending-meta">
            {{this.pendingAutoDeploy.ref}}
            {{#if this.pendingAutoDeploy.pusher}}
              ・ push:
              {{this.pendingAutoDeploy.pusher}}
            {{/if}}
            {{#if this.pendingAutoDeploy.workflow}}
              ・ ワークフロー
              {{this.pendingAutoDeploy.workflow}}
              の成功後
            {{/if}}
            ・ 受け取り
            <RelativeTime @time={{this.pendingAutoDeploy.received_at}} />
          </p>
        </:default>
        <:actions>
          <LinkTo @route="app.tasks" @model={{this.app.slug}}>
            実行中のコマンドを見る
          </LinkTo>
        </:actions>
      </Banner>
    {{/if}}

    {{#unless this.isEmpty}}
      <section class="deploy-history" aria-labelledby="history-title">
        <h2 id="history-title">履歴</h2>

        {{#if this.history.stale}}
          <Banner @tone="warning" data-test-stale>
            最新の状態を取得できていません。表示は最後に取得できた内容です。
          </Banner>
        {{/if}}

        <AsyncView
          @data={{this.history}}
          @errorTitle="デプロイの履歴を取得できませんでした"
        >
          <:loading>
            <div
              class="row-list deploy-rows"
              role="status"
              aria-label="読み込み中"
              data-test-loading
            >
              {{#each SKELETON_ROWS as |n|}}
                <div class="row" aria-hidden="true" data-skeleton={{n}}>
                  <div><span class="skeleton skeleton-fixed"></span></div>
                  <div><span class="skeleton skeleton-wide"></span></div>
                  <div><span class="skeleton skeleton-fixed"></span></div>
                  <div><span class="skeleton skeleton-fixed"></span></div>
                </div>
              {{/each}}
            </div>
          </:loading>

          <:default as |deployments|>
            <RowList class="deploy-rows" @label="デプロイの履歴">
              <:header>
                <span>状態</span>
                <span>コミット</span>
                <span>所要時間</span>
                <span>開始</span>
              </:header>
              <:rows>
                {{#each deployments key="id" as |deployment|}}
                  <li
                    class="deploy-item
                      {{if (this.isSelected deployment) 'is-open'}}"
                    data-test-deployment={{deployment.id}}
                    {{scrollIntoViewWhen (this.isScrollTarget deployment)}}
                  >
                    <div class="row deploy-row">
                      <div class="deploy-cell-state">
                        <StatusIndicator
                          @kind={{statusKind deployment}}
                          @label={{statusLabel deployment}}
                          @quiet={{isUneventful deployment}}
                          data-test-deployment-status
                        />
                        {{#if (this.isLive deployment)}}
                          <Badge
                            @tone="ok"
                            title="いま応答しているリリースです"
                            data-test-live
                          >Live</Badge>
                        {{/if}}
                      </div>

                      <div class="deploy-cell-commit">
                        <button
                          type="button"
                          class="row-link deploy-toggle"
                          aria-expanded={{if
                            (this.isSelected deployment)
                            "true"
                            "false"
                          }}
                          aria-controls={{detailId deployment}}
                          data-test-show-log
                          {{on "click" (fn this.toggle deployment)}}
                        >{{rowTitle deployment}}</button>
                        <span class="deploy-meta">
                          {{#if (isRollback deployment)}}
                            <Badge
                              @tone="info"
                              title="保持している版を、ビルドせずに起動し直したデプロイです"
                              data-test-rollback-badge
                            >ロールバック</Badge>
                          {{/if}}
                          <span
                            class="deploy-sha"
                            title={{deployment.commit_sha}}
                          >{{shortSha deployment.commit_sha}}</span>
                          {{#if (isRollback deployment)}}
                            <span data-test-rollback-summary>
                              {{rollbackSummary deployment}}
                            </span>
                          {{else if deployment.ref}}
                            <span>{{deployment.ref}}</span>
                          {{/if}}
                          {{#if deployment.promoted_from}}
                            <Badge
                              @tone="info"
                              title="{{deployment.promoted_from.app_slug}} のデプロイ #{{deployment.promoted_from.deployment_id}} を昇格"
                              data-test-promoted-from
                            >{{deployment.promoted_from.app_slug}}
                              から昇格</Badge>
                          {{/if}}
                          {{#if (isPush deployment)}}
                            <Badge
                              @tone="info"
                              title={{pushTitle deployment}}
                              data-test-push-badge
                            >push で自動デプロイ</Badge>
                          {{/if}}
                          {{#if deployment.actor}}
                            <span
                              class="deploy-actor"
                              title="デプロイした人"
                              data-test-deploy-actor
                            >実行: {{deployment.actor.login}}</span>
                          {{/if}}
                        </span>
                        {{#if deployment.error}}
                          <span class="deploy-row-error" data-test-row-error>
                            {{deployment.error}}
                          </span>
                        {{/if}}
                      </div>

                      <div class="deploy-cell-duration">
                        {{#if (duration deployment)}}
                          <span class="deploy-cell-label">所要</span>
                          <span data-test-duration>
                            {{duration deployment}}
                          </span>
                        {{/if}}
                      </div>

                      <div class="deploy-cell-started">
                        <span class="visually-hidden">開始</span>
                        <RelativeTime
                          @time={{startedAt deployment}}
                          data-test-started
                        />
                      </div>

                      <div class="deploy-cell-actions">
                        {{#if this.readOnly}}
                          {{! Looking only: no actions. }}
                        {{else if (isActiveDeployment deployment)}}
                          <Button
                            @size="small"
                            @disabled={{eq this.canceling deployment.id}}
                            class="row-action"
                            data-test-cancel
                            {{on "click" (fn this.cancel deployment)}}
                          >キャンセル</Button>
                        {{else if (this.canRollBack deployment)}}
                          <Button
                            @size="small"
                            @variant="ghost"
                            @disabled={{this.deploying}}
                            class="row-action"
                            title="保持しているこの版を、ビルドせずに起動し直します"
                            data-test-rollback
                            {{on "click" (fn this.requestRollback deployment)}}
                          >この版に戻す</Button>
                        {{else if deployment.commit_sha}}
                          <Button
                            @size="small"
                            @variant="ghost"
                            @disabled={{this.deploying}}
                            class="row-action"
                            title={{if
                              (this.isReleaseGone deployment)
                              GONE_HINT
                            }}
                            data-test-redeploy
                            {{on "click" (fn this.redeploy deployment)}}
                          >このコミットを再デプロイ</Button>
                        {{/if}}
                        <Icon @name="chevron-down" class="deploy-chevron" />
                      </div>
                    </div>

                    {{#if (this.isSelected deployment)}}
                      <DeploymentDetail
                        id={{detailId deployment}}
                        @deployment={{deployment}}
                        @onLogEnd={{this.refresh}}
                      >
                        {{#if (this.showsReleaseActions deployment)}}
                          <div class="deploy-release" data-test-release-actions>
                            {{#if (this.canRollBack deployment)}}
                              <p data-test-release-kept>
                                この版のリリースを保持しています。ビルドせずに、すぐ起動し直せます。
                              </p>
                              <div class="deploy-release-buttons">
                                <Button
                                  @size="small"
                                  @disabled={{this.deploying}}
                                  data-test-detail-rollback
                                  {{on
                                    "click"
                                    (fn this.requestRollback deployment)
                                  }}
                                >この版に戻す</Button>
                                <Button
                                  @size="small"
                                  @variant="ghost"
                                  @disabled={{this.deploying}}
                                  data-test-detail-redeploy
                                  {{on "click" (fn this.redeploy deployment)}}
                                >このコミットを再デプロイ</Button>
                              </div>
                            {{else}}
                              <p data-test-release-gone>
                                この版のリリースはもう保持していないため、すぐには戻せません。戻すには、このコミットを再デプロイします（取得とビルドからやり直すため、数分かかります）。
                              </p>
                              <div class="deploy-release-buttons">
                                <Button
                                  @size="small"
                                  @disabled={{this.deploying}}
                                  data-test-detail-redeploy
                                  {{on "click" (fn this.redeploy deployment)}}
                                >このコミットを再デプロイ</Button>
                              </div>
                            {{/if}}
                          </div>
                        {{/if}}
                      </DeploymentDetail>
                    {{/if}}
                  </li>
                {{/each}}
              </:rows>
            </RowList>
          </:default>
        </AsyncView>
      </section>
    {{/unless}}

    {{#if this.showsReviewApps}}
      <div class="deploy-review" data-test-review-tab>
        <ReviewAppsPanel
          id="review-apps"
          tabindex="-1"
          @parent={{this.app}}
          @onCreated={{@model.reload}}
          {{focusSection this.focusReviewApps}}
        />
        <ReviewAppsPermission @context={{@model}} />
      </div>
    {{/if}}

    <DeploySettings
      @editor={{this.settingsEditor}}
      @section={{@controller.section}}
    />

    {{#if this.unsavedTarget}}
      <ConfirmDialog
        @title="保存していない変更があります"
        @confirmLabel="保存してデプロイ"
        @altLabel="保存せずにデプロイ"
        @busy={{this.settingsEditor.saving}}
        @onConfirm={{this.saveAndDeploy}}
        @onAlt={{this.deployWithoutSaving}}
        @onCancel={{this.cancelUnsaved}}
      >
        <p data-test-unsaved-dialog>
          デプロイの設定に保存していない変更があります。デプロイは保存済みの設定で行われます。保存してからデプロイしますか？保存せずにデプロイしても、変更は欄に残ります。
        </p>
      </ConfirmDialog>
    {{/if}}

    {{#if this.pending}}
      <ConfirmDialog
        @title="本番アプリにデプロイしますか？"
        @confirmLabel="デプロイする"
        @busy={{this.deploying}}
        @onConfirm={{this.confirmDeploy}}
        @onCancel={{this.cancelDeploy}}
      >
        <p>本番アプリ
          <strong>{{this.app.slug}}</strong>
          に
          <strong data-test-dialog-target>{{this.pending.label}}</strong>
          をデプロイします。</p>
        <p
        >コンテナの入れ替えで数秒の断があります。起動に失敗した場合は直前の成功リリースに戻ります。</p>
        <EnvRequirementsWarning
          @slug={{this.app.slug}}
          @requirements={{this.pendingRequirementsValue}}
          @inDialog={{true}}
        />
      </ConfirmDialog>
    {{/if}}

    {{#if this.rollbackTarget}}
      <ConfirmDialog
        @title={{this.rollbackTitle}}
        @confirmLabel="この版に戻す"
        @busy={{this.rollingBack}}
        @onConfirm={{this.confirmRollback}}
        @onCancel={{this.cancelRollback}}
      >
        <div data-test-rollback-dialog>
          <p>
            {{#if @model.isProduction}}
              <EnvironmentBadge
                @environment="production"
                data-test-rollback-production
              />
            {{/if}}
            <strong>{{this.app.slug}}</strong>
            を、デプロイ
            <strong data-test-rollback-dialog-target>#{{this.rollbackSourceId}}
              {{commitText this.rollbackTarget}}</strong>
            の版に戻します。
          </p>
          <ul class="dialog-points">
            <li
            >ビルドはしません。保持しているイメージをそのまま起動します。コンテナの入れ替えで数秒の断があります。</li>
            <li
              data-test-rollback-env
            >環境変数は、この版が動いていた時の値に戻ります。いま保存している値は残り、環境変数のタブに未反映の変更として表示されます。</li>
            <li data-test-rollback-migration>DB
              のマイグレーションは元に戻りません。この版より後にスキーマを変えていると、起動に失敗することがあります。失敗したときは、いま動いている版に自動で戻ります。</li>
          </ul>
          <ErrorMessage @error={{this.rollbackError}} />
        </div>
      </ConfirmDialog>
    {{/if}}
  </template>
}
