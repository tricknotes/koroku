// One place that turns API errors into Japanese messages.

export class ApiError extends Error {
  status: number;
  code: string;
  details: Record<string, unknown>;

  constructor(
    status: number,
    code: string,
    message: string,
    details: Record<string, unknown> = {},
  ) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

const MESSAGES: Record<string, string> = {
  invalid_json: 'リクエストの形式が正しくありません。',
  invalid_limit: '件数の指定が正しくありません。',
  unauthenticated: 'ログインが必要です。',
  forbidden_origin:
    'この画面からの操作として認められませんでした。ページを再読み込みしてください。',
  not_found: '対象が見つかりません。',
  app_not_found: 'アプリが見つかりません。',
  deployment_not_found: 'デプロイが見つかりません。',
  domain_not_found: 'ドメインが見つかりません。',
  slug_taken: 'この slug はすでに使われています。',
  no_free_port: '割り当てられるポートが残っていません。',
  not_cancelable: 'このデプロイはすでに終了しているためキャンセルできません。',
  hostname_taken: 'このホスト名はすでに登録されています。',
  busy: 'ほかの処理が実行中です。しばらくしてからやり直してください。',
  not_deployed: 'まだ一度もデプロイされていません。先にデプロイしてください。',
  validation_failed: '入力内容に誤りがあります。',
  ref_unresolvable: '指定したブランチまたはコミットが見つかりません。',
  disk_full: 'ディスク使用率が上限を超えているためデプロイできません。',
  internal:
    'サーバーでエラーが発生しました。詳細はサーバーのログを確認してください。',
  not_configured: 'この機能は設定されていません。',
  repo_not_found:
    'リポジトリが見つかりません。GitHub App がインストールされているか確認してください。',
  github_rate_limited:
    'GitHub API の利用上限に達しました。しばらくしてからやり直してください。',
  github_error: 'GitHub との通信に失敗しました。',
  network_error: 'サーバーに接続できませんでした。',
};

const GENERIC = 'エラーが発生しました。';

function percent(value: unknown): string | null {
  return typeof value === 'number' ? `${Math.round(value * 10) / 10}%` : null;
}

/** Japanese message for anything thrown by the API service. */
export function errorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    const known = MESSAGES[error.code];
    if (known === undefined) {
      return error.message ? `${GENERIC}（${error.message}）` : GENERIC;
    }
    if (error.code === 'disk_full') {
      const used = percent(error.details['used_percent']);
      const limit = percent(error.details['limit_percent']);
      if (used && limit) {
        return `${known}（使用率 ${used} / 上限 ${limit}）`;
      }
    }
    return known;
  }
  if (error instanceof Error && error.message) {
    return `${GENERIC}（${error.message}）`;
  }
  return GENERIC;
}

/**
 * Field name → reason from a `422 validation_failed` response. `slug_taken`
 * and `hostname_taken` are mapped onto their field so forms can show them
 * inline as well.
 */
export function fieldErrors(error: unknown): Record<string, string> {
  if (!(error instanceof ApiError)) return {};
  if (error.code === 'slug_taken') {
    return { slug: MESSAGES['slug_taken'] as string };
  }
  if (error.code === 'hostname_taken') {
    return { hostname: MESSAGES['hostname_taken'] as string };
  }
  if (error.code !== 'validation_failed') return {};
  const fields: Record<string, string> = {};
  for (const [name, reason] of Object.entries(error.details)) {
    fields[name] = typeof reason === 'string' ? reason : JSON.stringify(reason);
  }
  return fields;
}

export type Unavailability = 'not_configured' | 'unavailable';

const SPECIFIC_NOT_FOUND = new Set([
  'app_not_found',
  'deployment_not_found',
  'domain_not_found',
  'repo_not_found',
]);

/**
 * Whether an error means "this whole feature is missing": the setting it
 * needs is absent (`503 not_configured`) or the backend does not have the
 * endpoint yet (a 404 that is not about a specific record).
 */
export function unavailability(error: unknown): Unavailability | null {
  if (!(error instanceof ApiError)) return null;
  if (error.status === 503 && error.code === 'not_configured') {
    return 'not_configured';
  }
  if (error.status === 404 && !SPECIFIC_NOT_FOUND.has(error.code)) {
    return 'unavailable';
  }
  return null;
}

export const UNAVAILABLE_MESSAGES: Record<Unavailability, string> = {
  not_configured: 'この機能は設定されていません。',
  unavailable: 'この機能はまだ利用できません。',
};

const LOGIN_ERRORS: Record<string, string> = {
  not_allowed: 'このアカウントはログインを許可されていません。',
  state_mismatch:
    'ログインの途中で状態が一致しなくなりました。もう一度やり直してください。',
  github_error:
    'GitHub との通信に失敗しました。時間をおいてやり直してください。',
  not_configured: 'GitHub ログインがサーバーに設定されていません。',
};

/** Message for the `?login_error=<code>` redirect of the OAuth callback. */
export function loginErrorMessage(code: string | null | undefined): string {
  if (!code) return '';
  return LOGIN_ERRORS[code] ?? 'ログインに失敗しました。';
}
