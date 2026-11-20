import { tracked } from '@glimmer/tracking';
import { isValidEnvKey } from 'koroku-console/utils/format';
import type { EnvPreset } from 'koroku-console/utils/types';

/** Names of the frameworks `GET .../env-presets` can report. */
const FRAMEWORK_LABELS: Record<string, string> = {
  rails: 'Rails',
  rack: 'Rack',
  laravel: 'Laravel',
  django: 'Django',
  phoenix: 'Phoenix',
  elixir: 'Elixir',
  node: 'Node.js',
};

export function frameworkLabel(name: string): string {
  return FRAMEWORK_LABELS[name] ?? name;
}

/** Why a variable is suggested, in a short sentence. */
export function presetReason(preset: EnvPreset): string {
  const { file, match } = preset;
  if (match === '') return `${file} があるため`;
  switch (file) {
    case 'Gemfile':
    case 'composer.json':
      return `${file} に ${match} があるため`;
    case 'mix.exs':
      return `mix.exs の依存に ${match} があるため`;
    default:
      // A configuration file that reads the variable.
      return `${file} が ${match} を読むため`;
  }
}

/** koroku writes these itself (ARCHITECTURE 6). */
const RESERVED_PREFIX = 'KOROKU_';

let nextId = 0;

/** One variable of the create form. */
export class EnvDraftRow {
  readonly id = `env-draft-${++nextId}`;
  @tracked key: string;
  @tracked value: string;
  /** A generated secret the user chose to see. */
  @tracked revealed = false;
  /** The user typed in this row: detecting again leaves it alone. */
  @tracked edited = false;

  constructor(
    key: string,
    value: string,
    /** The suggestion the row came from; null for a row the user added. */
    readonly preset: EnvPreset | null = null,
  ) {
    this.key = key;
    this.value = value;
  }

  get generated(): boolean {
    return this.preset?.generated === true;
  }

  get masked(): boolean {
    return this.generated && !this.revealed;
  }

  get reason(): string {
    return this.preset ? presetReason(this.preset) : '';
  }

  get trimmedKey(): string {
    return this.key.trim();
  }

  /** Neither a key nor a value: left out when the app is created. */
  get blank(): boolean {
    return this.trimmedKey === '' && this.value === '';
  }
}

/**
 * The variables the create form will store with the app: suggestions from
 * the repository, as edited, plus rows the user added. Detecting again
 * (another branch) only replaces suggestions nobody touched, and does not
 * bring back the ones the user removed.
 */
export class EnvDraft {
  @tracked rows: EnvDraftRow[] = [];
  private dismissed = new Set<string>();

  applyPresets(presets: EnvPreset[]): void {
    const kept = this.rows.filter((row) => row.preset === null || row.edited);
    const taken = new Set(kept.map((row) => row.trimmedKey));
    const fresh = presets
      .filter((p) => !taken.has(p.key) && !this.dismissed.has(p.key))
      .map((p) => new EnvDraftRow(p.key, p.value, p));
    this.rows = [...fresh, ...kept];
  }

  /** Forgets everything (another repository). */
  reset(): void {
    this.rows = [];
    this.dismissed.clear();
  }

  add(): EnvDraftRow {
    const row = new EnvDraftRow('', '');
    this.rows = [...this.rows, row];
    return row;
  }

  remove(row: EnvDraftRow): void {
    if (row.preset) this.dismissed.add(row.preset.key);
    this.rows = this.rows.filter((r) => r !== row);
  }

  setKey(row: EnvDraftRow, key: string): void {
    row.key = key;
    row.edited = true;
  }

  setValue(row: EnvDraftRow, value: string): void {
    row.value = value;
    row.edited = true;
  }

  /** What is wrong with a row's key, by the API's rules. */
  keyError(row: EnvDraftRow): string | undefined {
    if (row.blank) return undefined;
    const key = row.trimmedKey;
    if (key === '') return 'キーを入力してください。';
    if (!isValidEnvKey(key)) {
      return '英字かアンダースコアで始まる英数字とアンダースコアだけが使えます。';
    }
    if (key.startsWith(RESERVED_PREFIX)) {
      return `${RESERVED_PREFIX} で始まる名前は koroku が使うため保存できません。`;
    }
    const twice = this.rows.some(
      (other) => other !== row && other.trimmedKey === key,
    );
    return twice ? 'キーが重複しています。' : undefined;
  }

  get valid(): boolean {
    return this.rows.every((row) => this.keyError(row) === undefined);
  }

  /** The body of `env` in `POST /api/apps`. */
  get vars(): { key: string; value: string }[] {
    return this.rows
      .filter((row) => !row.blank)
      .map((row) => ({ key: row.trimmedKey, value: row.value }));
  }
}
