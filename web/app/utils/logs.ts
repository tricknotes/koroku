// Pure helpers of the log screen: JST timestamps, the period presets and
// keyword matching. Plain functions, so templates call them directly and
// unit tests need no container.

import type { LogLine } from 'koroku-console/utils/types';

/** The pseudo-service that holds the reverse proxy's access log of an app. */
export const ACCESS_SERVICE = '_access';

/** How long logs stay on the host; older ones only exist as archives. */
export const LOCAL_RETENTION_DAYS = 14;

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** JST has no daylight saving time, so a fixed offset is exact. */
const JST_OFFSET = 9 * HOUR;

function pad(value: number, length = 2): string {
  return String(value).padStart(length, '0');
}

/** The date shifted so that its UTC getters read the JST wall clock. */
function inJst(iso: string | null | undefined): Date | null {
  if (!iso) return null;
  const time = new Date(iso).getTime();
  return Number.isNaN(time) ? null : new Date(time + JST_OFFSET);
}

/** `11/18 12:34:56` (JST): the time column of a log row. */
export function formatLogStamp(iso: string | null | undefined): string {
  const date = inJst(iso);
  if (!date) return '';
  return (
    `${pad(date.getUTCMonth() + 1)}/${pad(date.getUTCDate())} ` +
    `${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}:${pad(date.getUTCSeconds())}`
  );
}

/** `2026/11/18 12:34:56.123 JST`: the exact time in a row's detail. */
export function formatLogTime(iso: string | null | undefined): string {
  const date = inJst(iso);
  if (!date) return '';
  return (
    `${date.getUTCFullYear()}/${pad(date.getUTCMonth() + 1)}/${pad(date.getUTCDate())} ` +
    `${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}:${pad(date.getUTCSeconds())}` +
    `.${pad(date.getUTCMilliseconds(), 3)} JST`
  );
}

const INPUT_PATTERN = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2})?$/;

/**
 * Value of a `datetime-local` input, read as JST whatever the browser's
 * time zone, as RFC 3339 (UTC). Empty or malformed input gives ''.
 */
export function jstInputToIso(value: string): string {
  if (!INPUT_PATTERN.test(value)) return '';
  const date = new Date(`${value.length === 16 ? `${value}:00` : value}+09:00`);
  return Number.isNaN(date.getTime()) ? '' : date.toISOString();
}

/** A time as the value of a `datetime-local` input showing JST. */
export function toJstInput(time: number): string {
  const date = new Date(time + JST_OFFSET);
  return (
    `${date.getUTCFullYear()}-${pad(date.getUTCMonth() + 1)}-${pad(date.getUTCDate())}` +
    `T${pad(date.getUTCHours())}:${pad(date.getUTCMinutes())}`
  );
}

/**
 * What the period control selects: the live tail, a span ending now, a
 * custom range, or the archives of what is no longer on the host.
 */
export type Period =
  'tail' | '15m' | '1h' | '24h' | '7d' | 'custom' | 'archive';

export interface PeriodOption {
  value: Period;
  label: string;
  /** Length of a span that ends now. */
  span?: number;
}

export const PERIODS: PeriodOption[] = [
  { value: 'tail', label: '最新' },
  { value: '15m', label: '直近 15 分', span: 15 * MINUTE },
  { value: '1h', label: '直近 1 時間', span: HOUR },
  { value: '24h', label: '直近 24 時間', span: DAY },
  { value: '7d', label: '直近 7 日', span: 7 * DAY },
  { value: 'custom', label: '期間を指定' },
  { value: 'archive', label: '14 日より前（アーカイブ）' },
];

export function isPeriod(value: string): value is Period {
  return PERIODS.some((option) => option.value === value);
}

export function periodSpan(period: Period): number | undefined {
  return PERIODS.find((option) => option.value === period)?.span;
}

/** Whether a time lies before what the host still keeps. */
export function isBeyondRetention(iso: string, now: number): boolean {
  if (!iso) return false;
  return new Date(iso).getTime() < now - LOCAL_RETENTION_DAYS * DAY;
}

/** The pseudo-service of deploy logs; it only exists among the archives. */
export const DEPLOY_SERVICE = '_deploy';

/** Name of a service as the controls show it. */
export function serviceLabel(name: string): string {
  if (name === ACCESS_SERVICE) return `アクセスログ（${name}）`;
  if (name === DEPLOY_SERVICE) return `デプロイログ（${name}）`;
  return name;
}

/**
 * One SSE payload as a log line. A payload that is not a log line is kept
 * as text, so nothing the server sent is hidden.
 */
export function parseLogLine(data: string): LogLine {
  try {
    const parsed: unknown = JSON.parse(data);
    if (
      typeof parsed === 'object' &&
      parsed !== null &&
      typeof (parsed as LogLine).line === 'string'
    ) {
      const line = parsed as Partial<LogLine>;
      return {
        service: typeof line.service === 'string' ? line.service : '',
        time: typeof line.time === 'string' ? line.time : '',
        line: line.line as string,
      };
    }
  } catch {
    // Not JSON: fall through.
  }
  return { service: '', time: '', line: data };
}

export interface TextPart {
  text: string;
  match: boolean;
}

/**
 * Splits text around the occurrences of a keyword, to mark them. Matching
 * is case-sensitive, as the search of the API is.
 */
export function splitMatches(text: string, keyword: string): TextPart[] {
  if (!keyword) return [{ text, match: false }];
  const parts: TextPart[] = [];
  let from = 0;
  for (;;) {
    const at = text.indexOf(keyword, from);
    if (at < 0) break;
    if (at > from) parts.push({ text: text.slice(from, at), match: false });
    parts.push({ text: keyword, match: true });
    from = at + keyword.length;
  }
  if (from < text.length || parts.length === 0) {
    parts.push({ text: text.slice(from), match: false });
  }
  return parts;
}

const NUMBER = new Intl.NumberFormat('ja-JP');

/** `1,000`: line counts. */
export function formatCount(value: number): string {
  return NUMBER.format(value);
}
