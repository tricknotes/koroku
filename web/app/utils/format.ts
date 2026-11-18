// Pure formatting helpers. They are plain functions so templates can call
// them directly and unit tests need no container.

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

function parse(iso: string | null | undefined): Date | null {
  if (!iso) return null;
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? null : date;
}

const JST = new Intl.DateTimeFormat('ja-JP', {
  timeZone: 'Asia/Tokyo',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hourCycle: 'h23',
});

/** The fields of a time on the JST wall clock, each zero-padded. */
function jstParts(date: Date): Record<string, string> {
  const parts: Record<string, string> = {};
  for (const part of JST.formatToParts(date)) parts[part.type] = part.value;
  return parts;
}

/** `2026/11/18` (JST). */
function formatDate(date: Date): string {
  const parts = jstParts(date);
  return `${parts['year']}/${parts['month']}/${parts['day']}`;
}

/**
 * `2026/11/18 12:34 JST`, whatever the browser's time zone: the absolute
 * time behind a relative one. The host runs on JST, and so do its schedules.
 */
export function formatDateTimeJst(iso: string | null | undefined): string {
  const date = parse(iso);
  if (!date) return '';
  const parts = jstParts(date);
  return `${formatDate(date)} ${parts['hour']}:${parts['minute']} JST`;
}

/**
 * `3 分前` style distance from `now`; falls back to the date (JST, like the
 * absolute time shown next to it) after 30 days.
 */
export function relativeTime(
  iso: string | null | undefined,
  now: number = Date.now(),
): string {
  const date = parse(iso);
  if (!date) return '';
  const diff = now - date.getTime();
  if (diff < MINUTE) return 'たった今';
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)} 分前`;
  if (diff < DAY) return `${Math.floor(diff / HOUR)} 時間前`;
  if (diff < 30 * DAY) return `${Math.floor(diff / DAY)} 日前`;
  return formatDate(date);
}

/** `1 分 9 秒` between two times; empty when either end is missing. */
export function formatDuration(
  from: string | null | undefined,
  to: string | null | undefined,
): string {
  const start = parse(from);
  const end = parse(to);
  if (!start || !end) return '';
  const seconds = Math.max(
    0,
    Math.round((end.getTime() - start.getTime()) / 1000),
  );
  if (seconds < 60) return `${seconds} 秒`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes} 分 ${seconds % 60} 秒`;
  return `${Math.floor(minutes / 60)} 時間 ${minutes % 60} 分`;
}

const UNITS = ['B', 'KB', 'MB', 'GB', 'TB'];

/** `8.0 GB` with 1024-based units. */
export function formatBytes(bytes: number | null | undefined): string {
  if (typeof bytes !== 'number' || !Number.isFinite(bytes) || bytes < 0) {
    return '';
  }
  let value = bytes;
  let unit = 0;
  const last = UNITS.length - 1;
  while (value >= 1024 && unit < last) {
    value /= 1024;
    unit += 1;
  }
  // Just below the next unit the rounded figure would read "1024.0 KB".
  if (unit > 0 && unit < last && Number(value.toFixed(1)) >= 1024) {
    value /= 1024;
    unit += 1;
  }
  return unit === 0
    ? `${value} ${UNITS[unit]}`
    : `${value.toFixed(1)} ${UNITS[unit]}`;
}

export function formatPercent(value: number | null | undefined): string {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '';
  return `${Math.round(value * 10) / 10}%`;
}

export function shortSha(sha: string | null | undefined): string {
  return (sha ?? '').slice(0, 7);
}

/** First line of a commit message. */
export function firstLine(text: string | null | undefined): string {
  return (text ?? '').split('\n', 1)[0] ?? '';
}

/** `www.example.org` from `https://www.example.org`; other text as is. */
export function hostOf(url: string | null | undefined): string {
  if (!url) return '';
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

const SHA_PATTERN = /^[0-9a-f]{40}$/;

export function isFullSha(value: string): boolean {
  return SHA_PATTERN.test(value);
}

/**
 * Slug suggestion from a repository name: lower-case letters, digits and
 * hyphens, starting with a letter, at most 40 characters (the API's rule).
 */
export function suggestSlug(repoFullName: string): string {
  const name = repoFullName.replace(/\/+$/, '').split('/').pop() ?? '';
  return name
    .toLowerCase()
    .replace(/\.git$/, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^[^a-z]+/, '')
    .slice(0, 40)
    .replace(/-+$/, '');
}

const SLUG_PATTERN = /^[a-z][a-z0-9-]{0,39}$/;

export function isValidSlug(slug: string): boolean {
  return SLUG_PATTERN.test(slug);
}

const ENV_KEY_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/;

export function isValidEnvKey(key: string): boolean {
  return ENV_KEY_PATTERN.test(key);
}
