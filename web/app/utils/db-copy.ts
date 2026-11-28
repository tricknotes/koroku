// Copying a database into a staging app (ARCHITECTURE 10.6): the presets
// for the dump and load hooks, the settings form, the choice of the source
// and the steps of a copy log.

import { tracked } from '@glimmer/tracking';
import { parseHeadline, type DeployStep } from './deploy-steps';
import type { App, AppPatch, DbCopy } from './types';

/** Ready-made hooks for the common databases. */
export interface HookPreset {
  id: string;
  label: string;
  service: string;
  dump: string;
  load: string;
}

export const HOOK_PRESETS: readonly HookPreset[] = [
  {
    id: 'postgresql',
    label: 'PostgreSQL',
    service: 'db',
    dump: 'pg_dump -Fc -U "$POSTGRES_USER" "$POSTGRES_DB"',
    load: 'pg_restore --clean --if-exists --no-owner -U "$POSTGRES_USER" -d "$POSTGRES_DB"',
  },
  {
    id: 'mysql',
    label: 'MySQL',
    service: 'db',
    dump: 'mysqldump --single-transaction --routines --triggers -u root -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"',
    load: 'mysql -u root -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"',
  },
  {
    id: 'mariadb',
    label: 'MariaDB',
    service: 'db',
    dump: 'mariadb-dump --single-transaction --routines --triggers -u root -p"$MARIADB_ROOT_PASSWORD" "$MARIADB_DATABASE"',
    load: 'mariadb -u root -p"$MARIADB_ROOT_PASSWORD" "$MARIADB_DATABASE"',
  },
  {
    id: 'sqlite',
    label: 'SQLite',
    service: 'web',
    dump: 'sqlite3 /data/app.sqlite3 .dump',
    load: 'set -e; rm -f /data/app.sqlite3.load; sqlite3 /data/app.sqlite3.load; mv /data/app.sqlite3.load /data/app.sqlite3',
  },
];

export function findPreset(id: string): HookPreset | undefined {
  return HOOK_PRESETS.find((preset) => preset.id === id);
}

/**
 * Editable copy of a staging app's copy settings: the load hook and the
 * command run after a copy. `patch` returns only what changed.
 */
export class CopySettingsForm {
  @tracked loadHookService: string;
  @tracked loadHookCommand: string;
  @tracked postLoadService: string;
  @tracked postLoadCommand: string;

  /** Whether the backend knows database copies at all. */
  readonly available: boolean;

  constructor(app: App) {
    this.available = app.load_hook_service !== undefined;
    this.loadHookService = app.load_hook_service ?? '';
    this.loadHookCommand = app.load_hook_command ?? '';
    this.postLoadService = app.post_load_service ?? '';
    this.postLoadCommand = app.post_load_command ?? '';
  }

  applyPreset(preset: HookPreset): void {
    this.loadHookService = preset.service;
    this.loadHookCommand = preset.load;
  }

  /** Field name (as in the API) → problem found before sending. */
  validate(): Record<string, string> {
    return {
      ...pairErrors(
        'load_hook',
        this.loadHookService.trim(),
        this.loadHookCommand.trim(),
      ),
      ...pairErrors(
        'post_load',
        this.postLoadService.trim(),
        this.postLoadCommand.trim(),
      ),
    };
  }

  patch(app: App): AppPatch {
    const patch: AppPatch = {};
    if (!this.available) return patch;
    const fields = [
      ['load_hook_service', this.loadHookService],
      ['load_hook_command', this.loadHookCommand],
      ['post_load_service', this.postLoadService],
      ['post_load_command', this.postLoadCommand],
    ] as const;
    for (const [name, value] of fields) {
      const trimmed = value.trim();
      if (trimmed !== (app[name] ?? '')) patch[name] = trimmed;
    }
    return patch;
  }
}

/** A service and command pair: both set or both empty. */
export function pairErrors(
  prefix: string,
  service: string,
  command: string,
): Record<string, string> {
  if (command && !service) {
    return {
      [`${prefix}_service`]:
        'コマンドを指定するときはサービスも入力してください。',
    };
  }
  if (service && !command) {
    return {
      [`${prefix}_command`]:
        'サービスを指定するときはコマンドも入力してください。',
    };
  }
  return {};
}

