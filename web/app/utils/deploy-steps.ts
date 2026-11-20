// Turns a deployment log into the steps the deploy tab shows.
//
// koroku keeps no table of steps: the log file is the only record. A step
// starts at a headline `==> <RFC 3339 time> <key> <title>` (docs/API.md) and
// lasts until the next one. What happened after a failure (the automatic
// rollback) is read from the same log; the phrases matched below are written
// by internal/deploy/engine.go and steps.go.

import { isRollback } from './rollback';
import type { Deployment } from './types';

export type StepState = 'done' | 'running' | 'failed' | 'canceled' | 'pending';

export interface DeployStep {
  /** Unique within one log: the key, or `key-2` for a repeated key. */
  id: string;
  key: string;
  /** The title koroku gave the step (English), e.g. "Building images". */
  title: string;
  startedAt: string | null;
  /** When the next step began; `null` while running or when unknown. */
  endedAt: string | null;
  lines: string[];
  state: StepState;
}

export type RollbackKind =
  /** The previous release was started again and is serving. */
  | 'restored'
  /** The rollback has begun and has not reported its result yet. */
  | 'restoring'
  /** The previous release could not be brought back. */
  | 'failed'
  /** A first deployment: there was nothing to go back to. */
  | 'removed'
  /** It failed before the running containers were touched. */
  | 'untouched'
  /** koroku was shutting down and left the containers as they were. */
  | 'skipped';

export interface RollbackOutcome {
  kind: RollbackKind;
  /** The deployment that was (to be) restored, when the log names it. */
  deploymentId: number | null;
  /** The log line that explains a rollback that did not work. */
  detail: string;
}

export interface DeployFailure {
  /** The step that was running when the deployment stopped. */
  step: DeployStep | null;
  /** Title of the `failed` headline: the final status and the reason. */
  title: string;
}

export interface ParsedDeployLog {
  steps: DeployStep[];
  failure: DeployFailure | null;
  rollback: RollbackOutcome | null;
}

type DeploymentTimes = Pick<
  Deployment,
  'status' | 'queued_at' | 'started_at' | 'finished_at'
> &
  Pick<Deployment, 'source_deployment_id'>;

/** Lines written before the first step: queueing, an early cancel. */
export const QUEUE_KEY = 'queue';

const STEP_LABELS: Record<string, string> = {
  [QUEUE_KEY]: '受付',
  fetch: '取得',
  prepare: '生成',
  validate: '検証',
  reuse: 'イメージの再利用',
  build: 'ビルド',
  pull: 'イメージの pull',
  backup: 'デプロイ前のバックアップ',
  start: '起動',
  route: '経路',
  check: '確認',
  done: '完了',
  rollback: 'ロールバック',
  cleanup: '後片付け',
};

/**
 * Where a rollback's steps mean something else: its `prepare` generates
 * nothing, it checks that the kept release is still there (ARCHITECTURE 7.1).
 */
const ROLLBACK_STEP_LABELS: Record<string, string> = {
  prepare: '準備',
};

/**
 * Japanese name of a step; an unknown key is shown as it is. `rollback`
 * names the steps of a rollback deployment.
 */
export function stepLabel(key: string, rollback = false): string {
  return (
    (rollback ? ROLLBACK_STEP_LABELS[key] : undefined) ??
    STEP_LABELS[key] ??
    key
  );
}

/** The order of a deployment that goes well (ARCHITECTURE chapter 7). */
const ORDER = [
  'fetch',
  'prepare',
  'validate',
  'build',
  'pull',
  'backup',
  'start',
  'route',
  'check',
  'done',
];

/**
 * Steps every deployment goes through. The others (build, pull, backup,
 * route) depend on the app and appear once koroku announces them.
 */
const ALWAYS = new Set([
  'fetch',
  'prepare',
  'validate',
  'start',
  'check',
  'done',
]);

/**
 * A rollback starts a kept release again: no fetch, build, pull or backup
 * (ARCHITECTURE 7.1). `route` still depends on the host.
 */
const ROLLBACK_ORDER = [
  'prepare',
  'validate',
  'start',
  'route',
  'check',
  'done',
];

const ROLLBACK_ALWAYS = new Set([
  'prepare',
  'validate',
  'start',
  'check',
  'done',
]);

