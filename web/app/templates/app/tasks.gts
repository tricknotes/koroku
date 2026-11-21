import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import AsyncView from 'koroku-console/components/async-view';
import Banner from 'koroku-console/components/banner';
import Button, { buttonClass } from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import EmptyState from 'koroku-console/components/empty-state';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import RelativeTime from 'koroku-console/components/relative-time';
import RoleNote from 'koroku-console/components/role-note';
import RowList from 'koroku-console/components/row-list';
import StatusIndicator from 'koroku-console/components/status-indicator';
import TabSection from 'koroku-console/components/tab-section';
import TaskRunDetail from 'koroku-console/components/task-run-detail';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type AppContext from 'koroku-console/utils/app-context';
import { inputValue, preventDefault, setValue } from 'koroku-console/utils/dom';
import { ApiError, fieldErrors } from 'koroku-console/utils/errors';
import { formatDuration } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import { can } from 'koroku-console/utils/roles';
import {
  TASK_RUN_STATUS_KINDS,
  TASK_RUN_STATUS_LABELS,
} from 'koroku-console/utils/status';
import type {
  App,
  ServiceStatus,
  Task,
  TaskRun,
} from 'koroku-console/utils/types';

const POLL_INTERVAL = 3000;

const SKELETON_ROWS = [1, 2];

/** What a run is about to do, as the confirmation dialog shows it. */
interface RunTarget {
  /** The registered task, or '' for the command typed in the form. */
  task: string;
  service: string;
  command: string;
}

let draftIds = 0;

/** One row of the task editor. */
class DraftTask {
  readonly id = ++draftIds;
  @tracked name: string;
  @tracked service: string;
  @tracked command: string;

  constructor(task: Task) {
    this.name = task.name;
    this.service = task.service;
    this.command = task.command;
  }
}

function statusKind(run: TaskRun) {
  return TASK_RUN_STATUS_KINDS[run.status] ?? 'stopped';
}

function statusLabel(run: TaskRun): string {
  return TASK_RUN_STATUS_LABELS[run.status] ?? run.status;
}

function isActive(run: TaskRun): boolean {
  return run.status === 'running';
}

function isUneventful(run: TaskRun): boolean {
  return run.status === 'succeeded';
}

function duration(run: TaskRun): string {
  return formatDuration(run.started_at, run.finished_at);
}

function detailId(run: TaskRun): string {
  return `task-run-${run.id}-detail`;
}

function exitLabel(run: TaskRun): string {
  // A zero exit code is the normal case and stays out of the way.
  return run.exit_code === null || run.exit_code === 0
    ? ''
    : `終了コード ${run.exit_code}`;
}

interface Signature {
  Args: { model: AppContext };
}

/**
 * Tasks tab (ARCHITECTURE 9.6): commands run once in a service of the
 * running release, the named ones kept for one-click runs, and the history
 * of runs with their output.
 */
