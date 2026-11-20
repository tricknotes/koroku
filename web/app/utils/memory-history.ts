// Geometry and wording of the memory history (ARCHITECTURE 9.5). Pure
// functions, so the chart component stays a template and tests need no
// rendering.

import type {
  MemoryHistory,
  MemoryPoint,
  MemoryRange,
  MemoryServiceHistory,
} from 'koroku-console/utils/types';

export const MEMORY_RANGES: { value: MemoryRange; label: string }[] = [
  { value: '24h', label: '24 時間' },
  { value: '7d', label: '7 日' },
  { value: '14d', label: '14 日' },
];

/**
 * A tick of the y axis: `768 MB`, `1.5 GB`. The axis is round in MiB, so
 * the decimals `formatBytes` keeps for measured values are noise here.
 */
export function axisBytes(bytes: number): string {
  const mib = bytes / 1024 ** 2;
  if (mib < 1024) return `${Math.round(mib)} MB`;
  return `${Math.round((mib / 1024) * 10) / 10} GB`;
}

const SHORT_JST = new Intl.DateTimeFormat('ja-JP', {
  timeZone: 'Asia/Tokyo',
  month: 'numeric',
  day: 'numeric',
  hour: '2-digit',
  minute: '2-digit',
  hourCycle: 'h23',
});

/** `11/20 03:00` (JST), for the ends of the time axis. */
export function axisTime(iso: string): string {
  const parts: Record<string, string> = {};
  for (const part of SHORT_JST.formatToParts(new Date(iso))) {
    parts[part.type] = part.value;
  }
  return `${parts['month']}/${parts['day']} ${parts['hour']}:${parts['minute']}`;
}

/** The plot is drawn in this box and stretched to the element's width. */
export const PLOT_WIDTH = 600;
export const PLOT_HEIGHT = 120;

const MIB = 1024 ** 2;
/** Hours of data the suggestion needs (ARCHITECTURE 9.5). */
export const READY_HOURS = 24;

export interface ChartTick {
  /** Distance from the top, in percent of the plot height. */
  top: number;
  label: string;
}

export interface ChartPoint {
  /** Position in percent of the plot box. */
  left: number;
  top: number;
  point: MemoryPoint;
}

export interface ChartGeometry {
  usagePath: string;
  areaPath: string;
  limitPath: string;
  ticks: ChartTick[];
  points: ChartPoint[];
}

/**
 * The top of the y axis: a power-of-two number of MiB times 1–4, a bit
 * above the highest of usage and limit, so the ticks stay round.
 */
export function axisTop(highest: number): number {
  const target = Math.max(highest * 1.08, 64 * MIB);
  let step = 16 * MIB;
  while (target / step > 4) step *= 2;
  return Math.ceil(target / step) * step;
}

function round(value: number): number {
  return Math.round(value * 100) / 100;
}

/**
 * Turns a service's points into SVG paths in the PLOT_WIDTH × PLOT_HEIGHT
 * box. Points more than one and a half steps apart are not joined: the
 * service was stopped, or koroku was not sampling.
 */
export function chartGeometry(
  service: MemoryServiceHistory,
  history: Pick<MemoryHistory, 'from' | 'to' | 'step_seconds'>,
): ChartGeometry {
  const from = Date.parse(history.from);
  const span = Math.max(1, Date.parse(history.to) - from);
  const step = history.step_seconds * 1000;
  let highest = 0;
  for (const p of service.points) {
    highest = Math.max(highest, p.max_bytes, p.limit_bytes ?? 0);
  }
  const top = axisTop(highest);
  const x = (at: number) => round(((at - from) / span) * PLOT_WIDTH);
  const y = (bytes: number) => round(PLOT_HEIGHT - (bytes / top) * PLOT_HEIGHT);

  const usage: string[] = [];
  const area: string[] = [];
  const limit: string[] = [];
  let run: { x: number; y: number }[] = [];
  const flush = () => {
    if (run.length === 0) return;
    usage.push(
      run.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x} ${p.y}`).join(' '),
    );
    const first = run[0]!;
    const last = run[run.length - 1]!;
    area.push(
      `M${first.x} ${PLOT_HEIGHT} ` +
        run.map((p) => `L${p.x} ${p.y}`).join(' ') +
        ` L${last.x} ${PLOT_HEIGHT} Z`,
    );
    run = [];
  };

  const points: ChartPoint[] = [];
  service.points.forEach((point, i) => {
    const at = Date.parse(point.at);
    const previous = service.points[i - 1];
    if (previous && at - Date.parse(previous.at) > step * 1.5) flush();
    const px = x(at);
    const py = y(point.max_bytes);
    run.push({ x: px, y: py });
    points.push({
      left: round((px / PLOT_WIDTH) * 100),
      top: round((py / PLOT_HEIGHT) * 100),
      point,
    });
    if (point.limit_bytes !== null) {
      // The limit holds until the next point (or one step on, at a gap).
      const next = service.points[i + 1];
      const nextAt = next ? Date.parse(next.at) : Infinity;
      const end = x(Math.min(at + step, nextAt, from + span));
      const ly = y(point.limit_bytes);
      const start = `${px} ${ly}`;
      const joined =
        previous?.limit_bytes != null &&
        at - Date.parse(previous.at) <= step * 1.5;
      limit.push(`${joined ? 'L' : 'M'}${start} L${Math.max(end, px)} ${ly}`);
    }
  });
  flush();

  return {
    usagePath: usage.join(' '),
    areaPath: area.join(' '),
    limitPath: limit.join(' '),
    ticks: [0, 0.5, 1].map((share) => ({
      top: round((1 - share) * 100),
      label: share === 0 ? '0' : axisBytes(top * share),
    })),
    points,
  };
}

/** The index of the point nearest to `fraction` (0–1) of the width. */
export function nearestPoint(points: ChartPoint[], fraction: number): number {
  let best = -1;
  let distance = Infinity;
  points.forEach((p, i) => {
    const d = Math.abs(p.left / 100 - fraction);
    if (d < distance) {
      best = i;
      distance = d;
    }
  });
  return best;
}

/** `640m` from bytes, the way the settings form takes it. */
export function limitSetting(bytes: number): string {
  return `${Math.round(bytes / MIB)}m`;
}

/** Bytes of a setting such as `512m` or `1g`; null when it is not one. */
export function settingBytes(setting: string): number | null {
  const match = /^([0-9]+)([bkmg])$/.exec(setting.trim());
  if (!match) return null;
  const units = { b: 1, k: 1024, m: MIB, g: 1024 * MIB } as const;
  return Number(match[1]) * units[match[2] as keyof typeof units];
}

/** `3.5 時間` / `2 日と 4 時間` of data. */
export function formatHours(hours: number): string {
  if (hours < 24) return `${Math.round(hours * 10) / 10} 時間`;
  const days = Math.floor(hours / 24);
  const rest = Math.round(hours - days * 24);
  return rest === 0 ? `${days} 日` : `${days} 日と ${rest} 時間`;
}

/** Share of the limit, `93%`; empty without a limit. */
export function shareOfLimit(bytes: number, limit: number | null): string {
  if (!limit) return '';
  return `${Math.round((bytes / limit) * 100)}%`;
}
