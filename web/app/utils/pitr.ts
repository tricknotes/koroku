/**
 * Point-in-time recovery helpers (ARCHITECTURE 10.9). Times are entered on
 * the clock of the console, JST, like every other time it shows.
 */

const JST_OFFSET_MS = 9 * 60 * 60 * 1000;

/** `2026-11-27T10:15:30` (JST) → `2026-11-27T01:15:30.000Z`; '' if invalid. */
export function jstInputToIso(value: string): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(
    value.trim(),
  );
  if (!match) return '';
  const [, y, mo, d, h, mi, s] = match;
  const utc = Date.UTC(
    Number(y),
    Number(mo) - 1,
    Number(d),
    Number(h),
    Number(mi),
    Number(s ?? 0),
  );
  return Number.isNaN(utc) ? '' : new Date(utc - JST_OFFSET_MS).toISOString();
}

/** An ISO time as the value of a datetime-local input on JST. */
export function isoToJstInput(iso: string): string {
  const date = new Date(Date.parse(iso) + JST_OFFSET_MS);
  return date.toISOString().slice(0, 19);
}

/** Why the time cannot be restored to, checked before asking; '' if fine. */
export function targetProblem(
  iso: string,
  earliest: string | null,
  now: Date,
): string {
  if (!iso) return '日時を入力してください（JST）。';
  const at = Date.parse(iso);
  if (at > now.getTime()) return '未来の時刻には戻せません。';
  if (!earliest) return 'まだベースバックアップがありません。';
  if (at < Date.parse(earliest)) {
    return '戻せる期間より前です。';
  }
  return '';
}

const STEP_LABELS: Record<string, string> = {
  check: '確認',
  stop: '停止',
  move: 'いまのデータの退避',
  base: 'ベースバックアップの展開',
  wal: 'WAL の取り出し',
  recover: '回復',
  cleanup: '回復の設定の削除',
  start: '起動',
  done: '完了',
};

export function pitrStepLabel(key: string): string {
  return STEP_LABELS[key] ?? key;
}
