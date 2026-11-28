import { tracked } from '@glimmer/tracking';
import {
  DEFAULT_DRAIN_SECONDS,
  drainSecondsProblem,
} from 'koroku-console/utils/blue-green';
import {
  composeContentProblem,
  composeSourceOf,
  normalizeCompose,
} from 'koroku-console/utils/compose-source';
import type {
  App,
  AppPatch,
  ComposeSource,
  EnvSource,
  Environment,
} from 'koroku-console/utils/types';

let nextExemptId = 0;

export class ExemptRow {
  readonly id = `exempt-${nextExemptId++}`;
  @tracked path: string;

  constructor(path = '') {
    this.path = path;
  }
}

/** The most paths an app may exempt; the API refuses more. */
export const MAX_EXEMPT_PATHS = 20;

const EXEMPT_CHARS = /^[A-Za-z0-9._~\-/@:+]+$/;

/**
 * Why a path prefix cannot skip the access protection, or null when it can.
 * Mirrors the API (ARCHITECTURE 8.3): an absolute path of a few safe
 * characters, neither the whole site nor koroku's own `/.koroku`.
 */
export function exemptPathProblem(path: string): string | null {
  if (!path.startsWith('/')) return '/ で始まるパスを入力してください。';
  if (path === '/') {
    return '/ だけでは全体が外れます。保護そのものを無効にしてください。';
  }
  if (path.length > 200) return '200 文字以内で入力してください。';
  if (path.toLowerCase().startsWith('/.koroku')) {
    return '/.koroku は koroku が使うため指定できません。';
  }
  if (path.includes('//')) return '// を含むパスは指定できません。';
  if (path.split('/').some((seg) => seg === '.' || seg === '..')) {
    return '. や .. の区切りは指定できません。';
  }
  if (!EXEMPT_CHARS.test(path)) {
    return '英数字と . _ ~ - / @ : + だけで入力してください（* や % は使えません）。';
  }
  return null;
}

/** The idle time of a new app, as the API has it. */
export const DEFAULT_IDLE_MINUTES = 30;

/** The idle times the settings offer (the API takes 10 to 1440). */
export const IDLE_MINUTES_CHOICES = [15, 30, 60, 180];

/** "30 分", "3 時間": an idle time as the settings show it. */
export function minutesLabel(minutes: number): string {
  return minutes % 60 === 0 ? `${minutes / 60} 時間` : `${minutes} 分`;
}

const MEMORY_PATTERN = /^[0-9]+[bkmg]$/;

/** Empty (use the default) or digits plus one of `b` `k` `m` `g`. */
export function isValidMemoryLimit(value: string): boolean {
  return value === '' || MEMORY_PATTERN.test(value);
}

/** Empty (the default), 0 (none) or a size like a memory limit. */
export function isValidReservation(value: string): boolean {
  return value === '0' || isValidMemoryLimit(value);
}

/**
 * Editable copy of an app's settings (the backup settings have their own
 * form on the backups tab, the memory limits and reservations are saved one
 * value at a time on the resources tab). `patch` returns only what differs
 * from the app, because `PATCH` updates just the fields it is sent. Each tab
 * saves its own fields of it (utils/settings-editor).
 */
export default class SettingsForm {
  @tracked defaultBranch: string;
  @tracked composePath: string;
  /** The repository's Compose file or one written here (ARCHITECTURE 6.3). */
  @tracked composeSource: ComposeSource;
  @tracked composeContent: string;
  /** The role is not shown the file, so it is not this form's to check. */
  private composeHidden: boolean;
  @tracked webService: string;
  @tracked webPort: string;
  @tracked healthcheckPath: string;
  @tracked environment: Environment;
  @tracked relaxPolicy: boolean;
  @tracked envSource: EnvSource;
  @tracked envExamplePath: string;
  @tracked accessProtection: boolean;
  @tracked exemptPaths: ExemptRow[];
  /** Slug of the staging app to promote from; '' for none. */
  @tracked promoteFrom: string;
  /** Deploy on push (ARCHITECTURE 5.2); the branch '' follows the default. */
  @tracked autoDeploy: boolean;
  @tracked autoDeployBranch: string;
  /** The workflow to wait for before deploying a push; '' for none. */
  @tracked autoDeployWorkflow: string;
  /** Sleep while idle (ARCHITECTURE 8.4) and after how many minutes. */
  @tracked idleSleep: boolean;
  @tracked idleSleepMinutes: number;
  /** Blue/green deploys of the public service (ARCHITECTURE 7.2). */
  @tracked blueGreen: boolean;
  /** The drain in seconds, as typed. */
  @tracked blueGreenDrainSeconds: string;

  constructor(app: App) {
    this.defaultBranch = app.default_branch;
    this.composePath = app.compose_path;
    this.composeSource = composeSourceOf(app);
    this.composeContent = app.compose_content ?? '';
    this.composeHidden = app.compose_content_hidden === true;
    this.webService = app.web_service;
    this.webPort = String(app.web_port);
    this.healthcheckPath = app.healthcheck_path;
    this.environment = app.environment;
    this.relaxPolicy = app.relax_policy;
    this.envSource = app.env_source ?? 'compose';
    this.envExamplePath = app.env_example_path ?? '';
    this.accessProtection = app.access_protection ?? false;
    this.exemptPaths = (app.protection_exempt_paths ?? []).map(
      (path) => new ExemptRow(path),
    );
    this.promoteFrom = app.promote_from ?? '';
    this.autoDeploy = app.auto_deploy ?? false;
    this.autoDeployBranch = app.auto_deploy_branch ?? '';
    this.autoDeployWorkflow = app.auto_deploy_workflow ?? '';
    this.idleSleep = app.idle_sleep ?? false;
    this.idleSleepMinutes = app.idle_sleep_minutes ?? DEFAULT_IDLE_MINUTES;
    this.blueGreen = app.blue_green ?? false;
    this.blueGreenDrainSeconds = String(
      app.blue_green_drain_seconds ?? DEFAULT_DRAIN_SECONDS,
    );
  }

