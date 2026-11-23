// One service's memory reservation on the resources tab (ARCHITECTURE 9.5):
// what the running release reserves, what the saved settings give the next
// deployment, and a suggestion from the memory history.

import { isValidReservation } from './settings-form';
import type { App, MemoryServiceHistory, ServiceStatus } from './types';

const MiB = 1024 * 1024;

/** The smallest reservation Docker accepts; the API refuses less. */
export const MIN_RESERVATION = 6 * MiB;

/** Suggestions are rounded up to this, like the limit suggestion. */
const SUGGESTION_STEP = 64 * MiB;

/**
 * Bytes of a memory setting (`512m`, `1g`, `1048576`), or null. Mirrors
 * `compose.ParseMemory`.
 */
export function parseMemory(value: string | null | undefined): number | null {
  let text = (value ?? '').trim().toLowerCase();
  if (text.endsWith('b')) text = text.slice(0, -1);
  if (text === '') return null;
  let multiplier = 1;
  const unit = text[text.length - 1];
  if (unit === 'k') multiplier = 1024;
  else if (unit === 'm') multiplier = MiB;
  else if (unit === 'g') multiplier = 1024 * MiB;
  if (multiplier !== 1) text = text.slice(0, -1);
  if (!/^[0-9]+$/.test(text)) return null;
  const n = Number.parseInt(text, 10);
  return n > 0 ? n * multiplier : null;
}

/** Where the reservation of the next deployment comes from. */
export type ReservationSource = 'service' | 'app' | 'default';

export interface ReservationPlan {
  source: ReservationSource;
  /** The setting that applies: `256m`, `0` (none), or '' for half the limit. */
  setting: string;
  /** The limit the next deployment is expected to run with; null if unknown. */
  limitBytes: number | null;
  /**
   * What the next deployment reserves (0 for none), or null when it cannot
   * be told: half of a limit that is not known.
   */
  nextBytes: number | null;
  /** What the running release reserves (0 for none). */
  runningBytes: number;
  /** The next deployment reserves something else than the running release. */
  pending: boolean;
  /**
   * A finished service that gets no reservation whatever the settings say:
   * a one-shot service such as a migration (ARCHITECTURE 6.2).
   */
  oneShot: boolean;
}

/** The limit the app settings give the service, or what it runs with now. */
function limitOf(app: App, service: ServiceStatus): number | null {
  const set = parseMemory(
    app.service_memory_limits?.[service.name] || app.memory_limit,
  );
  if (set !== null) return set;
  const running = service.memory_limit_bytes;
  return typeof running === 'number' && running > 0 ? running : null;
}

/**
 * The service's reservation now and from the next deployment, by the
 * precedence of ARCHITECTURE 6.2: the per-service setting, the app-wide
 * one, half the limit. A staging app reserves nothing.
 */
export function reservationPlan(
  app: App,
  service: ServiceStatus,
): ReservationPlan {
  const running = service.memory_reservation_bytes;
  const runningBytes = typeof running === 'number' && running > 0 ? running : 0;
  const own = app.service_memory_reservations?.[service.name];
  const source: ReservationSource = own
    ? 'service'
    : app.memory_reservation
      ? 'app'
      : 'default';
  const setting = own || app.memory_reservation || '';
  const limitBytes = limitOf(app, service);

  const fromServer = service.next_memory_reservation_bytes;
  let nextBytes: number | null;
  if (typeof fromServer === 'number') {
    // The running release's shapes with the saved settings (6.2).
    nextBytes = fromServer;
  } else if (service.state !== 'running') {
    // Without the shapes a finished one-shot service cannot be told from
    // one that should reserve: estimate nothing.
    nextBytes = null;
  } else if (app.environment !== 'production' || setting === '0') {
    nextBytes = 0;
  } else if (setting === '') {
    nextBytes =
      limitBytes === null ? null : Math.floor(limitBytes / 2 / MiB) * MiB;
  } else {
    nextBytes = parseMemory(setting);
    if (nextBytes !== null && limitBytes !== null) {
      nextBytes = Math.min(nextBytes, limitBytes);
    }
  }
  if (nextBytes !== null && nextBytes < MIN_RESERVATION) nextBytes = 0;
  const oneShot =
    app.environment === 'production' &&
    fromServer === 0 &&
    setting !== '0' &&
    service.state === 'exited';

  return {
    oneShot,
    source,
    setting,
    limitBytes,
    nextBytes,
    runningBytes,
    pending: nextBytes !== null && nextBytes !== runningBytes,
  };
}

/** `サービス別 256m`, `全体の値 300m`, `既定（上限の半分）`. */
export function sourceText(plan: ReservationPlan): string {
  if (plan.oneShot) return '終わって止まるサービスには付きません';
  const value = plan.setting === '0' ? '0（予約しない）' : plan.setting;
  switch (plan.source) {
    case 'service':
      return `サービス別の値 ${value}`;
    case 'app':
      return `全体の値 ${value}`;
    default:
      return '既定（上限の半分）';
  }
}

/** What the service falls back to without its own value. */
export function fallbackText(app: App): string {
  const all = app.memory_reservation;
  if (all === '0') return '全体の値 0（予約しない）';
  if (all) return `全体の値 ${all}`;
  return '上限の半分';
}

/**
 * A reservation from the memory history: the 95th percentile of the
 * 5-minute maxima under the current limit, rounded up to 64 MiB and cut to
 * the limit (ARCHITECTURE 9.5). Null until the limit suggestion is ready,
 * or when it would be below what Docker accepts.
 */
export function suggestedReservation(
  history: MemoryServiceHistory | undefined,
  limitBytes: number | null,
): string | null {
  if (!history?.suggestion.ready) return null;
  let bytes =
    Math.ceil(history.suggestion.p95_bytes / SUGGESTION_STEP) * SUGGESTION_STEP;
  const limit = limitBytes ?? history.limit_bytes;
  if (limit) bytes = Math.min(bytes, Math.floor(limit / MiB) * MiB);
  if (bytes < MIN_RESERVATION) return null;
  return `${bytes / MiB}m`;
}

/** Why a value cannot be saved as a service's reservation, or null. */
export function reservationProblem(value: string): string | null {
  if (value === '')
    return '値を入力してください。既定に戻すときは「既定に戻す」を押してください。';
  if (!isValidReservation(value)) {
    return '0（予約しない）か、数字と単位 b / k / m / g で入力してください（例 256m）。';
  }
  return null;
}

/**
 * `service_memory_reservations` with one service set to `value`, or
 * removed with null; the other services keep their entries.
 */
export function withReservation(
  app: App,
  service: string,
  value: string | null,
): Record<string, string> {
  const next = { ...(app.service_memory_reservations ?? {}) };
  if (value === null) delete next[service];
  else next[service] = value;
  return next;
}
