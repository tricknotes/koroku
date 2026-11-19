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
  invalid_volumes: 'volume を削除するかどうかの指定が正しくありません。',
  body_too_large: '送信した内容が大きすぎます。',
  unsupported_media_type:
    'リクエストの形式（Content-Type）が JSON ではないため受け付けられませんでした。ページを再読み込みしてください。',
  method_not_allowed: 'この操作はサーバーが受け付けていません。',
  unauthenticated: 'ログインが必要です。',
  reauth_required:
    'この操作には再ログインが必要です（直近 10 分以内のログインが条件です）。',
  session_not_found: 'セッションが見つかりません。すでに終了しています。',
  forbidden_origin:
    'この画面からの操作として認められませんでした。ページを再読み込みしてください。',
  forbidden_host:
    'このホスト名ではコンソールを使えません。開発用モードでは localhost か 127.0.0.1 で開いてください。',
  not_found: '対象が見つかりません。',
  app_not_found: 'アプリが見つかりません。',
  deployment_not_found: 'デプロイが見つかりません。',
  domain_not_found: 'ドメインが見つかりません。',
  group_not_found:
    'グループが見つかりません。ほかの画面で削除されたかもしれません。再読み込みしてください。',
  snapshot_not_found:
    'スナップショットが見つからないか、この操作には使えない種類です。',
  slug_taken: 'この slug はすでに使われています。',
  no_free_port: '割り当てられるポートが残っていません。',
  not_cancelable: 'このデプロイはすでに終了しているためキャンセルできません。',
  hostname_taken: 'このホスト名はすでに登録されています。',
  group_name_taken: '同じ名前のグループがすでにあります。',
  group_not_empty:
    'メンバーのアプリがいるため削除できません。先に各アプリの設定タブでグループから外してください。',
  env_var_exists: '同じキーの環境変数がすでにあります。',
  env_var_not_found:
    'この環境変数は見つかりません。ほかの画面で変更されたかもしれません。再読み込みしてください。',
  busy: 'ほかの処理が実行中です。しばらくしてからやり直してください。',
  not_deployed: 'まだ一度もデプロイされていません。先にデプロイしてください。',
  validation_failed: '入力内容に誤りがあります。',
  policy_violation:
    '稼働中のリリースがポリシー検査を通らなくなっているため起動できません。再デプロイすると直ります。',
  file_too_large: 'ファイルが大きすぎて読み取れません。',
  release_unavailable:
    'この版のリリースはもう保持していないため、すぐには戻せません。戻すには、このコミットを再デプロイしてください。',
  ref_unresolvable: '指定したブランチまたはコミットが見つかりません。',
  disk_full: 'ディスク使用率が上限を超えているためデプロイできません。',
  internal:
    'サーバーでエラーが発生しました。詳細はサーバーのログを確認してください。',
  not_configured: 'この機能は設定されていません。',
  prune_failed: 'prune に失敗しました。',
  repo_not_found:
    'リポジトリ、ブランチまたはファイルが見つかりません。GitHub App がインストールされているか確認してください。',
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
  if (error.code === 'group_name_taken') {
    return { name: MESSAGES['group_name_taken'] as string };
  }
  if (error.code !== 'validation_failed') return {};
  const fields: Record<string, string> = {};
  for (const [name, reason] of Object.entries(error.details)) {
    fields[name] = typeof reason === 'string' ? reason : JSON.stringify(reason);
  }
  return fields;
}

/**
 * The findings of a `422 policy_violation`, one line each: the service (when
 * the finding is about one) and the reason as the server words it.
 */
export function policyViolations(error: unknown): string[] {
  if (!(error instanceof ApiError) || error.code !== 'policy_violation') {
    return [];
  }
  const violations = error.details['violations'];
  if (!Array.isArray(violations)) return [];
  const lines: string[] = [];
  for (const entry of violations as unknown[]) {
    if (typeof entry !== 'object' || entry === null) continue;
    const { service, message } = entry as Record<string, unknown>;
    if (typeof message !== 'string' || message === '') continue;
    lines.push(
      typeof service === 'string' && service !== ''
        ? `${service}: ${message}`
        : message,
    );
  }
  return lines;
}

export type Unavailability = 'not_configured' | 'unavailable';

const SPECIFIC_NOT_FOUND = new Set([
  'app_not_found',
  'deployment_not_found',
  'domain_not_found',
  'group_not_found',
  'repo_not_found',
  'snapshot_not_found',
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
  two_factor_required:
    'GitHub の 2 段階認証が有効になっていないため、ログインできません。GitHub の設定で 2 段階認証を有効にしてからやり直してください。',
  two_factor_unknown:
    'GitHub から 2 段階認証の状態を取得できなかったため、ログインを止めました。サーバーの設定（github.require_two_factor）を確認してください。',
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
