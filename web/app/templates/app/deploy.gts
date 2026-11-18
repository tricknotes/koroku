import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from 'koroku-console/components/async-view';
import Badge from 'koroku-console/components/badge';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import DeploymentDetail from 'koroku-console/components/deployment-detail';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import RelativeTime from 'koroku-console/components/relative-time';
import RowList from 'koroku-console/components/row-list';
import StatusIndicator from 'koroku-console/components/status-indicator';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type AppContext from 'koroku-console/utils/app-context';
import { inputValue, preventDefault, setValue } from 'koroku-console/utils/dom';
import {
  firstLine,
  formatDuration,
  hostOf,
  isFullSha,
  shortSha,
} from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
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

interface Signature {
  Args: { model: AppContext };
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
  history = new Loadable<Deployment[]>(() =>
    this.api.listDeployments(this.app.slug),
  );

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

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.branch = this.app.default_branch;
    this.manualRef = this.app.default_branch;

    void this.loadHistory();
    if (this.app.repo_full_name) {
      void this.branches.load();
      void this.commits.load();
    }

    this.poller.every(this, POLL_INTERVAL, () => this.poll());
  }

  get app(): App {
    return this.args.model.app;
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

  private async loadHistory(): Promise<void> {
    await this.history.load();
    // Open what most likely brought the reader here: a deployment that is
    // in progress, or the latest one when it failed.
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
    if (!this.hasActive && !this.behindApp) return;
    await this.refresh();
  }

  refresh = async (): Promise<void> => {
    const wasActive = this.hasActive;
    await this.history.load({ quiet: true });
    if (wasActive && !this.hasActive) await this.args.model.reload();
  };

  selectBranch = (event: Event) => {
    this.branch = inputValue(event);
    this.sha = '';
    void this.commits.load();
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

  /** Production apps go through the confirmation dialog first. */
  private requestDeploy(target: DeployTarget): void {
    this.error = null;
    if (!target.ref && !target.sha) return;
    if (this.args.model.isProduction) {
      this.pending = target;
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

  /** Opens the steps of a deployment, or closes them again. */
  toggle = (deployment: Deployment) => {
    this.selectedId = this.isSelected(deployment) ? null : deployment.id;
  };

  <template>
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
              {{on "change" (setValue this "sha")}}
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

      <div class="deploy-form-error" aria-live="polite">
        <ErrorMessage @error={{this.error}} @showDetails={{true}} />
      </div>
    </section>

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
                          <span
                            class="deploy-sha"
                            title={{deployment.commit_sha}}
                          >{{shortSha deployment.commit_sha}}</span>
                          {{#if deployment.ref}}
                            <span>{{deployment.ref}}</span>
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
                        {{#if (isActiveDeployment deployment)}}
                          <Button
                            @size="small"
                            @disabled={{eq this.canceling deployment.id}}
                            class="row-action"
                            data-test-cancel
                            {{on "click" (fn this.cancel deployment)}}
                          >キャンセル</Button>
                        {{else if deployment.commit_sha}}
                          <Button
                            @size="small"
                            @variant="ghost"
                            @disabled={{this.deploying}}
                            class="row-action"
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
                      />
                    {{/if}}
                  </li>
                {{/each}}
              </:rows>
            </RowList>
          </:default>
        </AsyncView>
      </section>
    {{/unless}}

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
      </ConfirmDialog>
    {{/if}}
  </template>
}
