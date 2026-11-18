import { tracked } from '@glimmer/tracking';
import type { App, AppPatch, Environment } from 'paas-console/utils/types';

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
    return patch;
  }
}
