import type {
  EnvRequirement,
  EnvRequirements,
  EnvRequirementsReason,
  EnvSource,
} from 'koroku-console/utils/types';

export const ENV_SOURCE_LABELS: Record<EnvSource, string> = {
  compose: 'Compose ファイルの ${VAR}',
  dotenv: '.env.example',
  off: '照合しない',
};

const REASONS: Record<EnvRequirementsReason, string> = {
  file_not_found: 'ファイルがリポジトリにありません。',
  repo_unavailable:
    'GitHub App がこのリポジトリにインストールされていないため、ファイルを読めません。',
  unsupported_repo:
    'リポジトリを URL で指定したアプリでは、ファイルを読めません（GitHub のリポジトリ名で指定したアプリだけが対象です）。',
  not_configured: 'GitHub App が設定されていないため、ファイルを読めません。',
  ref_not_found: 'ブランチかコミットが見つかりません。',
  too_large: 'ファイルが大きすぎて読めません。',
  rate_limited:
    'GitHub への問い合わせが制限されています。しばらくしてから開き直してください。',
  github_error: 'GitHub への問い合わせに失敗しました。',
};

/** Why nothing could be compared, in a sentence. */
export function reasonText(reason: EnvRequirementsReason | ''): string {
  return reason ? REASONS[reason] : '';
}

/** The file the list comes from, as the reader knows it. */
export function sourceFile(req: EnvRequirements): string {
  if (req.path) return req.path;
  return req.source === 'dotenv' ? '.env.example' : 'Compose ファイル';
}

export interface Missing {
  required: EnvRequirement[];
  optional: EnvRequirement[];
}

/**
 * The variables the repository asks for that are not saved. `saved` are
 * the keys on screen, which may be newer than the response.
 */
export function missingVariables(
  req: EnvRequirements | null,
  saved?: Set<string>,
): Missing {
  const missing: Missing = { required: [], optional: [] };
  if (!req || req.status !== 'ok') return missing;
  for (const v of req.variables) {
    const isSet = saved ? saved.has(v.key) : v.set;
    if (isSet) continue;
    (v.required ? missing.required : missing.optional).push(v);
  }
  return missing;
}

/** Saved keys a `.env.example` no longer lists (only meaningful there). */
export function unlistedKeys(
  req: EnvRequirements | null,
  saved: string[],
): string[] {
  if (!req || req.status !== 'ok' || req.source !== 'dotenv') return [];
  const listed = new Set(req.variables.map((v) => v.key));
  return saved.filter((key) => !listed.has(key)).sort();
}

/** "SMTP_HOST、API_KEY" — a short list of keys for sentences. */
export function keyList(vars: EnvRequirement[], limit = 5): string {
  const keys = vars.map((v) => v.key);
  if (keys.length <= limit) return keys.join('、');
  return `${keys.slice(0, limit).join('、')} ほか ${keys.length - limit} 件`;
}
