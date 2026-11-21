import type { StatusKind } from './status';
import type {
  AppBackupRun,
  AppBackupStatus,
  BackupOverview,
  BackupRun,
  BackupTargetOutcome,
} from './types';

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

/** How the backups of one app are doing (ARCHITECTURE 10.2). */
export interface AppBackupHealth {
  kind: StatusKind;
  label: string;
  quiet: boolean;
  /** The latest run that concerns the app, when it did not succeed. */
  failedRun: AppBackupRun | null;
  /** Its targets that failed; empty when the whole run failed. */
  failures: BackupTargetOutcome[];
  /** Targets stored although some files could not be read. */
  warnings: BackupTargetOutcome[];
  /** Nothing of the app stored for 24 hours (once it has been backed up). */
  stale: boolean;
}

/**
 * One state for the backups of an app, from its own outcome only: other
 * apps' failures do not show here. A run in progress (the host's) wins,
 * then a failure, then "not stored for a day", then stored with warnings.
 */
export function appBackupHealth(
  overview: BackupOverview | null,
  status: AppBackupStatus,
): AppBackupHealth {
  const run = status.last_run;
  const failures = (run?.targets ?? []).filter((t) => t.status === 'failed');
  const warnings = (run?.targets ?? []).filter((t) => t.status === 'warning');
  const failedRun = run?.status === 'failed' ? run : null;
  const base = { failedRun, failures, warnings, stale: status.warning };
  if (!status.configured) {
    return { ...base, kind: 'stopped', label: '未設定', quiet: true };
  }
  if (overview?.running) {
    return { ...base, kind: 'deploying', label: '実行中', quiet: false };
  }
  if (!run) {
    // Not part of a run yet (a new app): nothing is late.
    return {
      ...base,
      kind: 'stopped',
      label: 'まだ取得していません',
      quiet: true,
    };
  }
  if (failedRun) {
    return { ...base, kind: 'failed', label: '直近の取得が失敗', quiet: false };
  }
  if (status.warning) {
    return {
      ...base,
      kind: 'pending',
      label: status.last_success_at
        ? '24 時間以上成功なし'
        : 'まだ成功していません',
      quiet: false,
    };
  }
  if (warnings.length > 0) {
    return {
      ...base,
      kind: 'pending',
      label: '一部のファイルを読めずに保存',
      quiet: false,
    };
  }
  return { ...base, kind: 'running', label: '正常', quiet: true };
}

const TARGET_KINDS: Record<BackupTargetOutcome['kind'], string> = {
  system: '基盤',
  volume: 'volume',
  dump: 'DB のダンプ',
};

/** `volume uploads` / `DB のダンプ（db）`, for the lines of a banner. */
export function targetLabel(target: BackupTargetOutcome): string {
  const kind = TARGET_KINDS[target.kind];
  if (!target.target) return kind;
  return target.kind === 'dump'
    ? `${kind}（${target.target}）`
    : `${kind} ${target.target}`;
}
