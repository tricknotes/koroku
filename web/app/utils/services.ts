// How one Compose service is presented on the overview tab: its state in
// the shared vocabulary, its health check and its memory use.

import { formatBytes } from './format';
import type { StatusKind } from './status';
import type { ServiceStatus, WebColor } from './types';

export interface ServiceState {
  kind: StatusKind;
  label: string;
  /** Needs attention: shown in colour instead of quietly. */
  problem: boolean;
}

/** `Exited (1) 3 minutes ago` in the status text of `docker compose ps`. */
const EXIT_CODE = /^Exited \((\d+)\)/;

/**
 * State of a service from the `docker compose ps` fields. A container that
 * ended with code 0 is a finished one-shot job (a migration), not a failure.
 */
export function serviceState(
  service: Pick<ServiceStatus, 'state' | 'status' | 'health'>,
): ServiceState {
  switch (service.state) {
    case 'running':
      if (service.health === 'unhealthy') {
        return { kind: 'failed', label: '異常', problem: true };
      }
      if (service.health === 'starting') {
        return { kind: 'deploying', label: '起動中', problem: false };
      }
      return { kind: 'running', label: '稼働中', problem: false };
    case 'restarting':
      return { kind: 'pending', label: '再起動中', problem: true };
    case 'exited': {
      const code = EXIT_CODE.exec(service.status)?.[1];
      return code === undefined || code === '0'
        ? { kind: 'stopped', label: '終了', problem: false }
        : { kind: 'failed', label: '異常終了', problem: true };
    }
    case 'dead':
      return { kind: 'failed', label: '異常終了', problem: true };
    case 'created':
      return { kind: 'stopped', label: '未起動', problem: false };
    case 'paused':
      return { kind: 'stopped', label: '一時停止', problem: false };
    default:
      return {
        kind: 'stopped',
        label: service.state || '不明',
        problem: false,
      };
  }
}

const HEALTH_LABELS: Record<string, string> = {
  healthy: '正常',
  unhealthy: '異常',
  starting: '確認中',
};

/** Result of the container's health check; empty when it has none. */
export function healthLabel(health: string): string {
  return HEALTH_LABELS[health] ?? health;
}

/** From this share of its limit on, a service is close to being killed. */
export const MEMORY_HIGH_PERCENT = 90;

export interface MemoryUsage {
  /** `310.0 MB / 512.0 MB`, or `310.0 MB / 上限なし`. */
  text: string;
  /** Share of the limit in use; `null` without a limit. */
  percent: number | null;
  high: boolean;
}

/** Memory use of a service; `null` when the server could not measure it. */
export function memoryUsage(
  service: Pick<ServiceStatus, 'memory_bytes' | 'memory_limit_bytes'>,
): MemoryUsage | null {
  const used = service.memory_bytes;
  if (typeof used !== 'number') return null;
  const limit = service.memory_limit_bytes;
  if (typeof limit !== 'number' || limit <= 0) {
    return {
      text: `${formatBytes(used)} / 上限なし`,
      percent: null,
      high: false,
    };
  }
  const percent = Math.round((used / limit) * 100);
  return {
    text: `${formatBytes(used)} / ${formatBytes(limit)}`,
    percent,
    high: percent >= MEMORY_HIGH_PERCENT,
  };
}

/** A row of the service lists, with what a blue/green copy adds. */
export interface ServiceRow {
  /** Unique within the list: the name, plus the colour of a copy. */
  key: string;
  service: ServiceStatus;
  /** The colour of a copy of a blue/green app's public service. */
  color: WebColor | null;
  /**
   * Both copies run (during a deployment, ARCHITECTURE 7.2) and this one
   * receives the requests.
   */
  live: boolean;
  /** Both copies run and this one does not receive the requests. */
  standby: boolean;
}

/**
 * The service rows of `GET /api/apps/{slug}/services`. A blue/green app's
 * public service has a row per running copy, each with its colour;
 * `webColor` (the app's) tells which one receives the requests.
 */
export function serviceRows(
  services: readonly ServiceStatus[],
  webColor: WebColor | null | undefined,
): ServiceRow[] {
  const copies = new Map<string, number>();
  for (const service of services) {
    if (service.color) {
      copies.set(service.name, (copies.get(service.name) ?? 0) + 1);
    }
  }
  return services.map((service) => {
    const color = service.color ?? null;
    const paired = color !== null && (copies.get(service.name) ?? 0) > 1;
    const live = paired && color === webColor;
    return {
      key: color ? `${service.name}:${color}` : service.name,
      service,
      color,
      live,
      standby: paired && !live,
    };
  });
}
