import { formatBytes } from 'koroku-console/utils/format';
import type {
  MemoryReservationSummary,
  SystemInfo,
} from 'koroku-console/utils/types';

/** The summary of production memory reservations as the system screen shows it. */
export interface ReservationView {
  total: string;
  host: string | null;
  margin: string;
  /** Share of host memory reserved; 0 when host memory is unknown. */
  percent: number;
  /** Where the margin starts on the meter (host minus margin). */
  mark: number | undefined;
  /** Reservations plus the margin exceed the host's memory. */
  warning: boolean;
  /** How much the reservations may grow before the warning; '' when over. */
  room: string;
  /** The total after every app is deployed, when it differs; or ''. */
  next: string;
  apps: {
    slug: string;
    /** The app was switched to staging after the release that reserves. */
    switched: boolean;
    total: string;
    breakdown: string;
    /**
     * What the next deployment reserves, when the saved settings change
     * it: `300.0 MB（db 200.0 MB、web 100.0 MB）`; or ''.
     */
    next: string;
  }[];
}

function breakdownOf(services: Record<string, number>): string {
  return Object.entries(services)
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([service, bytes]) => `${service} ${formatBytes(bytes)}`)
    .join('、');
}

function sameServices(
  a: Record<string, number>,
  b: Record<string, number>,
): boolean {
  const keys = Object.keys(a);
  return (
    keys.length === Object.keys(b).length && keys.every((k) => a[k] === b[k])
  );
}

/** The next reservation of an app, when its saved settings change it. */
function nextOf(app: MemoryReservationSummary['apps'][number]): string {
  const next = app.next_services;
  if (!next || sameServices(next, app.services)) return '';
  const total = app.next_total_bytes ?? 0;
  if (total === 0) return 'なし';
  return `${formatBytes(total)}（${breakdownOf(next)}）`;
}

/**
 * Reads `memory_reservation` of `GET /api/system` (ARCHITECTURE 6.2);
 * `null` when the backend does not report it (members, older backends).
 */
export function reservationView(system: SystemInfo): ReservationView | null {
  const summary: MemoryReservationSummary | null | undefined =
    system.memory_reservation;
  if (!summary) return null;
  const host = system.memory?.total_bytes ?? 0;
  const known = host > 0;
  const limit = host - summary.margin_bytes;
  return {
    total: formatBytes(summary.total_bytes),
    host: known ? formatBytes(host) : null,
    margin: formatBytes(summary.margin_bytes),
    percent: known ? Math.min(100, (summary.total_bytes / host) * 100) : 0,
    mark: known && limit > 0 ? (limit / host) * 100 : undefined,
    warning: summary.warning,
    room:
      known && limit > summary.total_bytes
        ? formatBytes(limit - summary.total_bytes)
        : '',
    next:
      typeof summary.next_total_bytes === 'number' &&
      summary.next_total_bytes !== summary.total_bytes
        ? formatBytes(summary.next_total_bytes)
        : '',
    apps: summary.apps.map((app) => ({
      slug: app.slug,
      switched: app.environment !== 'production',
      total: app.total_bytes > 0 ? formatBytes(app.total_bytes) : 'なし',
      breakdown: breakdownOf(app.services),
      next: app.environment === 'production' ? nextOf(app) : '',
    })),
  };
}
