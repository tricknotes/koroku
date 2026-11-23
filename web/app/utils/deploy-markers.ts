// Deployments on the memory history (ARCHITECTURE 9.5): which ones are
// marked, where, and how they are described. Pure functions, so the
// placement is tested without rendering.

import { axisTime } from 'koroku-console/utils/memory-history';
import { formatDateTimeJst, shortSha } from 'koroku-console/utils/format';
import type { Deployment, MemoryRange } from 'koroku-console/utils/types';

/**
 * `deploy`: a deployment that succeeded (rollbacks and promotions too).
 * `reverted`: one that started the new release, failed or was canceled,
 * and went back to the previous release.
 */
export type MarkerKind = 'deploy' | 'reverted';

export interface DeployMarker {
  deployment: Deployment;
  kind: MarkerKind;
  /** When it finished, in milliseconds. */
  at: number;
  /** Distance from the left edge, in percent of the range. */
  left: number;
}

export interface MarkerGroup {
  /** Newest first. */
  markers: DeployMarker[];
  /** Where the group's button sits, in percent of the range. */
  left: number;
  /** `reverted` only when every deployment in it was reverted. */
  kind: MarkerKind;
}

/** The deployments the history reads: as many as the API returns at once. */
export const MARKER_LIMIT = 200;

const HOUR = 3600_000;
const RANGE_MS: Record<MemoryRange, number> = {
  '24h': 24 * HOUR,
  '7d': 7 * 24 * HOUR,
  '14d': 14 * 24 * HOUR,
};

/**
 * `finished_since` for a range ending now. It is read alongside the
 * history, before the server says where the range starts, so it reaches
 * back a little further; `deployMarkers` clips to the exact range.
 */
export function rangeStart(range: MemoryRange, now: number): string {
  return new Date(now - RANGE_MS[range] - 10 * 60_000).toISOString();
}

/** Markers closer than this (in pixels) share a button. */
export const MARKER_GAP_PX = 24;

function round(value: number): number {
  return Math.round(value * 100) / 100;
}

/** Whether a deployment changed what runs, and how. */
export function markerKind(deployment: Deployment): MarkerKind | null {
  if (!deployment.finished_at) return null;
  if (deployment.status === 'succeeded') return 'deploy';
  if (
    (deployment.status === 'failed' || deployment.status === 'canceled') &&
    deployment.containers_replaced === true
  ) {
    return 'reverted';
  }
  return null;
}

/**
 * The marked deployments within `from`–`to`, oldest first. Times are
 * compared as instants, so the offset an ISO string is written in does not
 * matter.
 */
export function deployMarkers(
  deployments: Deployment[],
  range: { from: string; to: string },
): DeployMarker[] {
  const from = Date.parse(range.from);
  const to = Date.parse(range.to);
  const span = to - from;
  if (!(span > 0)) return [];
  const out: DeployMarker[] = [];
  for (const deployment of deployments) {
    const kind = markerKind(deployment);
    if (!kind) continue;
    const at = Date.parse(deployment.finished_at!);
    if (Number.isNaN(at) || at < from || at > to) continue;
    out.push({ deployment, kind, at, left: round(((at - from) / span) * 100) });
  }
  return out.sort((a, b) => a.at - b.at || a.deployment.id - b.deployment.id);
}

/**
 * Puts markers that would sit closer than `gap` pixels on a track `width`
 * pixels wide into one group, so every button stays large enough to press.
 * A group spans at most `gap` from its first marker. Without a width (not
 * laid out yet), every marker is its own group.
 */
export function groupMarkers(
  markers: DeployMarker[],
  width: number,
  gap = MARKER_GAP_PX,
): MarkerGroup[] {
  const sorted = [...markers].sort((a, b) => a.at - b.at);
  const runs: DeployMarker[][] = [];
  for (const marker of sorted) {
    const run = runs[runs.length - 1];
    const first = run?.[0];
    if (
      run &&
      first &&
      width > 0 &&
      ((marker.left - first.left) / 100) * width < gap
    ) {
      run.push(marker);
    } else {
      runs.push([marker]);
    }
  }
  return runs.map((run) => {
    const first = run[0]!;
    const last = run[run.length - 1]!;
    return {
      markers: [...run].reverse(),
      left: round((first.left + last.left) / 2),
      kind: run.every((m) => m.kind === 'reverted') ? 'reverted' : 'deploy',
    };
  });
}

/** What started the deployment, in a few words. */
export function triggerText(deployment: Deployment): string {
  if (deployment.source_deployment_id) {
    const who = deployment.actor ? `（${deployment.actor.login}）` : '';
    return `ロールバック${who}`;
  }
  if (deployment.promoted_from) {
    const who = deployment.actor ? `（${deployment.actor.login}）` : '';
    return `${deployment.promoted_from.app_slug} から昇格${who}`;
  }
  if (deployment.trigger === 'push') {
    return deployment.pusher
      ? `push で自動デプロイ（${deployment.pusher}）`
      : 'push で自動デプロイ';
  }
  return deployment.actor ? `手動（${deployment.actor.login}）` : '手動';
}

/** The branch, or what a rollback went back to. */
export function refText(deployment: Deployment): string {
  if (deployment.source_deployment_id) {
    return `#${deployment.source_deployment_id} の版へ`;
  }
  return deployment.ref;
}

export function resultText(marker: DeployMarker): string {
  if (marker.kind === 'deploy') return '成功';
  return marker.deployment.status === 'canceled'
    ? 'キャンセルして戻した'
    : '失敗して戻した';
}

export function markerTime(marker: DeployMarker): string {
  return formatDateTimeJst(marker.deployment.finished_at);
}

function kindWord(marker: DeployMarker): string {
  if (marker.kind === 'reverted') return `${resultText(marker)}デプロイ`;
  if (marker.deployment.source_deployment_id) return 'ロールバック';
  if (marker.deployment.promoted_from) return '昇格';
  return 'デプロイ';
}

/** The button's accessible name: when, what kind, which commit. */
export function groupLabel(group: MarkerGroup): string {
  if (group.markers.length === 1) {
    const marker = group.markers[0]!;
    return `${axisTime(marker.deployment.finished_at!)} の${kindWord(marker)} ${shortSha(marker.deployment.commit_sha)}`;
  }
  const newest = group.markers[0]!;
  const oldest = group.markers[group.markers.length - 1]!;
  return `デプロイ ${group.markers.length} 件（${axisTime(oldest.deployment.finished_at!)}〜${axisTime(newest.deployment.finished_at!)}）`;
}

/**
 * The deployment whose release runs now: the newest succeeded one, if its
 * release is the app's current one.
 */
export function liveDeploymentId(
  deployments: Deployment[],
  current: Deployment | null,
): number | null {
  if (!current) return null;
  const newest = deployments
    .filter((d) => d.status === 'succeeded' && d.finished_at)
    .sort((a, b) => Date.parse(b.finished_at!) - Date.parse(a.finished_at!))[0];
  if (!newest) return null;
  return (newest.source_deployment_id ?? newest.id) === current.id
    ? newest.id
    : null;
}
