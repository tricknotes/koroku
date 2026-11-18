import { tracked } from '@glimmer/tracking';
import type {
  App,
  AppPatch,
  AppVolume,
  Snapshot,
} from 'koroku-console/utils/types';

/** One named volume of the app and whether it is backed up. */
export interface VolumeChoice {
  name: string;
  included: boolean;
  /** Time of its newest snapshot, if it has one. */
  latest: string | null;
  /**
   * No volume of this name exists in Docker: it was excluded by name ahead
   * of time, or it is gone. Only known with the volume list.
   */
  missing: boolean;
}

/**
 * The volumes to choose from for the backup, sorted by name.
 *
 * `listed` is the app's volume list from the API, or `null` when it is not
 * available. The volumes that have a snapshot then stand in for it, which
 * leaves out those that were never backed up. Names that are excluded, in
 * the form or (`also`) saved or typed in, are always there, so that they
 * can be put back in.
 */
export function volumeChoices(options: {
  listed: AppVolume[] | null;
  snapshots: Snapshot[];
  /** Names that are not backed up, as the form stands. */
  excluded: string[];
  /** More names to keep on the list. */
  also?: string[];
}): VolumeChoice[] {
  const { listed, snapshots, excluded, also = [] } = options;
  const latest = new Map<string, string>();
  for (const snapshot of snapshots) {
    if (snapshot.kind !== 'volume') continue;
    const seen = latest.get(snapshot.target);
    if (!seen || snapshot.time > seen) {
      latest.set(snapshot.target, snapshot.time);
    }
  }
  const existing = new Set(
    (listed ?? []).filter((volume) => volume.exists).map(({ name }) => name),
  );
  const names = new Set([
    ...(listed ? listed.map(({ name }) => name) : latest.keys()),
    ...excluded,
    ...also,
  ]);
  return [...names].sort().map((name) => ({
    name,
    included: !excluded.includes(name),
    latest: latest.get(name) ?? null,
    missing: listed !== null && !existing.has(name),
  }));
}

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
