// The memory limits and reservations of an app, app-wide and per service,
// as the resources tab edits them one value at a time (ARCHITECTURE 9.5).

import { isValidMemoryLimit, isValidReservation } from './settings-form';
import type { App, AppPatch } from './types';

export type MemoryKind = 'limit' | 'reservation';

/** What one dialog of the resources tab changes. */
export interface MemoryTarget {
  kind: MemoryKind;
  /** The service, or null for the app-wide value. */
  service: string | null;
}

/** The field of `PATCH /api/apps/{slug}` a target writes. */
export function memoryField(target: MemoryTarget): keyof AppPatch {
  if (target.kind === 'limit') {
    return target.service === null ? 'memory_limit' : 'service_memory_limits';
  }
  return target.service === null
    ? 'memory_reservation'
    : 'service_memory_reservations';
}

/** The per-service values saved for a kind, sorted by service. */
export function serviceValues(
  app: App,
  kind: MemoryKind,
): { service: string; value: string }[] {
  const values =
    (kind === 'limit'
      ? app.service_memory_limits
      : app.service_memory_reservations) ?? {};
  return Object.entries(values)
    .map(([service, value]) => ({ service, value }))
    .sort((a, b) => a.service.localeCompare(b.service));
}

/** The saved value of a target; '' when it has none. */
export function savedValue(app: App, target: MemoryTarget): string {
  if (target.service === null) {
    return (
      (target.kind === 'limit' ? app.memory_limit : app.memory_reservation) ??
      ''
    );
  }
  const values =
    target.kind === 'limit'
      ? app.service_memory_limits
      : app.service_memory_reservations;
  return values?.[target.service] ?? '';
}

/** `512m`, or what applies without a value of its own. */
export function appWideText(app: App, kind: MemoryKind): string {
  const value = savedValue(app, { kind, service: null });
  if (kind === 'limit') return value || '基盤の既定値';
  if (value === '0') return '0（予約しない）';
  return value || '各サービスの上限の半分';
}

/** `256m`, `0（予約しない）`. */
export function valueText(value: string): string {
  return value === '0' ? '0（予約しない）' : value;
}

/**
 * Why a value cannot be saved, or null. The app-wide value may be empty
 * (back to the default); a service's value may not (it is removed instead).
 */
export function memoryProblem(
  target: MemoryTarget,
  value: string,
): string | null {
  if (target.service !== null && value === '') {
    return '値を入力してください。';
  }
  if (target.kind === 'limit') {
    return isValidMemoryLimit(value)
      ? null
      : '数字と単位 b / k / m / g で入力してください（例 512m）。';
  }
  return isValidReservation(value)
    ? null
    : '0（予約しない）か、数字と単位 b / k / m / g で入力してください（例 256m）。';
}

/**
 * The patch that saves one value (null removes a service's value or
 * empties the app-wide one); the other services keep their entries.
 */
export function memoryPatch(
  app: App,
  target: MemoryTarget,
  value: string | null,
): AppPatch {
  const field = memoryField(target);
  if (target.service === null) return { [field]: value ?? '' };
  const current =
    (target.kind === 'limit'
      ? app.service_memory_limits
      : app.service_memory_reservations) ?? {};
  const next = { ...current };
  if (value === null) delete next[target.service];
  else next[target.service] = value;
  return { [field]: next };
}