function sameRepo(a: App, b: App): boolean {
  return (
    (a.repo_full_name !== '' && a.repo_full_name === b.repo_full_name) ||
    (a.repo_url !== '' && a.repo_url === b.repo_url)
  );
}

export interface SourceChoice {
  app: App;
  sameRepo: boolean;
  /** Why a live copy from it is not possible right now; '' when it is. */
  liveProblem: string;
}

/**
 * The apps a staging app can copy from: every other app. Production apps of
 * the same repository come first, as they are what one almost always wants.
 */
export function sourceChoices(apps: App[], target: App): SourceChoice[] {
  const rank = (choice: SourceChoice) =>
    (choice.sameRepo ? 0 : 2) +
    (choice.app.environment === 'production' ? 0 : 1);
  return apps
    .filter((app) => app.slug !== target.slug)
    .map((app) => ({
      app,
      sameRepo: sameRepo(app, target),
      liveProblem: liveProblem(app),
    }))
    .sort((a, b) => rank(a) - rank(b) || a.app.slug.localeCompare(b.app.slug));
}

export function liveProblem(source: App): string {
  if (!source.dump_hook_service || !source.dump_hook_command) {
    return `${source.slug} にダンプフックがありません。${source.slug} のバックアップタブで設定してください。`;
  }
  if (!source.current_deployment || source.desired_state !== 'running') {
    return `${source.slug} は稼働していません。`;
  }
  return '';
}

/** What the target needs before anything can be copied into it. */
export function targetProblem(target: App): string {
  if (target.environment !== 'staging') {
    return '本番アプリへはコピーできません。';
  }
  if (!target.load_hook_service || !target.load_hook_command) {
    return 'ロードフックが設定されていません。下の「コピーの設定」で設定してください。';
  }
  if (!target.current_deployment) {
    return 'このアプリはまだデプロイされていません。';
  }
  if (target.desired_state !== 'running') {
    return 'このアプリは停止中です。起動してからコピーしてください。';
  }
  return '';
}

/**
 * What keeps an app from restoring one of its own dumps (ARCHITECTURE
 * 10.8); '' when nothing does. The dump hook takes the current data first,
 * the load hook reads the dump, and both run in the app's containers.
 */
export function restoreProblem(app: App): string {
  if (!app.dump_hook_service || !app.dump_hook_command) {
    return 'ダンプフックが設定されていません。戻す前のいまのデータをダンプフックで取るので、上の「バックアップの設定」で設定してください。';
  }
  if (!app.load_hook_service || !app.load_hook_command) {
    return 'ロードフック（標準入力からダンプを読むコマンド）が設定されていません。';
  }
  if (!app.current_deployment) {
    return 'このアプリはまだデプロイされていません。';
  }
  if (app.desired_state !== 'running') {
    return 'このアプリは停止中です。起動してから戻してください。';
  }
  return '';
}

/** Whether the dump snapshot can be restored into the app right now. */
export function canRestoreDump(
  app: App,
  snapshot: { kind: string; target: string },
): boolean {
  return (
    snapshot.kind === 'dump' &&
    snapshot.target === app.dump_hook_service &&
    restoreProblem(app) === ''
  );
}

/** One line of the preview of a copy. */
export interface PlanStep {
  title: string;
  detail: string;
  /** A command shown in monospace. */
  command?: string;
}

/** What a copy is about to do, step by step, in the reader's words. */
export function copyPlan(options: {
  source: App;
  target: App;
  kind: 'live' | 'snapshot';
  backupsConfigured: boolean;
}): PlanStep[] {
  const { source, target, kind, backupsConfigured } = options;
  const steps: PlanStep[] = [];
  if (!backupsConfigured) {
    steps.push({
      title: 'バックアップ',
      detail: `バックアップが設定されていないため、${target.slug} のいまのデータは保存しません。`,
    });
  } else if (!target.dump_hook_service || !target.dump_hook_command) {
    steps.push({
      title: 'バックアップ',
      detail: `${target.slug} にダンプフックがないため、いまのデータは保存しません。`,
    });
  } else {
    steps.push({
      title: 'バックアップ',
      detail: `${target.slug} のいまのデータをダンプフックで保存します。`,
      command: target.dump_hook_command,
    });
  }
  steps.push({
    title: '停止',
    detail: `${target.slug} の ${target.load_hook_service ?? ''} 以外のサービスを止めます。`,
  });
  steps.push(
    kind === 'live'
      ? {
          title: 'ダンプ',
          detail: `稼働中の ${source.slug} の ${source.dump_hook_service ?? ''} でダンプを取ります。`,
          command: source.dump_hook_command ?? '',
        }
      : {
          title: 'ダンプ',
          detail: `${source.slug} の最新のバックアップのダンプを読みます。${source.slug} には負荷をかけません。`,
        },
  );
  steps.push({
    title: 'ロード',
    detail: `${target.slug} の ${target.load_hook_service ?? ''} へ流し込みます。`,
    command: target.load_hook_command ?? '',
  });
  if (target.post_load_command) {
    steps.push({
      title: 'コピー後',
      detail: `公開する前に ${target.post_load_service ?? ''} で実行します。失敗したら、止めたサービスは止めたままにします。`,
      command: target.post_load_command,
    });
  }
  steps.push({ title: '起動', detail: '止めたサービスを起動します。' });
  return steps;
}

