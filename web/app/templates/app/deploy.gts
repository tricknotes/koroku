import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn } from '@ember/helper';
import { registerDestructor } from '@ember/destroyable';
import type Owner from '@ember/owner';
import Badge from 'paas-console/components/badge';
import AsyncView from 'paas-console/components/async-view';
import ConfirmDialog from 'paas-console/components/confirm-dialog';
import ErrorMessage from 'paas-console/components/error-message';
import LogView from 'paas-console/components/log-view';
import RelativeTime from 'paas-console/components/relative-time';
import type ApiService from 'paas-console/services/api';
import type EventSourceService from 'paas-console/services/event-source';
import type { Stream } from 'paas-console/services/event-source';
import type PollerService from 'paas-console/services/poller';
import type AppContext from 'paas-console/utils/app-context';
import { inputValue, preventDefault, setValue } from 'paas-console/utils/dom';
import {
  firstLine,
  formatDuration,
  isFullSha,
  shortSha,
} from 'paas-console/utils/format';
import LineBuffer from 'paas-console/utils/line-buffer';
import Loadable from 'paas-console/utils/loadable';
import {
  DEPLOYMENT_STATUS_LABELS,
  DEPLOYMENT_STATUS_TONES,
  isActiveDeployment,
} from 'paas-console/utils/status';
import type {
  App,
  Branch,
  Commit,
  Deployment,
  DeploymentStatus,
} from 'paas-console/utils/types';

const POLL_INTERVAL = 3000;

interface DeployTarget {
  ref?: string;
  sha?: string;
  /** What is shown in the confirmation dialog. */
  label: string;
}

type StreamState = 'idle' | 'streaming' | 'ended' | 'failed';

function statusLabel(status: DeploymentStatus): string {
  return DEPLOYMENT_STATUS_LABELS[status] ?? status;
}

function statusTone(status: DeploymentStatus) {
  return DEPLOYMENT_STATUS_TONES[status] ?? 'muted';
}

