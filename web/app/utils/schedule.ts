// Times that lie ahead: when the next scheduled run starts.

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/**
 * `3 時間後` style distance from `now` to a time in the future. A time that
 * has already come (a run that is due and about to start) reads `まもなく`;
 * empty for a missing or unreadable time.
 */
export function timeUntil(
  iso: string | null | undefined,
  now: number = Date.now(),
): string {
  if (!iso) return '';
  const at = new Date(iso).getTime();
  if (Number.isNaN(at)) return '';
  const diff = at - now;
  if (diff < MINUTE) return 'まもなく';
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)} 分後`;
  if (diff < DAY) return `${Math.floor(diff / HOUR)} 時間後`;
  return `${Math.floor(diff / DAY)} 日後`;
}