const STEP_LABELS: Record<string, string> = {
  check: '確認',
  backup: 'コピー前のバックアップ',
  stop: '停止',
  copy: 'コピー',
  after: 'コピー後のコマンド',
  start: '起動',
  done: '完了',
};

/** The steps of a restore of the app's own dump (ARCHITECTURE 10.8). */
const RESTORE_STEP_LABELS: Record<string, string> = {
  ...STEP_LABELS,
  backup: '戻す前のバックアップ',
  load: 'ダンプの流し込み',
  reload: '元のデータの流し込み',
};

export function copyStepLabel(key: string): string {
  return STEP_LABELS[key] ?? key;
}

export function restoreStepLabel(key: string): string {
  return RESTORE_STEP_LABELS[key] ?? key;
}

export function isRestore(copy: Pick<DbCopy, 'kind'>): boolean {
  return copy.kind === 'restore';
}

export interface ParsedCopyLog {
  steps: DeployStep[];
  /** Title of the `failed` headline (the reason), or null. */
  failure: string | null;
  /** The services were left stopped on purpose after a failure. */
  leftStopped: boolean;
}

const LEFT_STOPPED = /stay stopped/;
const RELOAD_FAILED = /^Loading the data back failed/;

/**
 * Splits a copy log into its steps (docs/API.md): like a deploy log, a step
 * runs from its headline to the next one.
 */
export function parseCopyLog(
  lines: readonly string[],
  copy: Pick<DbCopy, 'status' | 'finished_at'>,
): ParsedCopyLog {
  const steps: DeployStep[] = [];
  let current: DeployStep | null = null;
  let failure: string | null = null;
  let leftStopped = false;
  for (const line of lines) {
    const headline = parseHeadline(line);
    if (!headline) {
      if (LEFT_STOPPED.test(line)) leftStopped = true;
      current?.lines.push(line);
      continue;
    }
    if (current && current.endedAt === null) current.endedAt = headline.time;
    if (headline.key === 'failed') {
      failure = headline.title.replace(/^(Copy|Restore) failed: /, '');
      if (current?.key === 'reload') {
        // A restore whose load failed loads the data from before it back
        // (ARCHITECTURE 10.8): the load is what failed, and the reload
        // only when it says so.
        const load = steps.find((step) => step.key === 'load');
        if (load) load.state = 'failed';
        if (current.lines.some((entry) => RELOAD_FAILED.test(entry))) {
          current.state = 'failed';
        }
      } else if (current) {
        current.state = 'failed';
      }
      // What follows (starting the services again) belongs to the step
      // that failed.
      continue;
    }
    current = {
      id: `${headline.key}-${steps.length}`,
      key: headline.key,
      title: headline.title,
      startedAt: headline.time,
      endedAt: null,
      lines: [],
      state: 'done',
    };
    steps.push(current);
  }
  const last = steps[steps.length - 1];
  // Without a failed headline the log stopped short: the last step is the
  // one that ran or broke off. With one, the steps are marked above.
  if (
    last &&
    last.state === 'done' &&
    last.key !== 'done' &&
    failure === null
  ) {
    if (copy.status === 'running') last.state = 'running';
    else if (copy.status === 'failed') last.state = 'failed';
  }
  if (last && last.endedAt === null && last.state !== 'running') {
    last.endedAt = last.key === 'done' ? null : copy.finished_at;
  }
  return { steps, failure, leftStopped };
}
