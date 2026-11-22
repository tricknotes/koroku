// Wording and comparisons of the Compose tab (ARCHITECTURE 9.10).

import type { StatusKind, Tone } from 'koroku-console/utils/status';
import type {
  ComposeRelease,
  ContainerInfo,
  ContainerMismatch,
} from 'koroku-console/utils/types';
import { formatBytes, shortSha } from 'koroku-console/utils/format';

export interface MismatchMarker {
  code: ContainerMismatch;
  label: string;
  hint: string;
  tone: Tone;
}

/** Troubles with the container itself come first, then the differences. */
const ORDER: ContainerMismatch[] = [
  'oom_killed',
  'restarting',
  'unhealthy',
  'exited',
  'restarted',
  'outdated',
  'orphan',
  'memory_limit',
  'memory_reservation',
  'oom_score_adj',
];

export function mismatchMarkers(container: ContainerInfo): MismatchMarker[] {
  return [...container.mismatches]
    .sort((a, b) => ORDER.indexOf(a) - ORDER.indexOf(b))
    .map((code) => marker(code, container));
}

function marker(code: ContainerMismatch, c: ContainerInfo): MismatchMarker {
  switch (code) {
    case 'oom_killed':
      return {
        code,
        tone: 'danger',
        label: 'メモリ不足で停止',
        hint: '最後の停止はメモリ不足によるものです（OOM killer）。コンテナが起動し直すと、この記録は消えます。',
      };
    case 'restarting':
      return {
        code,
        tone: 'danger',
        label: '再起動中',
        hint: '止まっては起動し直すのを繰り返しています。ログタブで出力を確かめてください。',
      };
    case 'unhealthy':
      return {
        code,
        tone: 'danger',
        label: 'ヘルスチェック失敗',
        hint: 'Compose のヘルスチェックが通っていません。',
      };
    case 'exited':
      return {
        code,
        tone: 'danger',
        label: `異常終了（コード ${c.exit_code}）`,
        hint: '0 以外の終了コードで止まっています。',
      };
    case 'restarted':
      return {
        code,
        tone: 'warn',
        label: `自動の再起動 ${c.restart_count} 回`,
        hint: 'Docker がこのコンテナを起動し直した回数です。メモリ上限に当たって止められたときにも増えます。',
      };
    case 'outdated':
      return {
        code,
        tone: 'warn',
        label: '古い設定',
        hint: '稼働中のリリースとは違う設定で作られたコンテナです。次の起動かデプロイで作り直されます。',
      };
    case 'orphan':
      return {
        code,
        tone: 'warn',
        label: 'リリースにないサービス',
        hint: '稼働中のリリースの Compose にないサービスのコンテナです。',
      };
    case 'memory_limit':
      return {
        code,
        tone: 'warn',
        label: 'メモリ上限が違う',
        hint: 'Docker が付けている上限が、稼働中のリリースの上限と違います。',
      };
    case 'memory_reservation':
      return {
        code,
        tone: 'warn',
        label: 'メモリの予約が違う',
        hint: 'Docker が付けている予約が、稼働中のリリースの予約と違います。',
      };
    case 'oom_score_adj':
      return {
        code,
        tone: 'warn',
        label: 'OOM の順序が違う',
        hint: 'oom_score_adj が稼働中のリリースの値と違います。',
      };
  }
}

/** How the state of a container reads, as one of the five kinds. */
export function containerStatus(c: ContainerInfo): {
  kind: StatusKind;
  label: string;
} {
  switch (c.state) {
    case 'running':
      if (c.health === 'unhealthy') {
        return { kind: 'failed', label: '稼働中（不調）' };
      }
      if (c.health === 'starting') return { kind: 'pending', label: '起動中' };
      return { kind: 'running', label: '稼働中' };
    case 'restarting':
      return { kind: 'failed', label: '再起動中' };
    case 'exited':
    case 'dead':
      return c.exit_code === 0
        ? { kind: 'stopped', label: '停止' }
        : { kind: 'failed', label: `停止（コード ${c.exit_code}）` };
    case 'paused':
      return { kind: 'stopped', label: '一時停止' };
    default:
      return { kind: 'stopped', label: '停止' };
  }
}

function bytesOrNone(bytes: number): string {
  return bytes > 0 ? formatBytes(bytes) : 'なし';
}

/** A value Docker applies, with the release's value when they differ. */
export interface Compared {
  value: string;
  expected: string;
  differs: boolean;
}

function compared(actual: string, expected: string | null): Compared {
  return {
    value: actual,
    expected: expected ?? '',
    differs: expected !== null && expected !== actual,
  };
}

export function memoryLimit(c: ContainerInfo): Compared {
  return compared(
    bytesOrNone(c.memory_limit_bytes),
    c.expected ? bytesOrNone(c.expected.memory_limit_bytes) : null,
  );
}

export function memoryReservation(c: ContainerInfo): Compared {
  return compared(
    bytesOrNone(c.memory_reservation_bytes),
    c.expected ? bytesOrNone(c.expected.memory_reservation_bytes) : null,
  );
}

export function oomScoreAdj(c: ContainerInfo): Compared {
  return compared(
    String(c.oom_score_adj),
    c.expected ? String(c.expected.oom_score_adj) : null,
  );
}

/** "#42 のデプロイ（イメージのタグから）" and the like. */
export function createdBy(c: ContainerInfo): string {
  if (c.deployment_id === null) return '';
  const how =
    c.matched_by === 'image' ? 'イメージのタグから' : '作成時刻からの推定';
  return `#${c.deployment_id} のデプロイ（${how}）`;
}

/** The short image ID, or the registry digest when there is one. */
export function imageIdentity(c: ContainerInfo): string {
  if (c.image_digest) {
    const [, digest = c.image_digest] = c.image_digest.split('@');
    return digest.slice(0, 19);
  }
  return c.image_id.replace(/^sha256:/, '').slice(0, 12);
}

export function portsText(c: ContainerInfo): string {
  return c.ports
    .map(
      (p) =>
        `${p.host_ip ? `${p.host_ip}:` : ''}${p.host_port} → ${p.container_port}`,
    )
    .join('、');
}

/** "#42 abc1234 Fix the header（稼働中）" for the release selector. */
export function releaseLabel(release: ComposeRelease): string {
  const message = release.commit_message.split('\n')[0] ?? '';
  const text =
    `#${release.deployment_id} ${shortSha(release.commit_sha)} ${message}`.trim();
  return release.current ? `${text}（稼働中）` : text;
}
