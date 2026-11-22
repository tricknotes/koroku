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
  apps: {
    slug: string;
    /** The app was switched to staging after the release that reserves. */
    switched: boolean;
    total: string;
    breakdown: string;
  }[];
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
    apps: summary.apps.map((app) => ({
      slug: app.slug,
      switched: app.environment !== 'production',
      total: formatBytes(app.total_bytes),
      breakdown: Object.entries(app.services)
        .sort(([a], [b]) => a.localeCompare(b))
        .map(([service, bytes]) => `${service} ${formatBytes(bytes)}`)
        .join('、'),
    })),
  };
}