function commitLabel(commit: Commit): string {
  return `${shortSha(commit.sha)} ${firstLine(commit.message)}（${commit.author}）`;
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppDeployTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service('event-source') declare eventSource: EventSourceService;
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
  log = new LineBuffer(20000);

  @tracked branch: string;
  /** Empty means "the head of the branch". */
  @tracked sha = '';
  @tracked manualRef: string;

  @tracked pending: DeployTarget | null = null;
  @tracked deploying = false;
  @tracked canceling: number | null = null;
  @tracked error: unknown = null;

  @tracked selectedId: number | null = null;
  @tracked streamState: StreamState = 'idle';

  private stream: Stream | null = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.branch = this.app.default_branch;
    this.manualRef = this.app.default_branch;

    void this.loadHistory();
    if (this.app.repo_full_name) {
      void this.branches.load();
      void this.commits.load();
    }

    this.poller.every(this, POLL_INTERVAL, () => this.refreshWhileActive());
    registerDestructor(this, () => {
      this.closeStream();
      this.log.stop();
    });
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

  get deployments(): Deployment[] {
    return this.history.value ?? [];
  }

  get selected(): Deployment | null {
    return this.deployments.find((d) => d.id === this.selectedId) ?? null;
  }

  get hasActive(): boolean {
    return this.deployments.some((d) => isActiveDeployment(d));
  }

  get streamStatus(): string {
    switch (this.streamState) {
      case 'streaming':
        return 'ログを受信中…';
      case 'ended':
        return 'ログの終わりです。';
      case 'failed':
        return 'ログに接続できませんでした。';
      default:
        return '';
    }
  }

  get logTextUrl(): string {
    return this.selectedId === null
      ? ''
      : `${this.api.deploymentLogsUrl(this.selectedId)}?format=text`;
  }

  private async loadHistory(): Promise<void> {
    await this.history.load();
    // Follow a deployment that is already in progress.
    const active = this.deployments.find((d) => isActiveDeployment(d));
    if (active && this.selectedId === null) this.select(active.id);
  }

  /** Refreshes the history while something is queued or running. */
  async refreshWhileActive(): Promise<void> {
    if (!this.hasActive) return;
    await this.refresh();
  }

  private async refresh(): Promise<void> {
    const wasActive = this.hasActive;
    await this.history.load({ quiet: true });
    if (wasActive && !this.hasActive) await this.args.model.reload();
  }

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
      this.select(deployment.id);
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

  /** Shows the log of one deployment and follows it until it ends. */
  select = (id: number) => {
    this.closeStream();
    this.log.clear();
    this.selectedId = id;
    this.streamState = 'streaming';
    this.stream = this.eventSource.open(this.api.deploymentLogsUrl(id), {
      message: (line) => this.log.push(line),
      end: () => {
        // The server is done; without close() EventSource would reconnect.
        this.closeStream();
        this.log.flush();
        this.streamState = 'ended';
        void this.refresh();
      },
      error: (closed) => {
        if (!closed) return;
        this.closeStream();
        this.streamState = 'failed';
      },
    });
  };

  private closeStream(): void {
    this.stream?.close();
    this.stream = null;
  }

  <template>
    <section class="card" aria-labelledby="deploy-title">
      <h3 id="deploy-title">デプロイする</h3>

      <form
        class="form"
        data-test-deploy-form
        {{on "submit" (preventDefault this.submit)}}
      >
        {{#if this.manual}}
          <label class="field" for="deploy-ref">
            <span class="field-label">ブランチ / タグ / コミット SHA</span>
            <input
              id="deploy-ref"
              type="text"
              spellcheck="false"
              value={{this.manualRef}}
              data-test-deploy-ref
              {{on "input" (setValue this "manualRef")}}
            />
            <span
              class="field-hint"
            >ブランチとコミットの一覧を取得できないため、直接入力します。40 桁の
              SHA はそのコミットをデプロイします。</span>
          </label>
        {{else}}
          <div class="field-row">
            <label class="field" for="deploy-branch">
              <span class="field-label">ブランチ</span>
              <select
                id="deploy-branch"
                data-test-deploy-branch
                {{on "change" this.selectBranch}}
              >
                {{#each this.branchNames as |name|}}
                  <option value={{name}} selected={{eq name this.branch}}>
                    {{name}}
                  </option>
                {{/each}}
              </select>
            </label>

            <label class="field field-grow" for="deploy-commit">
              <span class="field-label">コミット</span>
              <select
                id="deploy-commit"
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
                  >{{commitLabel commit}}</option>
                {{/each}}
              </select>
              {{#if this.commits.error}}
                <span
                  class="field-hint"
                >コミット一覧を取得できませんでした。{{this.commits.errorMessage}}</span>
              {{/if}}
            </label>
          </div>
        {{/if}}

        <div class="form-actions">
          <button
            type="submit"
            class="button button-primary"
            disabled={{this.deploying}}
            data-test-deploy-submit
          >{{if this.deploying "開始中…" "デプロイ"}}</button>
          {{#if @model.isProduction}}
            <span class="muted">本番アプリのため、開始前に確認します。</span>
          {{/if}}
        </div>
      </form>

      <div aria-live="polite">
        <ErrorMessage @error={{this.error}} @showDetails={{true}} />
      </div>
    </section>

    {{#if this.selectedId}}
      <section class="card" aria-labelledby="deploy-log-title" data-test-log>
        <div class="card-header">
          <h3 id="deploy-log-title">デプロイ #{{this.selectedId}} のログ</h3>
          {{#if this.selected}}
            <Badge
              @tone={{statusTone this.selected.status}}
              data-test-log-status
            >{{statusLabel this.selected.status}}</Badge>
          {{/if}}
          <span class="muted" aria-live="polite" data-test-stream-state>
            {{this.streamStatus}}
          </span>
          <a
            class="card-header-end"
            href={{this.logTextUrl}}
            target="_blank"
            rel="noopener noreferrer"
          >全文を開く</a>
        </div>
        {{#if this.selected.error}}
          <p class="alert alert-danger" data-test-deployment-error>
            {{this.selected.error}}
          </p>
        {{/if}}
        <LogView
          @text={{this.log.text}}
          @label="デプロイログ"
          data-test-log-text
        />
      </section>
    {{/if}}

    <section class="card" aria-labelledby="history-title">
      <h3 id="history-title">履歴</h3>
      <AsyncView @data={{this.history}} as |deployments|>
        {{#if deployments.length}}
          <ul class="deploy-list">
            {{#each deployments key="id" as |deployment|}}
              <li
                class="deploy-row
                  {{if (eq deployment.id this.selectedId) 'is-selected'}}"
                data-test-deployment={{deployment.id}}
              >
                <div class="deploy-row-main">
                  <Badge
                    @tone={{statusTone deployment.status}}
                    data-test-deployment-status
                  >{{statusLabel deployment.status}}</Badge>
                  <span class="muted">#{{deployment.id}}</span>
                  <code title={{deployment.commit_sha}}>
                    {{shortSha deployment.commit_sha}}
                  </code>
                  <span class="deployment-message">
                    {{firstLine deployment.commit_message}}
                  </span>
                </div>
                <div class="deploy-row-meta muted">
                  {{#if deployment.ref}}<span>{{deployment.ref}}</span>{{/if}}
                  <RelativeTime @time={{deployment.queued_at}} />
                  {{#if deployment.finished_at}}
                    <span>所要
                      {{formatDuration
                        deployment.started_at
                        deployment.finished_at
                      }}</span>
                  {{/if}}
                </div>
                {{#if deployment.error}}
                  <p class="deploy-row-error">{{deployment.error}}</p>
                {{/if}}
                <div class="button-row">
                  <button
                    type="button"
                    class="button button-small"
                    data-test-show-log
                    {{on "click" (fn this.select deployment.id)}}
                  >ログ</button>
                  {{#if (isActiveDeployment deployment)}}
                    <button
                      type="button"
                      class="button button-small button-danger"
                      disabled={{eq this.canceling deployment.id}}
                      data-test-cancel
                      {{on "click" (fn this.cancel deployment)}}
                    >キャンセル</button>
                  {{else if deployment.commit_sha}}
                    <button
                      type="button"
                      class="button button-small"
                      disabled={{this.deploying}}
                      data-test-redeploy
                      {{on "click" (fn this.redeploy deployment)}}
                    >このコミットを再デプロイ</button>
                  {{/if}}
                </div>
              </li>
            {{/each}}
          </ul>
        {{else}}
          <p
            class="muted"
            data-test-no-deployments
          >まだデプロイしていません。</p>
        {{/if}}
      </AsyncView>
    </section>

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
          class="muted"
        >コンテナの入れ替えで数秒の断があります。起動に失敗した場合は直前の成功リリースに戻ります。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
