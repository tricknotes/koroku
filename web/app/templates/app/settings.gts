import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn } from '@ember/helper';
import type Owner from '@ember/owner';
import { LinkTo } from '@ember/routing';
import type RouterService from '@ember/routing/router-service';
import type Transition from '@ember/routing/transition';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import CopyButton from 'koroku-console/components/copy-button';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import RoleNote from 'koroku-console/components/role-note';
import RegistryCredential from 'koroku-console/components/registry-credential';
import StatusIndicator from 'koroku-console/components/status-indicator';
import type ApiService from 'koroku-console/services/api';
import type AppContext from 'koroku-console/utils/app-context';
import {
  inputValue,
  preventDefault,
  setChecked,
  setValue,
} from 'koroku-console/utils/dom';
import { missingVariables } from 'koroku-console/utils/env-requirements';
import { fieldErrors, policyViolations } from 'koroku-console/utils/errors';
import Loadable from 'koroku-console/utils/loadable';
import { formatBytes } from 'koroku-console/utils/format';
import { reservationPlan } from 'koroku-console/utils/service-reservation';
import { can } from 'koroku-console/utils/roles';
import SettingsForm, {
  ExemptRow,
  IDLE_MINUTES_CHOICES,
  LimitRow,
  MAX_EXEMPT_PATHS,
  exemptPathProblem,
} from 'koroku-console/utils/settings-form';
import {
  APP_STATE_HINTS,
  APP_STATE_KINDS,
  APP_STATE_LABELS,
  deriveAppState,
} from 'koroku-console/utils/status';
import type {
  App,
  AppPatch,
  EnvRequirements,
  EnvSource,
  Environment,
  ServiceStatus,
  SystemInfo,
} from 'koroku-console/utils/types';

/** A service whose reservation the saved settings change (ARCHITECTURE 6.2). */
interface ReservationChange {
  name: string;
  running: string;
  next: string;
}

function bytesOrNone(bytes: number): string {
  return bytes > 0 ? formatBytes(bytes) : 'なし';
}

/** Settings that change what the requirements check reads. */
const REQUIREMENT_FIELDS: (keyof AppPatch)[] = [
  'default_branch',
  'compose_path',
  'env_source',
  'env_example_path',
];

const DEPLOY_ROUTE = 'app.deploy';

const KNOWN_FIELDS = new Set([
  'default_branch',
  'compose_path',
  'web_service',
  'web_port',
  'healthcheck_path',
  'environment',
  'memory_limit',
  'service_memory_limits',
  'memory_reservation',
  'service_memory_reservations',
  'env_source',
  'env_example_path',
  'access_protection',
  'protection_exempt_paths',
  'promote_from',
  'auto_deploy',
  'auto_deploy_branch',
  'auto_deploy_workflow',
  'idle_sleep',
  'idle_sleep_minutes',
]);

/** "30 分", "3 時間": an idle time as the settings show it. */
function minutesLabel(minutes: number): string {
  return minutes % 60 === 0 ? `${minutes / 60} 時間` : `${minutes} 分`;
}

type ControlAction = 'start' | 'stop' | 'restart';

const CONTROL_DONE: Record<ControlAction, string> = {
  start: '起動しました。',
  stop: '停止しました。',
  restart: '再起動しました。',
};

function isChecked(current: Environment, value: Environment): boolean {
  return current === value;
}

function isSource(current: EnvSource, value: EnvSource): boolean {
  return current === value;
}

const VAR_PLAIN = '${VAR}';
const VAR_REQUIRED = '${VAR:?説明}';
const VAR_OPTIONAL = '${VAR:-既定値}';

function isBusy(current: ControlAction | null, action: ControlAction): boolean {
  return current === action;
}

/** Where the repository can be looked at, when that is a web address. */
function repositoryUrl(app: App): string | null {
  if (app.repo_full_name) return `https://github.com/${app.repo_full_name}`;
  return /^https?:\/\//.test(app.repo_url) ? app.repo_url : null;
}

/** Pushes reach koroku through the GitHub App (ARCHITECTURE 5.2). */
function usesGitHubApp(app: App): boolean {
  return app.repo_full_name !== '' && !app.repo_url;
}

