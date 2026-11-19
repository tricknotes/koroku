// How the memory and CPU of running containers are presented on the system
// screen: one row per app, plus one for the containers of no app.

import { formatBytes, formatPercent } from './format';
import { MEMORY_HIGH_PERCENT } from './services';
import type {
  ContainerGroup,
  ContainerResources,
  ContainerUsage,
} from './types';

export interface ContainerGroupRow {
  key: string;
  /** The app's slug; `null` for the containers of no app. */
  app: string | null;
  label: string;
  memory: string;
  /** Share of the host's memory, 0 to 100. */
  memoryPercent: number;
  /** `上限 1.0 GB（41%）`, or `上限なし`. */
  limit: string;
  /**
   * The containers close to their own memory limit (the kernel kills a
   * container that reaches it), by label; empty when none is.
   */
  nearLimit: string;
  cpu: string;
  /** Share of the whole host's CPU, 0 to 100. */
  cpuPercent: number;
  /** What each container uses: `web 310.0 MB、db 420.0 MB`. */
  breakdown: string;
}

export const OTHER_LABEL = 'その他';

function share(used: number, total: number): number {
  return total > 0 ? (used / total) * 100 : 0;
}

/** A container is named by its service within an app, by its name elsewhere. */
function containerLabel(group: ContainerGroup, container: ContainerUsage) {
  return group.app !== null && container.service
    ? container.service
    : container.name;
}

function nearItsLimit(container: ContainerUsage): boolean {
  const limit = container.memory_limit_bytes;
  return (
    typeof limit === 'number' &&
    limit > 0 &&
    share(container.memory_bytes, limit) >= MEMORY_HIGH_PERCENT
  );
}

export function containerGroupRows(
  resources: ContainerResources,
): ContainerGroupRow[] {
  return resources.groups.map((group) => {
    const limit = group.memory_limit_bytes;
    const ofLimit =
      typeof limit === 'number' && limit > 0
        ? Math.round(share(group.memory_bytes, limit))
        : null;
    return {
      key: group.app ?? '',
      app: group.app,
      label: group.app ?? OTHER_LABEL,
      memory: formatBytes(group.memory_bytes),
      memoryPercent: share(group.memory_bytes, resources.memory_total_bytes),
      limit:
        ofLimit === null
          ? '上限なし'
          : `上限 ${formatBytes(limit)}（${ofLimit}%）`,
      nearLimit: group.containers
        .filter(nearItsLimit)
        .map((container) => containerLabel(group, container))
        .join('、'),
      cpu: formatPercent(group.cpu_percent),
      cpuPercent: group.cpu_percent,
      breakdown: group.containers
        .map(
          (container) =>
            `${containerLabel(group, container)} ${formatBytes(container.memory_bytes)}`,
        )
        .join('、'),
    };
  });
}

export interface ContainerTotals {
  memory: string;
  memoryPercent: string;
  cpu: string;
  hostMemory: string;
  cpuCount: number;
}

/** What all running containers use together. */
export function containerTotals(
  resources: ContainerResources,
): ContainerTotals {
  let memory = 0;
  let cpu = 0;
  for (const group of resources.groups) {
    memory += group.memory_bytes;
    cpu += group.cpu_percent;
  }
  return {
    memory: formatBytes(memory),
    memoryPercent: formatPercent(share(memory, resources.memory_total_bytes)),
    cpu: formatPercent(cpu),
    hostMemory: formatBytes(resources.memory_total_bytes),
    cpuCount: resources.cpu_count,
  };
}
