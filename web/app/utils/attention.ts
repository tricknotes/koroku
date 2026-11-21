import type { App, BackupOverview, Deployment, SystemInfo } from './types';

/** Used when the backend does not report its limit (ARCHITECTURE 8.2). */
export const DEFAULT_DISK_LIMIT_PERCENT = 85;

/** One thing on the host or in an app that the owner should act on. */
export type AttentionItem =
  | { kind: 'disk'; usedPercent: number; limitPercent: number }
  | { kind: 'backup'; lastSuccessAt: string | null }
  | { kind: 'backup-app'; slug: string }
  | { kind: 'backup-run' }
  | { kind: 'backup-system' }
  | { kind: 'deployment'; slug: string; deployment: Deployment }
  | { kind: 'domains'; slug: string; count: number };

export function diskLimitPercent(system: SystemInfo): number {
  return system.disk_limit_percent ?? DEFAULT_DISK_LIMIT_PERCENT;
}

/** Whether deployments are being refused because the disk is too full. */
export function diskOverLimit(system: SystemInfo): boolean {
  return (
    system.disk !== null && system.disk.used_percent > diskLimitPercent(system)
  );
}

/**
 * Everything that needs attention, host first: the disk above the limit at
 * which deployments are refused, failed backups (the run, the system, then
 * each app), no successful backup for 24 hours, apps
 * whose latest deployment failed and apps with domains awaiting DNS
 * verification. Empty when all is well; data that could not be loaded
 * (`null`) contributes nothing.
 */
export function attentionItems(
  apps: App[] | null,
  system: SystemInfo | null,
  backups: BackupOverview | null,
): AttentionItem[] {
  const items: AttentionItem[] = [];

  if (system?.disk && diskOverLimit(system)) {
    items.push({
      kind: 'disk',
      usedPercent: system.disk.used_percent,
      limitPercent: diskLimitPercent(system),
    });
  }
  // A failed backup comes first: until it is fixed, data can be lost.
  // A run that failed before backing anything up is one line, not one per
  // app; stored-with-warnings is left to the app's tab.
  if (backups?.configured) {
    const problems = (backups.problems ?? []).filter(
      (p) => p.status === 'failed',
    );
    if (problems.some((p) => p.whole_run)) {
      items.push({ kind: 'backup-run' });
    }
    if (backups.system_failed && !problems.some((p) => p.whole_run)) {
      items.push({ kind: 'backup-system' });
    }
    for (const problem of problems) {
      if (!problem.whole_run) {
        items.push({ kind: 'backup-app', slug: problem.app });
      }
    }
  }
  if (backups?.configured && backups.warning) {
    items.push({ kind: 'backup', lastSuccessAt: backups.last_success_at });
  }
  for (const app of apps ?? []) {
    if (app.latest_deployment?.status === 'failed') {
      items.push({
        kind: 'deployment',
        slug: app.slug,
        deployment: app.latest_deployment,
      });
    }
  }
  for (const app of apps ?? []) {
    const count = app.pending_domains ?? 0;
    if (count > 0) items.push({ kind: 'domains', slug: app.slug, count });
  }
  return items;
}

/** Failed backups: the banner turns from a warning into an error. */
export function hasBackupFailure(items: AttentionItem[]): boolean {
  return items.some(
    (item) =>
      item.kind === 'backup-app' ||
      item.kind === 'backup-run' ||
      item.kind === 'backup-system',
  );
}