function repositoryName(app: App): string {
  return app.repo_full_name || app.repo_url;
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

  /** A save that changes the protection of a production app, held for confirmation. */
  @tracked pendingProtection: AppPatch | null = null;
  /** A save that makes a production app deploy pushes, held for confirmation. */
  @tracked pendingAutoDeploy: AppPatch | null = null;
  /** A save that lets a production app sleep while idle, held for confirmation. */
  @tracked pendingIdleSleep: AppPatch | null = null;

  @tracked controlling: ControlAction | null = null;
  @tracked controlNotice = '';
  @tracked controlError: unknown = null;
  @tracked confirmingStop = false;

  @tracked confirmingDelete = false;
  @tracked deleteVolumes = false;
  @tracked deleting = false;
  @tracked deleteError: unknown = null;

  /** The webhook URL and whether its secret is set (ARCHITECTURE 5.2). */
  system = new Loadable<SystemInfo>(() => this.api.getSystem());

  /** The other apps, for the promotion source (ARCHITECTURE 9.7). */
  apps = new Loadable<App[]>(() => this.api.listApps());

  /**
   * The services of the running release, for what it reserves against
   * what the saved settings will (ARCHITECTURE 6.2).
   */
  services = new Loadable<ServiceStatus[]>(() =>
    this.api.listServices(this.app.slug),
  );

  /**
   * What the requirements check finds with the saved settings (ARCHITECTURE
   * 9.4). Read again whenever a save changes where it reads from.
   */
  requirements = new Loadable<EnvRequirements>(() =>
    this.api.getEnvRequirements(this.app.slug),
  );

  /** A move to the deploy tab held back because of unsaved changes. */
  @tracked pendingLeave: Transition | null = null;
  /** The held move goes ahead once the save it waits for succeeds. */
  private leaveAfterSave: Transition | null = null;
  private leaveApproved = false;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.form = new SettingsForm(this.app);
    void this.requirements.load();
    void this.system.load();
    this.router.on('routeWillChange', this.guardLeave);
    if (this.app.environment === 'production' || this.app.promote_from) {
      void this.apps.load();
    }
    if (this.app.current_deployment) void this.services.load();
  }

  /** Services whose next deployment reserves something else than now. */
  get reservationChanges(): ReservationChange[] {
    const services = this.services.value ?? [];
    return services.flatMap((svc) => {
      const plan = reservationPlan(this.app, svc);
      if (!plan.pending || plan.nextBytes === null) return [];
      return [
        {
          name: svc.name,
          running: bytesOrNone(plan.runningBytes),
          next: bytesOrNone(plan.nextBytes),
        },
      ];
    });
  }

  willDestroy(): void {
    super.willDestroy();
    this.router.off('routeWillChange', this.guardLeave);
  }

  get hasUnsavedChanges(): boolean {
    return Object.keys(this.form.patch(this.app)).length > 0;
  }

  /**
   * Going to the deploy tab (its tab or the header's button) with unsaved
   * changes would deploy the old settings and drop the edits: ask first.
   */
  guardLeave = (transition: Transition) => {
    if (this.leaveApproved || transition.isAborted) return;
    if (transition.to?.name !== DEPLOY_ROUTE) return;
    if (!this.hasUnsavedChanges) return;
    transition.abort();
    this.pendingLeave = transition;
  };

  saveAndLeave = async () => {
    const transition = this.pendingLeave;
    if (!transition) return;
    this.leaveAfterSave = transition;
    this.pendingLeave = null;
    await this.save();
    // A production protection or auto-deploy change asks once more; the
    // move then waits for that confirmation (see confirmProtection).
    if (
      !this.pendingProtection &&
      !this.pendingAutoDeploy &&
      !this.pendingIdleSleep
    )
      this.continueLeave();
  };

  leaveWithoutSaving = () => {
    const transition = this.pendingLeave;
    this.pendingLeave = null;
    if (!transition) return;
    this.leaveApproved = true;
    transition.retry();
  };

  cancelLeave = () => {
    this.pendingLeave = null;
  };

  private continueLeave(): void {
    const transition = this.leaveAfterSave;
    this.leaveAfterSave = null;
    if (!transition || !this.saved) return;
    this.leaveApproved = true;
    transition.retry();
  }

  get requirementsSummary(): {
    tone: 'default' | 'warning';
    text: string;
  } | null {
    const req = this.requirements.value;
    if (this.requirements.loading || !req) return null;
    if (req.status === 'off') return null;
    if (req.status !== 'ok') {
      return { tone: 'warning', text: '必要な環境変数を読めませんでした。' };
    }
    const required = req.variables.filter((v) => v.required).length;
    const missing = missingVariables(req).required.length;
    const where = req.ref ? `ブランチ ${req.ref} の` : '';
    if (missing > 0) {
      return {
        tone: 'warning',
        text: `${where}リポジトリが必須とする環境変数 ${required} 件のうち、${missing} 件が未設定です。`,
      };
    }
    return {
      tone: 'default',
      text: `${where}リポジトリが必須とする環境変数 ${required} 件は、すべて設定済みです。`,
    };
  }

  get isProductionForm(): boolean {
    return this.form.environment === 'production';
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

  /** Start, stop and restart need deploy. */
  get canDeploy(): boolean {
    return can(this.app, 'deploy');
  }

  get state() {
    const state = deriveAppState(this.app);
    return {
      kind: APP_STATE_KINDS[state],
      label: APP_STATE_LABELS[state],
      hint: APP_STATE_HINTS[state],
    };
  }

  /** Start, stop and restart need a release to act on. */
  get neverDeployed(): boolean {
    return this.app.current_deployment === null;
  }

  get stopped(): boolean {
    return this.app.desired_state === 'stopped';
  }

  get controlsDisabled(): boolean {
    return this.neverDeployed || this.controlling !== null;
  }

  get cannotStop(): boolean {
    return this.controlsDisabled || this.stopped;
  }

  get otherErrors(): string[] {
    return Object.entries(this.errors)
      .filter(([name]) => !KNOWN_FIELDS.has(name))
      .map(([name, reason]) => `${name}: ${reason}`);
  }

  get violations(): string[] {
    return policyViolations(this.controlError);
  }

  setEnvironment = (event: Event) => {
    this.form.environment = inputValue(event) as Environment;
    if (this.form.environment === 'production') void this.apps.load();
  };

  setPromoteFrom = (event: Event) => {
    this.form.promoteFrom = inputValue(event);
    this.touch();
  };

  setEnvSource = (event: Event) => {
    this.form.envSource = inputValue(event) as EnvSource;
  };

  addLimit = () => {
    this.form.limits = [...this.form.limits, new LimitRow()];
  };

  removeLimit = (row: LimitRow) => {
    this.form.limits = this.form.limits.filter((r) => r !== row);
    this.touch();
  };

  addReservation = () => {
    this.form.reservations = [...this.form.reservations, new LimitRow()];
  };

  removeReservation = (row: LimitRow) => {
    this.form.reservations = this.form.reservations.filter((r) => r !== row);
    this.touch();
  };

  touch = () => {
    this.saved = false;
    this.nothingToSave = false;
  };

  /** Whether the app is or becomes a production app with this save. */
  get savesProduction(): boolean {
    return (
      this.app.environment === 'production' ||
      this.form.environment === 'production'
    );
  }

  /** Opening a production app to everyone is the dangerous direction. */
  get publicProduction(): boolean {
    return !this.form.accessProtection;
  }

  /** Staging apps get `noindex` from the proxy even when they are public. */
  get publicStaging(): boolean {
    return !this.form.accessProtection && this.form.environment === 'staging';
  }

  addExemptPath = () => {
    this.form.exemptPaths = [...this.form.exemptPaths, new ExemptRow()];
  };

  /** Marks the rows behind a reported problem, not every row. */
  exemptRowInvalid = (row: ExemptRow): boolean => {
    if (!this.errors['protection_exempt_paths']) return false;
    const path = row.path.trim();
    return (
      path !== '' &&
      (exemptPathProblem(path) !== null ||
        this.form.exemptList.length > MAX_EXEMPT_PATHS)
    );
  };

  removeExemptPath = (row: ExemptRow) => {
    this.form.exemptPaths = this.form.exemptPaths.filter((r) => r !== row);
    this.touch();
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
    if (patch.access_protection !== undefined && this.savesProduction) {
      // Closing a production app locks its users out; opening it publishes it.
      this.pendingProtection = patch;
      return;
    }
    if (this.startsProductionAutoDeploy(patch)) {
      this.pendingAutoDeploy = patch;
      return;
    }
    if (this.startsProductionIdleSleep(patch)) {
      this.pendingIdleSleep = patch;
      return;
    }
    await this.commit(patch);
  };

  /**
   * Whether a save lets a production app sleep while idle: its first
   * visitor after a sleep waits for it to start (ARCHITECTURE 8.4).
   */
  startsProductionIdleSleep(patch: AppPatch): boolean {
    if (!this.isProductionForm || !this.form.idleSleep) return false;
    return patch.idle_sleep === true || patch.environment === 'production';
  }

  /**
   * Whether a save makes a production app deploy pushes, or pushes of
   * another branch, or after another workflow (or none), without anyone
   * pressing a button (ARCHITECTURE 5.2).
   */
  startsProductionAutoDeploy(patch: AppPatch): boolean {
    if (!this.isProductionForm || !this.form.autoDeploy) return false;
    return (
      patch.auto_deploy === true ||
      patch.environment === 'production' ||
      patch.auto_deploy_branch !== undefined ||
      patch.auto_deploy_workflow !== undefined ||
      patch.default_branch !== undefined
    );
  }

  confirmProtection = async () => {
    const patch = this.pendingProtection;
    if (!patch) return;
    if (this.startsProductionAutoDeploy(patch)) {
      // One more question before anything is saved.
      this.pendingProtection = null;
      this.pendingAutoDeploy = patch;
      return;
    }
    if (this.startsProductionIdleSleep(patch)) {
      this.pendingProtection = null;
      this.pendingIdleSleep = patch;
      return;
    }
    await this.commit(patch);
    this.pendingProtection = null;
    this.continueLeave();
  };

  cancelProtection = () => {
    this.pendingProtection = null;
    this.leaveAfterSave = null;
  };

  confirmAutoDeploy = async () => {
    const patch = this.pendingAutoDeploy;
    if (!patch) return;
    if (this.startsProductionIdleSleep(patch)) {
      this.pendingAutoDeploy = null;
      this.pendingIdleSleep = patch;
      return;
    }
    await this.commit(patch);
    this.pendingAutoDeploy = null;
    this.continueLeave();
  };

  confirmIdleSleep = async () => {
    const patch = this.pendingIdleSleep;
    if (!patch) return;
    await this.commit(patch);
    this.pendingIdleSleep = null;
    this.continueLeave();
  };

  cancelIdleSleep = () => {
    this.pendingIdleSleep = null;
    this.leaveAfterSave = null;
  };

  /**
   * Requests cannot be counted without Caddy's per-app access logs; the
   * setting can then only be turned off.
   */
  get idleSleepUnavailable(): boolean {
    return this.system.value?.idle_sleep?.available === false;
  }

  get idleSleepDisabled(): boolean {
    return this.idleSleepUnavailable && !this.form.idleSleep;
  }

  setIdleMinutes = (event: Event) => {
    this.form.idleSleepMinutes = Number(inputValue(event));
    this.touch();
  };

  /** The saved idle time, offered even when it is not one of the choices. */
  get idleMinutesChoices(): number[] {
    const choices = [...IDLE_MINUTES_CHOICES];
    if (!choices.includes(this.form.idleSleepMinutes)) {
      choices.push(this.form.idleSleepMinutes);
      choices.sort((a, b) => a - b);
    }
    return choices;
  }

  cancelAutoDeploy = () => {
    this.pendingAutoDeploy = null;
    this.leaveAfterSave = null;
  };

  /** The form names a workflow to wait for before deploying a push. */
  get waitsForWorkflow(): boolean {
    return this.form.autoDeployWorkflow.trim() !== '';
  }

  get canAutoDeploy(): boolean {
    return usesGitHubApp(this.app);
  }

  /** Off and impossible: nothing to switch on. */
  get autoDeployDisabled(): boolean {
    // Making a production app deploy pushes by itself is the owner's call
    // (ARCHITECTURE 10.7).
    if (this.app.environment === 'production' && !this.isOwner) return true;
    return !this.canAutoDeploy && !this.form.autoDeploy;
  }

  get webhookSecretMissing(): boolean {
    return this.system.value?.webhook?.secret_configured === false;
  }

  commit = async (patch: AppPatch) => {
    this.saving = true;
    try {
      const app = await this.api.updateApp(this.app.slug, patch);
      this.args.model.set(app);
      this.form = new SettingsForm(app);
      this.saved = true;
      if (app.current_deployment) void this.services.load({ quiet: true });
      if (REQUIREMENT_FIELDS.some((field) => field in patch)) {
        void this.requirements.load();
      }
    } catch (error) {
      this.error = error;
      this.errors = fieldErrors(error);
    } finally {
      this.saving = false;
    }
  };

  control = async (action: ControlAction) => {
    if (this.controlling) return;
    this.controlling = action;
    this.controlNotice = '';
    this.controlError = null;
    try {
      const app = await this.api.controlApp(this.app.slug, action);
      this.args.model.set(app);
      this.controlNotice = CONTROL_DONE[action];
    } catch (error) {
      this.controlError = error;
    } finally {
      this.controlling = null;
      this.confirmingStop = false;
    }
  };

  requestStop = () => {
    this.controlNotice = '';
    this.controlError = null;
    this.confirmingStop = true;
  };

  cancelStop = () => {
    this.confirmingStop = false;
  };

  confirmStop = () => this.control('stop');

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
    {{#if this.readOnly}}
      <RoleNote @needed="manage" @what="設定の変更" />
    {{/if}}
    <form
      novalidate
      data-test-settings-form
      {{on "submit" (preventDefault this.save)}}
      {{on "input" this.touch}}
    >
      <fieldset class="role-fieldset" disabled={{this.readOnly}}>
        <div class="form-group">
          <div>
            <h2 class="form-group-title">ソース</h2>
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
                @error={{this.errors.default_branch}}
                data-test-field-group="default_branch"
                as |field|
              >
                <input
                  id={{field.id}}
                  type="text"
                  spellcheck="false"
                  autocomplete="off"
                  value={{this.form.defaultBranch}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="default_branch"
                  {{on "input" (setValue this.form "defaultBranch")}}
                />
              </FormField>

              <FormField
                class="field-wide"
                @label="Compose ファイルのパス"
                @hint="空なら compose.yaml などをリポジトリ直下から自動で探します。"
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
                  placeholder="自動検出"
                  value={{this.form.composePath}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="compose_path"
                  {{on "input" (setValue this.form "composePath")}}
                />
              </FormField>
            </div>
          </div>
        </div>

        <div class="form-group auto-deploy-group" data-test-auto-deploy-group>
          <div>
            <h2 class="form-group-title">自動デプロイ</h2>
            <p class="form-group-description">
              GitHub にブランチが push
              されたら、そのコミットを自動でデプロイします。結果は手動のデプロイと同じく履歴に残ります。
            </p>
          </div>
          <div class="form-group-body">
            <label class="option">
              <input
                type="checkbox"
                checked={{this.form.autoDeploy}}
                disabled={{this.autoDeployDisabled}}
                aria-describedby="settings-auto-deploy-hint"
                data-test-field="auto_deploy"
                {{on "change" (setChecked this.form "autoDeploy")}}
              />
              <span class="option-body">
                <span class="option-label">push されたら自動でデプロイする</span>
                <span class="option-hint" id="settings-auto-deploy-hint">
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

            {{#if this.form.autoDeploy}}
              <FormField
                class="field-narrow"
                @label="デプロイするブランチ"
                @hint="空なら既定のブランチ（{{this.form.defaultBranch}}）に従います。"
                @error={{this.errors.auto_deploy_branch}}
                data-test-field-group="auto_deploy_branch"
                as |field|
              >
                <input
                  id={{field.id}}
                  type="text"
                  spellcheck="false"
                  autocomplete="off"
                  placeholder={{this.form.defaultBranch}}
                  value={{this.form.autoDeployBranch}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="auto_deploy_branch"
                  {{on "input" (setValue this.form "autoDeployBranch")}}
                />
              </FormField>

              <FormField
                @label="先に待つワークフロー（任意）"
                @hint="GitHub Actions でイメージを作るなら、そのワークフローのファイルを入れます。push ではなく、そのワークフローがこのブランチの最新のコミットで成功したときにデプロイします。空なら push の直後にデプロイします。"
                @error={{this.errors.auto_deploy_workflow}}
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
                  value={{this.form.autoDeployWorkflow}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="auto_deploy_workflow"
                  {{on "input" (setValue this.form "autoDeployWorkflow")}}
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
            {{#if this.errors.auto_deploy}}
              <p class="field-error" data-test-field-error>
                {{this.errors.auto_deploy}}
              </p>
            {{/if}}
          </div>
        </div>

        <div class="form-group">
          <div>
            <h2 class="form-group-title">公開</h2>
            <p class="form-group-description">
              外からのリクエストを受けるサービスと、デプロイ後に応答を確かめるパスです。
            </p>
          </div>
          <div class="form-group-body">
            <div class="form-grid">
              <FormField
                @label="公開するサービス"
                @error={{this.errors.web_service}}
                data-test-field-group="web_service"
                as |field|
              >
                <input
                  id={{field.id}}
                  type="text"
                  spellcheck="false"
                  autocomplete="off"
                  value={{this.form.webService}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="web_service"
                  {{on "input" (setValue this.form "webService")}}
                />
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
                  value={{this.form.webPort}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="web_port"
                  {{on "input" (setValue this.form "webPort")}}
                />
              </FormField>

              <FormField
                @label="ヘルスチェックのパス"
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
                  value={{this.form.healthcheckPath}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="healthcheck_path"
                  {{on "input" (setValue this.form "healthcheckPath")}}
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
          </div>
        </div>

        <div class="form-group">
          <div>
            <h2 class="form-group-title">環境</h2>
            <p class="form-group-description">
              本番とステージングを取り違えないための目印です。ホストのメモリが足りなくなったときの優先度も決まり、こちらは次のデプロイから反映されます。
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
                  checked={{isChecked this.form.environment "staging"}}
                  disabled={{if this.isOwner false true}}
                  data-test-environment-option="staging"
                  {{on "change" this.setEnvironment}}
                />
                <span class="option-body">
                  <span class="option-label">ステージング</span>
                  <span
                    class="option-hint"
                  >デプロイは確認なしで始まります。メモリが足りなくなると、本番より先に止まります。</span>
                </span>
              </label>
              <label class="option">
                <input
                  type="radio"
                  name="environment"
                  value="production"
                  checked={{isChecked this.form.environment "production"}}
                  disabled={{if this.isOwner false true}}
                  data-test-environment-option="production"
                  {{on "change" this.setEnvironment}}
                />
                <span class="option-body">
                  <span class="option-label">本番</span>
                  <span class="option-hint">
                    バッジで目立たせ、デプロイの前に確認します。メモリが足りなくなっても最後まで止まらず、メモリを予約します。
                  </span>
                </span>
              </label>
            </fieldset>
          </div>
        </div>

        <div class="form-group" data-test-protection-group>
          <div>
            <h2 class="form-group-title">アクセスの保護</h2>
            <p class="form-group-description">
              このアプリのアドレス（独自ドメインを含む）を開けるのを、この管理画面にログインしている人だけにします。アプリには手を入れず、保存するとすぐに反映されます。
            </p>
          </div>
          <div class="form-group-body">
            <label class="option">
              <input
                type="checkbox"
                checked={{this.form.accessProtection}}
                aria-describedby="settings-protection-hint"
                data-test-field="access_protection"
                {{on "change" (setChecked this.form "accessProtection")}}
              />
              <span class="option-body">
                <span
                  class="option-label"
                >管理画面にログインしている人だけに公開する</span>
                <span class="option-hint" id="settings-protection-hint">
                  ログインしていない人が開くと、管理画面のログインを経て元のページに戻ります。ステージングのアプリは最初から有効です。ブラウザー以外（API
                  の呼び出しや監視サービス）は通れません。
                </span>
              </span>
            </label>

            {{#if this.form.accessProtection}}
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
                  Webhook
                  など、ログインできない呼び出し元のためのパスです。前方一致で、大文字と小文字は区別しません。ディレクトリは末尾を
                  / にします（例
                  <code
                  >/webhooks/</code>）。外したパスは、アプリ自身で認証してください。
                </p>
                {{#if this.form.exemptPaths.length}}
                  <ul class="exempt-rows">
                    {{#each this.form.exemptPaths key="id" as |row|}}
                      <li class="exempt-row" data-test-exempt-row>
                        <input
                          type="text"
                          class="mono"
                          placeholder="/webhooks/"
                          spellcheck="false"
                          autocomplete="off"
                          aria-label="保護を外すパス"
                          aria-invalid={{if (this.exemptRowInvalid row) "true"}}
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
                {{#if this.errors.protection_exempt_paths}}
                  <p class="field-error" data-test-field-error>
                    {{this.errors.protection_exempt_paths}}
                  </p>
                {{/if}}
              </div>
            {{else if this.publicStaging}}
              <Banner class="protection-public" data-test-protection-public>
                このステージングのアプリは誰でも開けます。検索エンジンに載らないよう、応答には
                <code>X-Robots-Tag: noindex</code>
                を付けます。
              </Banner>
            {{/if}}
          </div>
        </div>

        <div class="form-group" data-test-idle-sleep-group>
          <div>
            <h2 class="form-group-title">アクセスがない間の休止</h2>
            <p class="form-group-description">
              決めた時間アクセスがなければアプリのコンテナを止め、ホストのメモリを空けます。次のアクセスで起動して応答します。たまにしか開かないステージングのアプリ向けです。
            </p>
          </div>
          <div class="form-group-body">
            <label class="option">
              <input
                type="checkbox"
                checked={{this.form.idleSleep}}
                disabled={{this.idleSleepDisabled}}
                aria-describedby="settings-idle-sleep-hint"
                data-test-field="idle_sleep"
                {{on "change" (setChecked this.form "idleSleep")}}
              />
              <span class="option-body">
                <span class="option-label">アクセスがない間は止める</span>
                <span class="option-hint" id="settings-idle-sleep-hint">
                  休止のあとの最初のアクセスは、アプリの起動を待ちます（Rails
                  なら 5〜15
                  秒ほど）。クローラーや外部の死活監視が定期的にアクセスしていると休止しません。
                </span>
              </span>
            </label>
            {{#if this.idleSleepUnavailable}}
              <p
                class="field-hint idle-sleep-unavailable"
                data-test-idle-sleep-unavailable
              >
                このホストでは使えません。アクセスを数えるために、Caddy
                のアプリ別のアクセスログ（<code>[caddy] access_log_dir</code>）が要ります。
              </p>
            {{/if}}
            {{#if this.form.idleSleep}}
              <FormField
                class="idle-sleep-minutes"
                @label="止めるまでの時間"
                @error={{this.errors.idle_sleep_minutes}}
                data-test-field-group="idle_sleep_minutes"
                as |field|
              >
                <select
                  id={{field.id}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="idle_sleep_minutes"
                  {{on "change" this.setIdleMinutes}}
                >
                  {{#each this.idleMinutesChoices as |minutes|}}
                    <option
                      value={{minutes}}
                      selected={{eq minutes this.form.idleSleepMinutes}}
                    >{{minutesLabel minutes}}</option>
                  {{/each}}
                </select>
              </FormField>
            {{/if}}
            {{#if this.errors.idle_sleep}}
              <p class="field-error" data-test-field-error>
                {{this.errors.idle_sleep}}
              </p>
            {{/if}}
          </div>
        </div>

        {{#if this.isProductionForm}}
          <div class="form-group" data-test-promotion-group>
            <div>
              <h2 class="form-group-title">昇格元</h2>
              <p class="form-group-description">
                同じリポジトリのステージングアプリを選ぶと、デプロイの画面から、そのアプリで稼働中のリリースを本番へ昇格できます。ビルド引数が同じなら、ビルドしたイメージも再利用します。
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
                @error={{this.errors.promote_from}}
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
                  <option value="" selected={{eq this.form.promoteFrom ""}}>
                    なし
                  </option>
                  {{#if this.savedSourceMissing}}
                    <option
                      value={{this.app.promote_from}}
                      selected={{eq
                        this.form.promoteFrom
                        this.app.promote_from
                      }}
                    >{{this.app.promote_from}}（条件に合いません）</option>
                  {{/if}}
                  {{#each this.promotionSources key="slug" as |other|}}
                    <option
                      value={{other.slug}}
                      selected={{eq this.form.promoteFrom other.slug}}
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

        <div class="form-group" data-test-env-source-group>
          <div>
            <h2 class="form-group-title">必要な環境変数の照合</h2>
            <p class="form-group-description">
              リポジトリが必要とする環境変数を読み、環境変数の画面とデプロイの前に、未設定のものを知らせます。デプロイは止めません。
            </p>
          </div>
          <div class="form-group-body">
            <fieldset class="option-group">
              <legend class="visually-hidden">照合元</legend>
              <label class="option">
                <input
                  type="radio"
                  name="env_source"
                  value="compose"
                  checked={{isSource this.form.envSource "compose"}}
                  data-test-env-source-option="compose"
                  {{on "change" this.setEnvSource}}
                />
                <span class="option-body">
                  <span class="option-label">Compose ファイルの
                    <code>{{VAR_PLAIN}}</code>（推奨）</span>
                  <span class="option-hint">
                    <code>{{VAR_REQUIRED}}</code>
                    と既定値のない参照は必須、<code>{{VAR_OPTIONAL}}</code>
                    は任意として扱います。アプリのコードだけが読む変数は対象外です。
                  </span>
                </span>
              </label>
              <label class="option">
                <input
                  type="radio"
                  name="env_source"
                  value="dotenv"
                  checked={{isSource this.form.envSource "dotenv"}}
                  data-test-env-source-option="dotenv"
                  {{on "change" this.setEnvSource}}
                />
                <span class="option-body">
                  <span class="option-label">.env.example</span>
                  <span class="option-hint">
                    書かれたキーをすべて必須として扱い、直前のコメントを説明として表示します。
                  </span>
                </span>
              </label>
              <label class="option">
                <input
                  type="radio"
                  name="env_source"
                  value="off"
                  checked={{isSource this.form.envSource "off"}}
                  data-test-env-source-option="off"
                  {{on "change" this.setEnvSource}}
                />
                <span class="option-body">
                  <span class="option-label">照合しない</span>
                </span>
              </label>
            </fieldset>
            {{#if (isSource this.form.envSource "dotenv")}}
              <FormField
                class="field-wide"
                @label=".env.example のパス"
                @hint="空ならリポジトリ直下の .env.example を読みます。"
                @error={{this.errors.env_example_path}}
                data-test-field-group="env_example_path"
                as |field|
              >
                <input
                  id={{field.id}}
                  type="text"
                  class="mono"
                  spellcheck="false"
                  autocomplete="off"
                  placeholder=".env.example"
                  value={{this.form.envExamplePath}}
                  aria-describedby={{field.describedBy}}
                  aria-invalid={{field.invalid}}
                  data-test-field="env_example_path"
                  {{on "input" (setValue this.form "envExamplePath")}}
                />
              </FormField>
            {{/if}}
            {{#if this.errors.env_source}}
              <p class="field-error">{{this.errors.env_source}}</p>
            {{/if}}
            <p class="settings-requirements" aria-live="polite">
              {{#if this.requirements.loading}}
                <span data-test-settings-requirements-loading>
                  <Icon @name="spinner" />
                  必要な環境変数を確かめています…
                </span>
              {{else if this.requirementsSummary}}
                <span
                  class="settings-requirements-{{this.requirementsSummary.tone}}"
                  data-test-settings-requirements={{this.requirementsSummary.tone}}
                >
                  {{#if (eq this.requirementsSummary.tone "warning")}}
                    <Icon @name="warning" />
                  {{/if}}
                  {{this.requirementsSummary.text}}
                </span>
                <LinkTo
                  @route="app.env"
                  @model={{this.app.slug}}
                  data-test-settings-requirements-link
                >環境変数を開く</LinkTo>
              {{/if}}
            </p>
          </div>
        </div>

        <div class="form-group">
          <div>
            <h2 class="form-group-title">メモリ上限</h2>
            <p class="form-group-description">
              サービスごとの上限です。数字に単位 b・k・m・g
              のどれかを付けて書きます（例:
              512m、1g）。使用量の履歴と上限の目安は<LinkTo
                @route="app.resources"
                @model={{this.app.slug}}
                data-test-settings-resources-link
              >リソース</LinkTo>タブで確かめられます。
            </p>
          </div>
          <div class="form-group-body">
            <FormField
              class="field-narrow"
              @label="既定（全サービス共通）"
              @hint="空なら基盤の既定値を使います。"
              @error={{this.errors.memory_limit}}
              data-test-field-group="memory_limit"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                spellcheck="false"
                autocomplete="off"
                placeholder="基盤の既定値"
                value={{this.form.memoryLimit}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="memory_limit"
                {{on "input" (setValue this.form "memoryLimit")}}
              />
            </FormField>

            <div
              class="field"
              role="group"
              aria-labelledby="settings-limits-label"
              data-test-field-group="service_memory_limits"
            >
              <span class="field-label" id="settings-limits-label">
                サービス別
              </span>
              {{#if this.form.limits.length}}
                <ul class="limit-rows">
                  {{#each this.form.limits key="id" as |row|}}
                    <li class="limit-row" data-test-limit-row>
                      <input
                        type="text"
                        placeholder="サービス名"
                        spellcheck="false"
                        autocomplete="off"
                        aria-label="サービス名"
                        value={{row.service}}
                        data-test-limit-service
                        {{on "input" (setValue row "service")}}
                      />
                      <input
                        type="text"
                        placeholder="1g"
                        spellcheck="false"
                        autocomplete="off"
                        aria-label="{{if
                          row.service
                          row.service
                          'サービス'
                        }} の上限"
                        aria-invalid={{if
                          this.errors.service_memory_limits
                          "true"
                        }}
                        value={{row.limit}}
                        data-test-limit-value
                        {{on "input" (setValue row "limit")}}
                      />
                      <Button
                        @size="small"
                        aria-label="{{if
                          row.service
                          row.service
                          'この行'
                        }} の上限を外す"
                        data-test-limit-remove
                        {{on "click" (fn this.removeLimit row)}}
                      >外す</Button>
                    </li>
                  {{/each}}
                </ul>
              {{/if}}
              <div>
                <Button
                  @size="small"
                  data-test-limit-add
                  {{on "click" this.addLimit}}
                >サービス別の上限を追加</Button>
              </div>
              {{#if this.errors.service_memory_limits}}
                <p class="field-error" data-test-field-error>
                  {{this.errors.service_memory_limits}}
                </p>
              {{/if}}
            </div>

          </div>
        </div>

        <div class="form-group" data-test-reservation-group>
          <div>
            <h2 class="form-group-title">メモリの予約（本番）</h2>
            <p class="form-group-description">
              ホストのメモリが足りなくなったとき、予約した量まではほかのアプリより後に回収されます。空なら各サービスの上限の半分、0
              なら予約しません。上限を超える値は上限に切り詰めます。ホスト全体の合計はシステム画面で確かめられます。
            </p>
          </div>
          <div class="form-group-body">
            {{#unless this.isProductionForm}}
              <p
                class="field-hint reservation-staging-note"
                data-test-reservation-staging-note
              >
                ステージングのアプリには付きません。本番に切り替えると、次のデプロイから付きます。
              </p>
            {{/unless}}
            {{#if this.reservationChanges.length}}
              <div class="reservation-changes" data-test-reservation-changes>
                <p class="field-hint">
                  保存した設定は次のデプロイから反映されます。稼働中のリリースとの違い:
                </p>
                <ul>
                  {{#each this.reservationChanges key="name" as |change|}}
                    <li data-test-reservation-change={{change.name}}>
                      <span class="mono">{{change.name}}</span>
                      稼働中
                      {{change.running}}
                      → 次のデプロイから
                      <strong>{{change.next}}</strong>
                    </li>
                  {{/each}}
                </ul>
              </div>
            {{/if}}
            <FormField
              class="field-narrow"
              @label="既定（全サービス共通）"
              @hint="空なら上限の半分です。"
              @error={{this.errors.memory_reservation}}
              data-test-field-group="memory_reservation"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                spellcheck="false"
                autocomplete="off"
                placeholder="上限の半分"
                value={{this.form.memoryReservation}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="memory_reservation"
                {{on "input" (setValue this.form "memoryReservation")}}
              />
            </FormField>

            <div
              class="field"
              role="group"
              aria-labelledby="settings-reservations-label"
              data-test-field-group="service_memory_reservations"
            >
              <span class="field-label" id="settings-reservations-label">
                サービス別
              </span>
              {{#if this.form.reservations.length}}
                <ul class="limit-rows">
                  {{#each this.form.reservations key="id" as |row|}}
                    <li class="limit-row" data-test-reservation-row>
                      <input
                        type="text"
                        placeholder="サービス名"
                        spellcheck="false"
                        autocomplete="off"
                        aria-label="サービス名"
                        value={{row.service}}
                        data-test-reservation-service
                        {{on "input" (setValue row "service")}}
                      />
                      <input
                        type="text"
                        placeholder="256m"
                        spellcheck="false"
                        autocomplete="off"
                        aria-label="{{if
                          row.service
                          row.service
                          'サービス'
                        }} の予約"
                        aria-invalid={{if
                          this.errors.service_memory_reservations
                          "true"
                        }}
                        value={{row.limit}}
                        data-test-reservation-value
                        {{on "input" (setValue row "limit")}}
                      />
                      <Button
                        @size="small"
                        aria-label="{{if
                          row.service
                          row.service
                          'この行'
                        }} の予約を外す"
                        data-test-reservation-remove
                        {{on "click" (fn this.removeReservation row)}}
                      >外す</Button>
                    </li>
                  {{/each}}
                </ul>
              {{/if}}
              <div>
                <Button
                  @size="small"
                  data-test-reservation-add
                  {{on "click" this.addReservation}}
                >サービス別の予約を追加</Button>
              </div>
              {{#if this.errors.service_memory_reservations}}
                <p class="field-error" data-test-field-error>
                  {{this.errors.service_memory_reservations}}
                </p>
              {{/if}}
            </div>
          </div>
        </div>

        <div class="form-group">
          <div>
            <h2 class="form-group-title">デプロイの安全装置</h2>
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

            {{#unless this.isOwner}}
              {{#unless this.readOnly}}
                <RoleNote
                  @needed="owner"
                  @what="ポリシー検査と昇格元"
                  data-test-owner-only-note
                />
              {{/unless}}
            {{/unless}}
            <label class="option">
              <input
                type="checkbox"
                checked={{this.form.relaxPolicy}}
                aria-describedby="settings-relax-hint"
                disabled={{if this.isOwner false true}}
                data-test-field="relax_policy"
                {{on "change" (setChecked this.form "relaxPolicy")}}
              />
              <span class="option-body">
                <span class="option-label">Compose のポリシー検査を緩める</span>
                <span class="option-hint" id="settings-relax-hint">
                  特権コンテナ、ホストのネットワーク、Docker
                  ソケットやリリース外の bind mount
                  など、通常は拒否する指定を通します。項目ごとには選べません。
                </span>
              </span>
            </label>
            {{#if this.form.relaxPolicy}}
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
          <Button
            @type="submit"
            @variant="primary"
            @busy={{this.saving}}
            @busyLabel="保存中…"
            data-test-settings-save
          >保存</Button>
          <p class="form-status" aria-live="polite">
            {{#if this.saved}}
              <span class="form-saved" data-test-settings-saved>
                <Icon @name="check" />
                保存しました。Compose、メモリ上限と予約、環境による優先度の変更は、次のデプロイから反映されます。
              </span>
            {{else if this.nothingToSave}}
              <span data-test-settings-unchanged>変更はありません。</span>
            {{else}}
              Compose、メモリ上限と予約、環境による優先度の変更は、次のデプロイから反映されます。
            {{/if}}
          </p>
        </div>
      </fieldset>
    </form>

    <RegistryCredential @slug={{this.app.slug}} @canEdit={{this.canManage}} />

    {{#if this.canDeploy}}
      <section
        class="pane settings-operations"
        aria-labelledby="operations-title"
      >
        <div class="pane-header">
          <h2 class="pane-title" id="operations-title">起動と再起動</h2>
          <StatusIndicator
            @kind={{this.state.kind}}
            @label={{this.state.label}}
            title={{this.state.hint}}
            data-test-settings-state
          />
        </div>

        <div class="pane-notices" aria-live="polite">
          {{#if this.controlNotice}}
            <Banner data-test-notice>{{this.controlNotice}}</Banner>
          {{/if}}
          {{#if this.controlError}}
            <ErrorMessage @error={{this.controlError}} />
            {{#if this.violations.length}}
              <ul class="form-problems" data-test-violations>
                {{#each this.violations as |line|}}
                  <li>{{line}}</li>
                {{/each}}
              </ul>
            {{/if}}
          {{/if}}
        </div>

        <ul class="action-rows">
          {{#if this.stopped}}
            <li class="action-row">
              <div class="action-row-text">
                <p class="action-row-title">起動</p>
                <p class="action-row-description">
                  稼働中だったリリースでコンテナを起動します。
                </p>
              </div>
              <Button
                @disabled={{this.controlsDisabled}}
                @busy={{isBusy this.controlling "start"}}
                @busyLabel="起動中…"
                data-test-control="start"
                {{on "click" (fn this.control "start")}}
              >起動</Button>
            </li>
          {{else}}
            <li class="action-row">
              <div class="action-row-text">
                <p class="action-row-title">再起動</p>
                <p class="action-row-description">
                  {{#if this.neverDeployed}}
                    まだデプロイされていないため、起動や停止はできません。
                  {{else}}
                    いまのリリースのままコンテナを再起動します。数秒のあいだ応答が止まります。
                  {{/if}}
                </p>
              </div>
              <Button
                @disabled={{this.controlsDisabled}}
                @busy={{isBusy this.controlling "restart"}}
                @busyLabel="再起動中…"
                data-test-control="restart"
                {{on "click" (fn this.control "restart")}}
              >再起動</Button>
            </li>
          {{/if}}
        </ul>
      </section>
    {{/if}}

    {{#if this.canDeploy}}
      <section
        class="danger-zone"
        aria-labelledby="danger-title"
        data-test-danger-zone
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
              <p class="action-row-title">アプリを停止</p>
              <p class="action-row-description">
                {{#if this.stopped}}
                  すでに停止しています。
                {{else}}
                  コンテナを止めます。起動するまで、アクセスには 503
                  のページが返ります。データは消えません。
                {{/if}}
              </p>
            </div>
            <Button
              class="button-danger-outline"
              @disabled={{this.cannotStop}}
              data-test-control="stop"
              {{on "click" this.requestStop}}
            >停止…</Button>
          </li>
          {{#if this.canDelete}}
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
          {{/if}}
        </ul>
      </section>
    {{/if}}

    {{#if this.pendingLeave}}
      <ConfirmDialog
        @title="保存していない変更があります"
        @confirmLabel="保存して進む"
        @altLabel="保存せずに進む"
        @busy={{this.saving}}
        @onConfirm={{this.saveAndLeave}}
        @onAlt={{this.leaveWithoutSaving}}
        @onCancel={{this.cancelLeave}}
      >
        <p data-test-unsaved-dialog>
          設定に保存していない変更があります。デプロイは保存済みの設定で行われます。保存してからデプロイの画面に進みますか？保存せずに進むと、変更は破棄されます。
        </p>
      </ConfirmDialog>
    {{/if}}

    {{#if this.pendingProtection}}
      <ConfirmDialog
        @title={{if
          this.form.accessProtection
          "本番アプリを保護しますか？"
          "本番アプリの保護を外しますか？"
        }}
        @confirmLabel="保存する"
        @danger={{this.publicProduction}}
        @busy={{this.saving}}
        @onConfirm={{this.confirmProtection}}
        @onCancel={{this.cancelProtection}}
      >
        {{#if this.form.accessProtection}}
          <p data-test-protection-confirm="on">
            保存すると、この管理画面にログインしていない利用者は、本番アプリ
            <strong>{{this.app.slug}}</strong>
            を開けなくなります。
          </p>
        {{else}}
          <p data-test-protection-confirm="off">
            保存すると、本番アプリ
            <strong>{{this.app.slug}}</strong>
            は誰でも開けるようになります。
          </p>
        {{/if}}
      </ConfirmDialog>
    {{/if}}

    {{#if this.pendingAutoDeploy}}
      <ConfirmDialog
        @title="本番アプリを push で自動デプロイしますか？"
        @confirmLabel="保存する"
        @busy={{this.saving}}
        @onConfirm={{this.confirmAutoDeploy}}
        @onCancel={{this.cancelAutoDeploy}}
      >
        <p data-test-auto-deploy-confirm>
          保存すると、ブランチ
          <strong class="mono">{{this.form.effectiveAutoDeployBranch}}</strong>
          に push されたコミットが、{{#if this.waitsForWorkflow}}ワークフロー
            <strong class="mono">{{this.form.autoDeployWorkflow}}</strong>
            の成功のあと{{/if}}確認なしで本番アプリ
          <strong>{{this.app.slug}}</strong>
          にデプロイされます。そのブランチに push
          できる人は、本番を更新できるようになります。
        </p>
      </ConfirmDialog>
    {{/if}}

    {{#if this.pendingIdleSleep}}
      <ConfirmDialog
        @title="本番アプリをアクセスがない間に止めますか？"
        @confirmLabel="保存する"
        @busy={{this.saving}}
        @onConfirm={{this.confirmIdleSleep}}
        @onCancel={{this.cancelIdleSleep}}
      >
        <p data-test-idle-sleep-confirm>
          保存すると、本番アプリ
          <strong>{{this.app.slug}}</strong>
          は{{minutesLabel
            this.form.idleSleepMinutes
          }}アクセスがなければ止まります。休止のあとの最初の利用者は、アプリの起動を待つことになります。外部の死活監視を当てていると休止しません。
        </p>
      </ConfirmDialog>
    {{/if}}

    {{#if this.confirmingStop}}
      <ConfirmDialog
        @title={{if
          @model.isProduction
          "本番アプリを停止しますか？"
          "アプリを停止しますか？"
        }}
        @confirmLabel="停止する"
        @danger={{true}}
        @busy={{isBusy this.controlling "stop"}}
        @onConfirm={{this.confirmStop}}
        @onCancel={{this.cancelStop}}
      >
        <p>
          {{if @model.isProduction "本番アプリ" "アプリ"}}
          <strong>{{this.app.slug}}</strong>
          のコンテナを止めます。起動するまで、アクセスには 503
          のページが返ります。
        </p>
      </ConfirmDialog>
    {{/if}}

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
      </ConfirmDialog>
    {{/if}}
  </template>
}