  /** The branch whose pushes would deploy, as the form stands. */
  get effectiveAutoDeployBranch(): string {
    return this.autoDeployBranch.trim() || this.defaultBranch.trim();
  }

  /** The exempt paths as they would be saved: trimmed, no blanks or repeats. */
  get exemptList(): string[] {
    const paths: string[] = [];
    for (const row of this.exemptPaths) {
      const path = row.path.trim();
      if (path && !paths.includes(path)) paths.push(path);
    }
    return paths;
  }

  /** Field name (as in the API) → problem found before sending. */
  validate(): Record<string, string> {
    const errors: Record<string, string> = {};
    const port = Number(this.webPort);
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      errors['web_port'] = '1〜65535 の整数を入力してください。';
    }
    const exempt = this.exemptList;
    if (exempt.length > MAX_EXEMPT_PATHS) {
      errors['protection_exempt_paths'] =
        `${MAX_EXEMPT_PATHS} 件までにしてください。`;
    }
    for (const path of exempt) {
      const problem = exemptPathProblem(path);
      if (problem) {
        errors['protection_exempt_paths'] = `${path}: ${problem}`;
        break;
      }
    }
    const drain = drainSecondsProblem(this.blueGreenDrainSeconds);
    if (drain) errors['blue_green_drain_seconds'] = drain;
    if (this.composeSource === 'inline' && !this.composeHidden) {
      const compose = composeContentProblem(this.composeContent);
      if (compose) errors['compose_content'] = compose;
    }
    return errors;
  }

  patch(app: App): AppPatch {
    const patch: AppPatch = {};
    const defaultBranch = this.defaultBranch.trim();
    if (defaultBranch !== app.default_branch) {
      patch.default_branch = defaultBranch;
    }
    const composePath = this.composePath.trim();
    if (composePath !== app.compose_path) patch.compose_path = composePath;
    if (this.composeSource !== composeSourceOf(app)) {
      patch.compose_source = this.composeSource;
    }
    // A role that is not shown the file cannot have edited it.
    if (!app.compose_content_hidden) {
      const content = normalizeCompose(this.composeContent);
      if (content !== (app.compose_content ?? '')) {
        patch.compose_content = content;
      }
    }
    const webService = this.webService.trim();
    if (webService !== app.web_service) patch.web_service = webService;
    const webPort = Number(this.webPort);
    if (webPort !== app.web_port) patch.web_port = webPort;
    const healthcheckPath = this.healthcheckPath.trim();
    if (healthcheckPath !== app.healthcheck_path) {
      patch.healthcheck_path = healthcheckPath;
    }
    if (this.environment !== app.environment) {
      patch.environment = this.environment;
    }
    if (this.relaxPolicy !== app.relax_policy) {
      patch.relax_policy = this.relaxPolicy;
    }
    if (this.envSource !== (app.env_source ?? 'compose')) {
      patch.env_source = this.envSource;
    }
    const envExamplePath = this.envExamplePath.trim();
    if (envExamplePath !== (app.env_example_path ?? '')) {
      patch.env_example_path = envExamplePath;
    }
    if (this.accessProtection !== (app.access_protection ?? false)) {
      patch.access_protection = this.accessProtection;
    }
    const exempt = this.exemptList;
    const saved = app.protection_exempt_paths ?? [];
    if (
      exempt.length !== saved.length ||
      exempt.some((path, i) => path !== saved[i])
    ) {
      patch.protection_exempt_paths = exempt;
    }
    if (this.promoteFrom !== (app.promote_from ?? '')) {
      patch.promote_from = this.promoteFrom;
    }
    if (this.autoDeploy !== (app.auto_deploy ?? false)) {
      patch.auto_deploy = this.autoDeploy;
    }
    const autoDeployBranch = this.autoDeployBranch.trim();
    if (autoDeployBranch !== (app.auto_deploy_branch ?? '')) {
      patch.auto_deploy_branch = autoDeployBranch;
    }
    const autoDeployWorkflow = this.autoDeployWorkflow.trim();
    if (autoDeployWorkflow !== (app.auto_deploy_workflow ?? '')) {
      patch.auto_deploy_workflow = autoDeployWorkflow;
    }
    if (this.idleSleep !== (app.idle_sleep ?? false)) {
      patch.idle_sleep = this.idleSleep;
    }
    if (
      this.idleSleepMinutes !== (app.idle_sleep_minutes ?? DEFAULT_IDLE_MINUTES)
    ) {
      patch.idle_sleep_minutes = this.idleSleepMinutes;
    }
    if (this.blueGreen !== (app.blue_green ?? false)) {
      patch.blue_green = this.blueGreen;
    }
    // A value that does not pass the check still counts as a change (the
    // unsaved-changes guard asks about it); validate() keeps it from being
    // sent.
    if (drainSecondsProblem(this.blueGreenDrainSeconds) === null) {
      const drain = Number(this.blueGreenDrainSeconds.trim());
      if (drain !== (app.blue_green_drain_seconds ?? DEFAULT_DRAIN_SECONDS)) {
        patch.blue_green_drain_seconds = drain;
      }
    } else {
      patch.blue_green_drain_seconds = Number.NaN;
    }
    return patch;
  }
}
