// What the console shows about blue/green deploys of the public service
// (ARCHITECTURE 7.2): the setting's limits, the colour of a deployment and
// when it took over, and the failure for want of memory.

import type { Deployment } from './types';

/** The wait between the switch and stopping the old colour, as the API has it. */
export const DEFAULT_DRAIN_SECONDS = 10;
export const MAX_DRAIN_SECONDS = 300;

/** Where the Skill tells a repository how to get ready (requirements.md). */
export const BLUE_GREEN_GUIDE_URL =
  'https://github.com/tricknotes/koroku/blob/main/plugins/koroku/skills/prepare-app/references/requirements.md#無停止デプロイbluegreen選んだアプリだけ';

/** Why the drain field cannot be saved, or null when it can. */
export function drainSecondsProblem(value: string): string | null {
  const text = value.trim();
  if (!/^[0-9]+$/.test(text) || Number(text) > MAX_DRAIN_SECONDS) {
    return `0〜${MAX_DRAIN_SECONDS} の整数（秒）を入力してください。`;
  }
  return null;
}

const CLOCK = new Intl.DateTimeFormat('ja-JP', {
  timeZone: 'Asia/Tokyo',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hourCycle: 'h23',
});

/** `12:03:41` on the JST clock; empty for a missing or broken time. */
export function formatClockJst(iso: string | null | undefined): string {
  if (!iso) return '';
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return '';
  const parts: Record<string, string> = {};
  for (const part of CLOCK.formatToParts(date)) parts[part.type] = part.value;
  return `${parts['hour']}:${parts['minute']}:${parts['second']}`;
}

type ColorFields = Pick<Deployment, 'web_color' | 'switched_at'>;
type SummaryFields = ColorFields & Pick<Deployment, 'status'>;

/** "12:03:41 に green へ切り替え", or '' when the requests were not switched. */
export function switchText(deployment: ColorFields): string {
  const color = deployment.web_color;
  const clock = formatClockJst(deployment.switched_at);
  if (!color || !clock) return '';
  return `${clock} に ${color} へ切り替え`;
}

/**
 * What a blue/green deployment did with the public service, in one line:
 * "12:03:41 に green へ切り替え", "green で起動" (nothing ran to switch
 * from), "green を起動中", or "green を起動、切り替えなし" when it ended
 * before the switch or switched back. '' for a deployment without a colour.
 */
export function colorSummary(deployment: SummaryFields): string {
  const color = deployment.web_color;
  if (!color) return '';
  const switched = switchText(deployment);
  if (switched) return switched;
  switch (deployment.status) {
    case 'succeeded':
      return `${color} で起動`;
    case 'queued':
    case 'running':
      return `${color} を起動中`;
    default:
      return `${color} を起動、切り替えなし`;
  }
}

export interface MemoryShortage {
  service: string;
  available: string;
  needed: string;
}

const MEMORY_SHORTAGE =
  /Not enough memory to run two (\S+) containers \(([^,]+) available, ([^)]+) needed\)/;

/**
 * The host could not hold a second copy of the public service, so the
 * deployment stopped before starting it; null for any other error.
 */
export function memoryShortage(
  error: string | null | undefined,
): MemoryShortage | null {
  const match = MEMORY_SHORTAGE.exec(error ?? '');
  if (!match) return null;
  const [, service = '', available = '', needed = ''] = match;
  return { service, available, needed };
}

/** The headline of a failure for want of memory, in the reader's words. */
export const MEMORY_SHORTAGE_TITLE =
  'メモリが足りないため無停止で入れ替えられません';

/** "空き 312 MiB、必要 512 MiB". */
export function memoryShortageDetail(shortage: MemoryShortage): string {
  return `空き ${shortage.available}、必要 ${shortage.needed}`;
}
