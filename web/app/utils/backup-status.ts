import type { StatusKind } from './status';
import type { BackupOverview, BackupRun } from './types';

/** How the host's backups are doing, in the vocabulary of the five states. */
export interface BackupHealth {
  kind: StatusKind;
  label: string;
  /** Nothing to look at: the indicator stays in the background. */
  quiet: boolean;
  /** The most recent finished run, when it failed. */
  failedRun: BackupRun | null;
  /** No success for 24 hours (or none ever). */
  stale: boolean;
}

/** The newest run that has ended; `runs` are newest first. */
export function latestFinishedRun(overview: BackupOverview): BackupRun | null {
  return (overview.runs ?? []).find((run) => run.status !== 'running') ?? null;
}

/**
 * One state for the backups of the host: running wins, then a failed latest
 * run, then "no success for a day"; otherwise all is well and quiet.
 */
export function backupHealth(overview: BackupOverview): BackupHealth {
  if (!overview.configured) {
    return {
      kind: 'stopped',
      label: '未設定',
      quiet: true,
      failedRun: null,
      stale: false,
    };
  }
  const latest = latestFinishedRun(overview);
  const failedRun = latest?.status === 'failed' ? latest : null;
  const stale = overview.warning;
  if (overview.running) {
    return {
      kind: 'deploying',
      label: '実行中',
      quiet: false,
      failedRun,
      stale,
    };
  }
  if (failedRun) {
    return {
      kind: 'failed',
      label: '直近の実行が失敗',
      quiet: false,
      failedRun,
      stale,
    };
  }
  if (stale) {
    return {
      kind: 'pending',
      label: overview.last_success_at
        ? '24 時間以上成功なし'
        : 'まだ成功していません',
      quiet: false,
      failedRun,
      stale,
    };
  }
  return { kind: 'running', label: '正常', quiet: true, failedRun, stale };
}
