import { tracked } from '@glimmer/tracking';
import type {
  App,
  AppPatch,
  EnvSource,
  Environment,
} from 'koroku-console/utils/types';

let nextLimitId = 0;

export class LimitRow {
  readonly id = `limit-${nextLimitId++}`;
  @tracked service: string;
  @tracked limit: string;

  constructor(service = '', limit = '') {
    this.service = service;
    this.limit = limit;
  }
}

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

const MEMORY_PATTERN = /^[0-9]+[bkmg]$/;

/** Empty (use the default) or digits plus one of `b` `k` `m` `g`. */
export function isValidMemoryLimit(value: string): boolean {
  return value === '' || MEMORY_PATTERN.test(value);
}

function sameRecord(
  a: Record<string, string>,
  b: Record<string, string>,
): boolean {
  const keys = Object.keys(a);
  return (
    keys.length === Object.keys(b).length && keys.every((k) => a[k] === b[k])
  );
}

/**
 * Editable copy of an app's settings (the backup settings have their own
 * form on the backups tab). `patch` returns only what differs from the app,
 * because `PATCH` updates just the fields it is sent.
 */
export default class SettingsForm {
  @tracked defaultBranch: string;
  @tracked composePath: string;
  @tracked webService: string;
  @tracked webPort: string;
  @tracked healthcheckPath: string;
  @tracked environment: Environment;
  @tracked relaxPolicy: boolean;
  @tracked memoryLimit: string;
  @tracked limits: LimitRow[];
  @tracked envSource: EnvSource;
  @tracked envExamplePath: string;
  @tracked accessProtection: boolean;
  @tracked exemptPaths: ExemptRow[];
  /** Slug of the staging app to promote from; '' for none. */
  @tracked promoteFrom: string;
  /** Deploy on push (ARCHITECTURE 5.2); the branch '' follows the default. */
  @tracked autoDeploy: boolean;
  @tracked autoDeployBranch: string;

  constructor(app: App) {
    this.defaultBranch = app.default_branch;
    this.composePath = app.compose_path;
    this.webService = app.web_service;
    this.webPort = String(app.web_port);
    this.healthcheckPath = app.healthcheck_path;
    this.environment = app.environment;
    this.relaxPolicy = app.relax_policy;
    this.memoryLimit = app.memory_limit;
    this.limits = Object.entries(app.service_memory_limits ?? {}).map(
      ([service, limit]) => new LimitRow(service, limit),
    );
    this.envSource = app.env_source ?? 'compose';
    this.envExamplePath = app.env_example_path ?? '';
    this.accessProtection = app.access_protection ?? false;
    this.exemptPaths = (app.protection_exempt_paths ?? []).map(
      (path) => new ExemptRow(path),
    );
    this.promoteFrom = app.promote_from ?? '';
    this.autoDeploy = app.auto_deploy ?? false;
    this.autoDeployBranch = app.auto_deploy_branch ?? '';
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

  get serviceLimits(): Record<string, string> {
    const limits: Record<string, string> = {};
    for (const row of this.limits) {
      const service = row.service.trim();
      if (service) limits[service] = row.limit.trim();
    }
    return limits;
  }

  /** Field name (as in the API) → problem found before sending. */
  validate(): Record<string, string> {
    const errors: Record<string, string> = {};
    const port = Number(this.webPort);
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      errors['web_port'] = '1〜65535 の整数を入力してください。';
    }
    const memoryHint =
      '数字と単位 b / k / m / g で入力してください（例 512m）。';
    if (!isValidMemoryLimit(this.memoryLimit.trim())) {
      errors['memory_limit'] = memoryHint;
    }
    for (const row of this.limits) {
      const limit = row.limit.trim();
      if (row.service.trim() && (limit === '' || !isValidMemoryLimit(limit))) {
        errors['service_memory_limits'] = memoryHint;
      }
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
    const memoryLimit = this.memoryLimit.trim();
    if (memoryLimit !== app.memory_limit) patch.memory_limit = memoryLimit;
    const limits = this.serviceLimits;
    if (!sameRecord(limits, app.service_memory_limits ?? {})) {
      patch.service_memory_limits = limits;
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
    return patch;
  }
}