/** Markers without a duration of their own. */
const INSTANT = new Set(['done']);
/** Steps that run after the deployment was recorded as finished. */
const AFTER_FINISH = new Set(['done', 'cleanup']);

const HEADLINE = /^==> (\S+) ([a-z][a-z0-9_-]*)(?: (.*))?$/;

interface Headline {
  time: string;
  key: string;
  title: string;
}

export function parseHeadline(line: string): Headline | null {
  const match = HEADLINE.exec(line);
  if (!match) return null;
  const [, time = '', key = '', title = ''] = match;
  if (Number.isNaN(Date.parse(time))) return null;
  return { time, key, title };
}

export function isHeadline(line: string): boolean {
  return parseHeadline(line) !== null;
}

const ROLLBACK_TARGET = /^Rolling back to deployment #(\d+)/;
const ROLLBACK_DONE = /^Rolled back: /;
const ROLLBACK_BROKEN =
  /^(Rollback failed|Rollback is not possible|Removing the containers failed)\b/;
const ROLLBACK_NOTHING = /^No previous release/;
const SHUTTING_DOWN = /^koroku is shutting down/;
const CANCELED = /^Deployment canceled\b/;

function isActive(status: Deployment['status']): boolean {
  return status === 'queued' || status === 'running';
}

function notBefore(time: string | null, start: string | null): string | null {
  if (!time || !start) return null;
  return Date.parse(time) >= Date.parse(start) ? time : null;
}

function trimBlankEnd(lines: string[]): void {
  while (lines.length > 0 && lines[lines.length - 1]?.trim() === '') {
    lines.pop();
  }
}

function rollbackOutcome(
  step: DeployStep | undefined,
  afterFailure: string[],
  started: boolean,
  active: boolean,
): RollbackOutcome | null {
  if (!step) {
    if (afterFailure.some((line) => SHUTTING_DOWN.test(line))) {
      return { kind: 'skipped', deploymentId: null, detail: '' };
    }
    // Up to build / pull / backup the running containers are left alone.
    if (!started) return { kind: 'untouched', deploymentId: null, detail: '' };
    // Once `start` ran, the rollback headline is about to follow. If it
    // never came, the log does not say what happened to the containers.
    return active
      ? { kind: 'restoring', deploymentId: null, detail: '' }
      : null;
  }

  const target = ROLLBACK_TARGET.exec(step.title);
  const deploymentId = target ? Number(target[1]) : null;
  const broken = step.lines.find((line) => ROLLBACK_BROKEN.test(line));
  if (broken) return { kind: 'failed', deploymentId, detail: broken };
  if (ROLLBACK_NOTHING.test(step.title)) {
    return {
      kind: active ? 'restoring' : 'removed',
      deploymentId: null,
      detail: '',
    };
  }
  if (step.lines.some((line) => ROLLBACK_DONE.test(line))) {
    return { kind: 'restored', deploymentId, detail: '' };
  }
  // No verdict in the log: still at it, or the log ends there.
  return { kind: active ? 'restoring' : 'failed', deploymentId, detail: '' };
}

/**
 * Splits the log lines of one deployment into steps and works out how it
 * ended. `deployment` supplies what the log cannot know: whether it is still
 * going and when it was queued and finished.
 */