export default class AppTasksTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  tasks = new Loadable<Task[]>(() => this.api.listTasks(this.app.slug));
  runs = new Loadable<TaskRun[]>(() => this.api.listTaskRuns(this.app.slug));
  services = new Loadable<ServiceStatus[]>(() =>
    this.api.listServices(this.app.slug),
  );

  // The command typed on the spot.
  @tracked service: string;
  @tracked command = '';

  // Editing the registered tasks.
  @tracked editing = false;
  @tracked drafts: DraftTask[] = [];
  @tracked saving = false;
  @tracked saveError: unknown = null;

  @tracked pending: RunTarget | null = null;
  @tracked starting = false;
  @tracked runError: unknown = null;
  @tracked canceling: number | null = null;

  /** The run whose output is open below its row. */
  @tracked selectedId: number | null = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.service = this.app.web_service;
    void this.tasks.load();
    void this.services.load();
    void this.loadRuns();
    this.poller.every(this, POLL_INTERVAL, () => this.poll());
  }

  /** Running, editing and canceling need manage (ARCHITECTURE 10.7). */
  get canManage(): boolean {
    return can(this.args.model.app, 'manage');
  }

  get canEditTasks(): boolean {
    return this.canManage && Boolean(this.tasks.value?.length);
  }

  canCancel = (run: TaskRun): boolean => this.canManage && isActive(run);

  get app(): App {
    return this.args.model.app;
  }

  /** Commands need the files of a release to run in. */
  get deployed(): boolean {
    return this.app.current_deployment !== null;
  }

  get isStopped(): boolean {
    return this.deployed && this.app.desired_state === 'stopped';
  }

  get runDisabled(): boolean {
    return !this.deployed || this.starting || this.hasActive;
  }

  /** Service names to pick from: the app's containers, the public one first. */
  get serviceNames(): string[] {
    const names = new Set<string>([this.app.web_service]);
    for (const svc of this.services.value ?? []) names.add(svc.name);
    if (this.service) names.add(this.service);
    return [...names];
  }

  get registered(): Task[] {
    return this.tasks.value ?? [];
  }

  get history(): TaskRun[] {
    return this.runs.value ?? [];
  }

  get hasActive(): boolean {
    return this.history.some(isActive);
  }

  get draftErrors(): Record<string, string> {
    return fieldErrors(this.saveError);
  }

  /** Validation problems of the editor apply to its fields, not the banner. */
  get saveBannerError(): unknown {
    const error = this.saveError;
    return error instanceof ApiError && error.code === 'validation_failed'
      ? null
      : error;
  }

  /** The list-wide problem of a refused save ("at most 20 tasks"). */
  get listError(): string | undefined {
    return this.draftErrors['tasks'];
  }

  draftError = (index: number, field: string): string | undefined =>
    this.draftErrors[`tasks[${index}].${field}`];

  isSelected = (run: TaskRun): boolean => this.selectedId === run.id;

  private async loadRuns(): Promise<void> {
    await this.runs.load();
    // Open what most likely brought the reader here.
    const head = this.history[0];
    if (
      this.selectedId === null &&
      head &&
      (isActive(head) || head.status === 'failed')
    ) {
      this.selectedId = head.id;
    }
  }

  async poll(): Promise<void> {
    if (this.hasActive) await this.refresh();
  }

  refresh = async (): Promise<void> => {
    await this.runs.load({ quiet: true });
  };

  // Running

  selectService = (event: Event) => {
    this.service = inputValue(event);
  };

  submitCommand = () => {
    const command = this.command.trim();
    if (!command) return;
    this.requestRun({ task: '', service: this.service, command });
  };

  runTask = (task: Task) => {
    this.requestRun({
      task: task.name,
      service: task.service,
      command: task.command,
    });
  };

  /** Production apps go through the confirmation dialog first. */
  private requestRun(target: RunTarget): void {
    this.runError = null;
    if (this.args.model.isProduction) {
      this.pending = target;
    } else {
      void this.start(target);
    }
  }

  confirmRun = async () => {
    if (this.pending) await this.start(this.pending);
    this.pending = null;
  };

  cancelConfirm = () => {
    this.pending = null;
  };

  private async start(target: RunTarget): Promise<void> {
    this.starting = true;
    this.runError = null;
    try {
      const run = await this.api.createTaskRun(
        this.app.slug,
        target.task
          ? { task: target.task }
          : { service: target.service, command: target.command },
      );
      if (!target.task) this.command = '';
      await this.runs.load({ quiet: true });
      this.selectedId = run.id;
    } catch (error) {
      this.runError = error;
    } finally {
      this.starting = false;
    }
  }

  cancel = async (run: TaskRun) => {
    this.canceling = run.id;
    this.runError = null;
    try {
      await this.api.cancelTaskRun(run.id);
    } catch (error) {
      // It ended on its own meanwhile: the refresh below shows how.
      if (!(error instanceof ApiError && error.code === 'not_cancelable')) {
        this.runError = error;
      }
    } finally {
      this.canceling = null;
    }
    await this.refresh();
  };

  toggle = (run: TaskRun) => {
    this.selectedId = this.isSelected(run) ? null : run.id;
  };

  // Editing the registered tasks

  startEditing = () => {
    this.drafts = this.registered.map((task) => new DraftTask(task));
    if (this.drafts.length === 0) this.addDraft();
    this.saveError = null;
    this.editing = true;
  };

  addDraft = () => {
    this.drafts = [
      ...this.drafts,
      new DraftTask({ name: '', service: this.app.web_service, command: '' }),
    ];
  };

  removeDraft = (draft: DraftTask) => {
    this.drafts = this.drafts.filter((entry) => entry !== draft);
  };

  setDraft = (draft: DraftTask, field: 'name' | 'service' | 'command') =>
    setValue(draft, field);

  stopEditing = () => {
    this.editing = false;
    this.saveError = null;
  };

  save = async () => {
    this.saving = true;
    this.saveError = null;
    try {
      const saved = await this.api.putTasks(
        this.app.slug,
        this.drafts.map((draft) => ({
          name: draft.name.trim(),
          service: draft.service.trim(),
          command: draft.command.trim(),
        })),
      );
      this.tasks.set(saved);
      this.editing = false;
    } catch (error) {
      this.saveError = error;
    } finally {
      this.saving = false;
    }
  };

  <template>
    <div class="pane-notices" aria-live="polite">
      {{#unless this.deployed}}
        <Banner @title="まだデプロイしていません" data-test-tasks-not-deployed>
          <:default
          >コマンドは稼働中のリリースのイメージで実行します。先にデプロイしてください。</:default>
          <:actions>
            <LinkTo
              @route="app.deploy"
              @model={{this.app.slug}}
              class={{buttonClass "default" "small"}}
            >デプロイへ</LinkTo>
          </:actions>
        </Banner>
      {{/unless}}
      {{#if this.isStopped}}
        <Banner @tone="warning" data-test-tasks-stopped>
          アプリは停止中です。コマンドは実行できますが、DB
          など依存するサービスも止まっているため、つながらずに失敗することがあります。
        </Banner>
      {{/if}}
      <ErrorMessage @error={{this.runError}} @showDetails={{true}} />
    </div>

    <TabSection @title="タスク" data-test-tasks-section>
      <:actions>
        {{#if this.canEditTasks}}
          {{#unless this.editing}}
            <Button
              @size="small"
              data-test-edit-tasks
              {{on "click" this.startEditing}}
            >編集</Button>
          {{/unless}}
        {{/if}}
      </:actions>
      <:default>
        {{#if this.editing}}
          <form
            class="box task-editor"
            data-test-task-editor
            {{on "submit" (preventDefault this.save)}}
          >
            {{#if this.listError}}
              <p
                class="field-error"
                data-test-tasks-error
              >{{this.listError}}</p>
            {{/if}}
            <ol class="task-editor-rows">
              {{#each this.drafts key="id" as |draft index|}}
                <li class="task-editor-row" data-test-task-draft>
                  <FormField
                    class="task-field-name"
                    @label="名前"
                    @error={{this.draftError index "name"}}
                    as |field|
                  >
                    <input
                      id={{field.id}}
                      type="text"
                      autocomplete="off"
                      placeholder="マイグレーションの状態"
                      value={{draft.name}}
                      aria-describedby={{field.describedBy}}
                      aria-invalid={{field.invalid}}
                      data-test-draft-name
                      {{on "input" (this.setDraft draft "name")}}
                    />
                  </FormField>
                  <FormField
                    class="task-field-service"
                    @label="サービス"
                    @error={{this.draftError index "service"}}
                    as |field|
                  >
                    <input
                      id={{field.id}}
                      type="text"
                      class="mono"
                      spellcheck="false"
                      autocomplete="off"
                      value={{draft.service}}
                      aria-describedby={{field.describedBy}}
                      aria-invalid={{field.invalid}}
                      data-test-draft-service
                      {{on "input" (this.setDraft draft "service")}}
                    />
                  </FormField>
                  <FormField
                    class="task-field-command"
                    @label="コマンド"
                    @error={{this.draftError index "command"}}
                    as |field|
                  >
                    <input
                      id={{field.id}}
                      type="text"
                      class="mono"
                      spellcheck="false"
                      autocomplete="off"
                      placeholder="bin/rails db:migrate:status"
                      value={{draft.command}}
                      aria-describedby={{field.describedBy}}
                      aria-invalid={{field.invalid}}
                      data-test-draft-command
                      {{on "input" (this.setDraft draft "command")}}
                    />
                  </FormField>
                  <Button
                    @variant="ghost"
                    @size="small"
                    class="task-remove"
                    data-test-remove-draft
                    {{on "click" (fn this.removeDraft draft)}}
                  >外す</Button>
                </li>
              {{/each}}
            </ol>
            <div class="task-editor-footer">
              <Button
                @variant="ghost"
                @size="small"
                data-test-add-draft
                {{on "click" this.addDraft}}
              >タスクを追加</Button>
              <div class="task-editor-actions">
                <Button
                  @disabled={{this.saving}}
                  data-test-cancel-edit
                  {{on "click" this.stopEditing}}
                >キャンセル</Button>
                <Button
                  @type="submit"
                  @variant="primary"
                  @busy={{this.saving}}
                  @busyLabel="保存中…"
                  data-test-save-tasks
                >保存</Button>
              </div>
            </div>
            <div aria-live="polite">
              <ErrorMessage @error={{this.saveBannerError}} />
            </div>
          </form>
        {{else}}
          <AsyncView
            @data={{this.tasks}}
            @errorTitle="タスクの一覧を取得できませんでした"
          >
            <:loading>
              <div
                class="row-list task-rows"
                role="status"
                aria-label="読み込み中"
                data-test-loading
              >
                {{#each SKELETON_ROWS as |n|}}
                  <div class="row" aria-hidden="true" data-skeleton={{n}}>
                    <div><span class="skeleton skeleton-fixed"></span></div>
                    <div><span class="skeleton skeleton-wide"></span></div>
                  </div>
                {{/each}}
              </div>
            </:loading>
            <:default as |tasks|>
              {{#if tasks.length}}
                <RowList class="task-rows" @label="登録したタスク">
                  <:header>
                    <span>名前</span>
                    <span>サービス</span>
                    <span>コマンド</span>
                    <span></span>
                  </:header>
                  <:rows as |Row|>
                    {{#each tasks key="name" as |task|}}
                      <Row data-test-task={{task.name}}>
                        <div class="task-name">{{task.name}}</div>
                        <div class="task-service">
                          <span class="cell-label">サービス</span>
                          <span class="mono">{{task.service}}</span>
                        </div>
                        <div class="task-command">
                          <span class="visually-hidden">コマンド</span>
                          <code
                            class="mono"
                            title={{task.command}}
                          >{{task.command}}</code>
                        </div>
                        <div class="task-action">
                          {{#if this.canManage}}
                            <Button
                              @size="small"
                              @disabled={{this.runDisabled}}
                              class="row-action"
                              aria-label="「{{task.name}}」を実行"
                              data-test-run-task
                              {{on "click" (fn this.runTask task)}}
                            >実行</Button>
                          {{/if}}
                        </div>
                      </Row>
                    {{/each}}
                  </:rows>
                </RowList>
              {{else}}
                <div class="box" data-test-no-tasks>
                  <EmptyState
                    @title="登録したタスクはありません"
                    @description="よく使うコマンド（マイグレーションの状態の確認など）に名前を付けておくと、ここからボタン 1 つで実行できます。"
                  >
                    <:action>
                      {{#if this.canManage}}
                        <Button
                          data-test-add-first-task
                          {{on "click" this.startEditing}}
                        >タスクを追加</Button>
                      {{/if}}
                    </:action>
                  </EmptyState>
                </div>
              {{/if}}
            </:default>
          </AsyncView>
        {{/if}}
      </:default>
    </TabSection>

    {{#if this.canManage}}
      <TabSection @title="コマンドを実行" data-test-command-section>
        <p class="pane-description">
          稼働中のリリースと同じイメージ・環境変数・メモリ上限の新しいコンテナで
          1
          回だけ実行します。端末と入力はありません。出力の環境変数の値は伏せて記録しますが、コマンドの文字列はそのまま残るので、秘密はコマンドに書かず環境変数で渡してください。
        </p>
        <form
          class="task-form"
          data-test-command-form
          {{on "submit" (preventDefault this.submitCommand)}}
        >
          <FormField class="task-form-service" @label="サービス" as |field|>
            <select
              id={{field.id}}
              data-test-command-service
              {{on "change" this.selectService}}
            >
              {{#each this.serviceNames as |name|}}
                <option value={{name}} selected={{eq name this.service}}>
                  {{name}}
                </option>
              {{/each}}
            </select>
          </FormField>
          <FormField class="task-form-command" @label="コマンド" as |field|>
            <input
              id={{field.id}}
              type="text"
              class="mono"
              spellcheck="false"
              autocomplete="off"
              placeholder="bin/rails runner 'puts User.count'"
              value={{this.command}}
              data-test-command-input
              {{on "input" (setValue this "command")}}
            />
          </FormField>
          <Button
            @type="submit"
            @variant="primary"
            @busy={{this.starting}}
            @busyLabel="開始中…"
            @disabled={{this.runDisabled}}
            class="task-form-submit"
            data-test-command-submit
          >実行</Button>
          {{#if this.hasActive}}
            <p class="task-form-note" data-test-run-active>
              実行中のコマンドが終わるまで、次のコマンドは実行できません。
            </p>
          {{else if @model.isProduction}}
            <p class="task-form-note" data-test-production-hint>
              本番アプリのため、実行前に確認します。
            </p>
          {{/if}}
        </form>
      </TabSection>
    {{else}}
      <RoleNote @needed="manage" @what="コマンドとタスクの実行" />
    {{/if}}

    <TabSection @title="実行履歴" data-test-runs-section>
      {{#if this.runs.stale}}
        <Banner @tone="warning" data-test-stale>
          最新の状態を取得できていません。表示は最後に取得できた内容です。
        </Banner>
      {{/if}}
      <AsyncView
        @data={{this.runs}}
        @errorTitle="実行履歴を取得できませんでした"
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
              </div>
            {{/each}}
          </div>
        </:loading>
        <:default as |runs|>
          {{#if runs.length}}
            <RowList class="deploy-rows task-run-rows" @label="実行履歴">
              <:header>
                <span>状態</span>
                <span>タスク / コマンド</span>
                <span>所要時間</span>
                <span>開始</span>
              </:header>
              <:rows>
                {{#each runs key="id" as |run|}}
                  <li
                    class="deploy-item {{if (this.isSelected run) 'is-open'}}"
                    data-test-task-run={{run.id}}
                  >
                    <div class="row deploy-row">
                      <div class="deploy-cell-state">
                        <StatusIndicator
                          @kind={{statusKind run}}
                          @label={{statusLabel run}}
                          @quiet={{isUneventful run}}
                          data-test-run-status
                        />
                      </div>

                      <div class="deploy-cell-commit">
                        <button
                          type="button"
                          class="row-link deploy-toggle
                            {{unless run.task_name 'mono task-run-adhoc'}}"
                          aria-expanded={{if
                            (this.isSelected run)
                            "true"
                            "false"
                          }}
                          aria-controls={{detailId run}}
                          data-test-show-output
                          {{on "click" (fn this.toggle run)}}
                        >{{if run.task_name run.task_name run.command}}</button>
                        <span class="deploy-meta">
                          <span class="mono">{{run.service}}</span>
                          {{#if (exitLabel run)}}
                            <span data-test-exit-code>{{exitLabel run}}</span>
                          {{/if}}
                          {{#if run.actor}}
                            <span
                              class="deploy-actor"
                              title="実行した人"
                              data-test-run-actor
                            >実行: {{run.actor.login}}</span>
                          {{/if}}
                        </span>
                        {{#if run.error}}
                          {{#if (eq run.exit_code null)}}
                            <span class="deploy-row-error" data-test-row-error>
                              {{run.error}}
                            </span>
                          {{/if}}
                        {{/if}}
                      </div>

                      <div class="deploy-cell-duration">
                        {{#if (duration run)}}
                          <span class="deploy-cell-label">所要</span>
                          <span data-test-duration>{{duration run}}</span>
                        {{/if}}
                      </div>

                      <div class="deploy-cell-started">
                        <span class="visually-hidden">開始</span>
                        <RelativeTime @time={{run.started_at}} />
                      </div>

                      <div class="deploy-cell-actions">
                        {{#if (this.canCancel run)}}
                          <Button
                            @size="small"
                            @disabled={{eq this.canceling run.id}}
                            class="row-action"
                            data-test-cancel-run
                            {{on "click" (fn this.cancel run)}}
                          >キャンセル</Button>
                        {{/if}}
                        <Icon @name="chevron-down" class="deploy-chevron" />
                      </div>
                    </div>

                    {{#if (this.isSelected run)}}
                      <TaskRunDetail
                        id={{detailId run}}
                        @run={{run}}
                        @onLogEnd={{this.refresh}}
                      />
                    {{/if}}
                  </li>
                {{/each}}
              </:rows>
            </RowList>
          {{else}}
            <p class="muted" data-test-no-runs>
              まだ実行していません。実行すると、ここに出力と結果が残ります（新しい
              50 件）。
            </p>
          {{/if}}
        </:default>
      </AsyncView>
    </TabSection>

    {{#if this.pending}}
      <ConfirmDialog
        @title="本番アプリでコマンドを実行しますか？"
        @confirmLabel="実行する"
        @busy={{this.starting}}
        @onConfirm={{this.confirmRun}}
        @onCancel={{this.cancelConfirm}}
      >
        <p>本番アプリ
          <strong>{{this.app.slug}}</strong>
          の
          <strong class="mono">{{this.pending.service}}</strong>
          で次のコマンドを実行します。</p>
        {{#if this.pending.task}}
          <p>タスク:
            <strong data-test-dialog-task>{{this.pending.task}}</strong></p>
        {{/if}}
        <pre
          class="code-block task-dialog-command"
          data-test-dialog-command
        >{{this.pending.command}}</pre>
        <p>本番のデータを書き換えるコマンドは取り消せません。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
