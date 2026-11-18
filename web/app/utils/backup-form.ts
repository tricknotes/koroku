import { tracked } from '@glimmer/tracking';
import type { App, AppPatch } from 'paas-console/utils/types';

/** `a, b` or one per line → `['a', 'b']`. */
export function parseList(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((item) => item.trim())
    .filter((item) => item !== '');
}

function sameSet(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((item) => b.includes(item));
}

/**
 * Editable copy of an app's backup settings: which volumes are left out,
 * the dump hook and "back up before a deployment". `patch` returns only
 * what differs from the app, because `PATCH` updates just the fields it is
 * sent (and a backend that does not report these fields would reject them).
 */
export default class BackupForm {
  /** Volume names that are not backed up. */
  @tracked excluded: string[];
  @tracked dumpHookService: string;
  @tracked dumpHookCommand: string;
  @tracked backupBeforeDeploy: boolean;

  /** Whether the backend reports the backup fields at all. */
  readonly available: boolean;

  constructor(app: App) {
    this.available = app.backup_before_deploy !== undefined;
    this.excluded = [...(app.backup_exclude_volumes ?? [])];
    this.dumpHookService = app.dump_hook_service ?? '';
    this.dumpHookCommand = app.dump_hook_command ?? '';
    this.backupBeforeDeploy = app.backup_before_deploy ?? false;
  }

  isExcluded(volume: string): boolean {
    return this.excluded.includes(volume);
  }

  /** Puts a volume back into the backup, or takes it out. */
  setIncluded(volume: string, included: boolean): void {
    const others = this.excluded.filter((name) => name !== volume);
    this.excluded = included ? others : [...others, volume];
  }

  /** Field name (as in the API) → problem found before sending. */
  validate(): Record<string, string> {
    const errors: Record<string, string> = {};
    // The dump hook is a pair: both set or both empty.
    const service = this.dumpHookService.trim();
    const command = this.dumpHookCommand.trim();
    if (command && !service) {
      errors['dump_hook_service'] =
        'コマンドを指定するときはサービスも入力してください。';
    }
    if (service && !command) {
      errors['dump_hook_command'] =
        'サービスを指定するときはコマンドも入力してください。';
    }
    return errors;
  }

  patch(app: App): AppPatch {
    const patch: AppPatch = {};
    if (!this.available) return patch;
    if (!sameSet(this.excluded, app.backup_exclude_volumes ?? [])) {
      patch.backup_exclude_volumes = [...this.excluded];
    }
    const service = this.dumpHookService.trim();
    if (service !== (app.dump_hook_service ?? '')) {
      patch.dump_hook_service = service;
    }
    const command = this.dumpHookCommand.trim();
    if (command !== (app.dump_hook_command ?? '')) {
      patch.dump_hook_command = command;
    }
    if (this.backupBeforeDeploy !== (app.backup_before_deploy ?? false)) {
      patch.backup_before_deploy = this.backupBeforeDeploy;
    }
    return patch;
  }
}