export function parseDeployLog(
  lines: readonly string[],
  deployment: DeploymentTimes,
): ParsedDeployLog {
  const active = isActive(deployment.status);
  const steps: DeployStep[] = [];
  const seen = new Map<string, number>();
  const afterFailure: string[] = [];
  let failureTitle: string | null = null;
  let failedStep: DeployStep | null = null;
  let current: DeployStep | null = null;
  let inFailure = false;

  const open = (
    key: string,
    title: string,
    startedAt: string | null,
  ): DeployStep => {
    const count = (seen.get(key) ?? 0) + 1;
    seen.set(key, count);
    const step: DeployStep = {
      id: count === 1 ? key : `${key}-${count}`,
      key,
      title,
      startedAt,
      endedAt: null,
      lines: [],
      state: 'done',
    };
    steps.push(step);
    return step;
  };

  for (const line of lines) {
    const headline = parseHeadline(line);
    if (!headline) {
      if (inFailure) {
        afterFailure.push(line);
      } else {
        current ??= open(QUEUE_KEY, '', deployment.queued_at);
        current.lines.push(line);
      }
      continue;
    }

    if (current && !inFailure) current.endedAt = headline.time;
    inFailure = false;

    if (headline.key === 'failed') {
      failureTitle = headline.title;
      failedStep = current;
      if (current) {
        current.state = CANCELED.test(headline.title) ? 'canceled' : 'failed';
      }
      inFailure = true;
      continue;
    }
    current = open(headline.key, headline.title, headline.time);
  }

  for (const step of steps) trimBlankEnd(step.lines);

  const last = steps[steps.length - 1];
  const rollbackStep = steps.find((step) => step.key === 'rollback');
  const failure: DeployFailure | null =
    failureTitle === null ? null : { step: failedStep, title: failureTitle };
  const rollback = failure
    ? rollbackOutcome(rollbackStep, afterFailure, seen.has('start'), active)
    : null;

  if (rollbackStep && rollback) {
    if (rollback.kind === 'failed') rollbackStep.state = 'failed';
    if (rollback.kind === 'restoring') rollbackStep.state = 'running';
  }

  if (last && last.state === 'done' && last !== rollbackStep) {
    if (failure) {
      // The step the deployment stopped in is already marked.
    } else if (AFTER_FINISH.has(last.key)) {
      // `done` and what follows it are written after the result is
      // recorded; the status on screen may lag behind for a moment.
    } else if (active) {
      last.state = 'running';
    } else if (deployment.status === 'failed') {
      // No `failed` headline: koroku was restarted in the middle.
      last.state = 'failed';
    } else if (deployment.status === 'canceled') {
      last.state = 'canceled';
    }
  }

  // The last step has no next headline to end it.
  if (last && last.endedAt === null && !active && !INSTANT.has(last.key)) {
    if (!AFTER_FINISH.has(last.key) && last.state !== 'running') {
      last.endedAt = notBefore(deployment.finished_at, last.startedAt);
    }
  }
  for (const step of steps) {
    if (INSTANT.has(step.key)) step.endedAt = null;
  }

  if (active && !failure) {
    const rollback = isRollback(deployment);
    const order = rollback ? ROLLBACK_ORDER : ORDER;
    const always = rollback ? ROLLBACK_ALWAYS : ALWAYS;
    const reached = Math.max(
      -1,
      ...steps.map((step) => order.indexOf(step.key)),
    );
    order.forEach((key, index) => {
      if (index <= reached || !always.has(key)) return;
      steps.push({
        id: key,
        key,
        title: '',
        startedAt: null,
        endedAt: null,
        lines: [],
        state: 'pending',
      });
    });
  }

  return { steps, failure, rollback };
}

// A line that most likely says what went wrong. Only applied to the step
// that failed, where a false positive costs little.
const ERROR_LINE =
  /\b(error|errors|fatal|panic|failed|failure|denied|refused|unhealthy|cannot|can't|unable to|not found|no such|timed out|timeout|violation|exited with code [1-9]\d*|exit code:? [1-9]\d*|returned (4|5)\d\d)\b|-> (4|5)\d\d\b/i;

// Exception class names as most runtimes print them (NameError,
// java.lang.IllegalStateException): "error" inside a word, which the
// pattern above leaves alone on purpose.
const EXCEPTION_NAME = /[A-Za-z](Error|Exception)\b/;

export function isErrorLine(line: string): boolean {
  return ERROR_LINE.test(line) || EXCEPTION_NAME.test(line);
}

export interface LogSegment {
  text: string;
  error: boolean;
}

/**
 * Groups the lines of a step into runs of ordinary and error lines, so a
 * long log renders as a handful of text nodes instead of one per line.
 */
export function logSegments(
  lines: readonly string[],
  highlight: boolean,
): LogSegment[] {
  const segments: LogSegment[] = [];
  let run: string[] = [];
  let runError = false;
  const flush = () => {
    if (run.length === 0) return;
    segments.push({ text: run.join('\n'), error: runError });
    run = [];
  };
  for (const line of lines) {
    const error = highlight && isErrorLine(line);
    if (error !== runError) {
      flush();
      runError = error;
    }
    run.push(line);
  }
  flush();
  return segments;
}
