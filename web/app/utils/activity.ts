// Japanese sentences for the activity of an app (`GET /api/apps/{slug}/
// activity`, ARCHITECTURE 10.7). The server records an action and a small
// JSON of details (docs/API.md); the console words them here.

import { roleLabel } from './roles';
import { ENVIRONMENT_LABELS } from './status';
import type { ActivityEntry, AppRole, Environment } from './types';

/** What an entry says: one line, and the specifics below it. */
export interface ActivityText {
  summary: string;
  details: string[];
}

/** Settings by the JSON name the server records, as the settings tab calls them. */
const SETTING_LABELS: Record<string, string> = {
  repo_full_name: 'リポジトリ',
  repo_url: 'リポジトリの URL',
  default_branch: '既定のブランチ',
  compose_path: 'Compose ファイルのパス',
  web_service: '公開するサービス',
  web_port: 'コンテナ側のポート',
  healthcheck_path: 'ヘルスチェックのパス',
  environment: '環境',
  relax_policy: 'ポリシー検査を緩める',
  memory_limit: 'メモリ上限（既定）',
  service_memory_limits: 'サービスごとのメモリ上限',
  backup_exclude_volumes: 'バックアップから外す volume',
  dump_hook_service: 'ダンプフックのサービス',
  dump_hook_command: 'ダンプフックのコマンド',
  backup_before_deploy: 'デプロイ前のバックアップ',
  load_hook_service: 'ロードフックのサービス',
  load_hook_command: 'ロードフックのコマンド',
  post_load_service: 'コピー後のコマンドのサービス',
  post_load_command: 'コピー後のコマンド',
  env_source: '必要な環境変数の照合元',
  env_example_path: '.env.example のパス',
  access_protection: 'アクセスの保護',
  protection_exempt_paths: '保護を外すパス',
  auto_deploy: 'push での自動デプロイ',
  auto_deploy_branch: '自動デプロイするブランチ',
  auto_deploy_workflow: '先に待つワークフロー',
  promote_from: '昇格元のステージングアプリ',
};

const MAX_VALUE = 60;

function clip(text: string): string {
  return text.length > MAX_VALUE ? `${text.slice(0, MAX_VALUE - 1)}…` : text;
}

function str(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function list(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((item): item is string => typeof item === 'string')
    : [];
}

/** A setting's value as the settings tab would show it. */
function settingValue(field: string, value: unknown): string {
  if (field === 'environment' && typeof value === 'string') {
    return ENVIRONMENT_LABELS[value as Environment] ?? value;
  }
  if (typeof value === 'boolean') return value ? '有効' : '無効';
  if (typeof value === 'number') return String(value);
  if (typeof value === 'string') return value ? clip(value) : '（なし）';
  if (Array.isArray(value)) {
    return value.length ? clip(value.join(', ')) : '（なし）';
  }
  if (value && typeof value === 'object') {
    const pairs = Object.entries(value).map(([k, v]) => `${k}=${String(v)}`);
    return pairs.length ? clip(pairs.join(', ')) : '（なし）';
  }
  return '（なし）';
}

function settingChange(change: unknown): string {
  if (!change || typeof change !== 'object') return '';
  const { field, from, to, secret } = change as Record<string, unknown>;
  const name = typeof field === 'string' ? field : '';
  const label = SETTING_LABELS[name] ?? name;
  if (secret) return `${label}（値は記録しません）`;
  return `${label}: ${settingValue(name, from)} → ${settingValue(name, to)}`;
}

function envDetails(details: Record<string, unknown>): string[] {
  const lines: string[] = [];
  const added = list(details['added']);
  const changed = list(details['changed']);
  const removed = list(details['removed']);
  const renamed = Array.isArray(details['renamed'])
    ? (details['renamed'] as Record<string, unknown>[])
    : [];
  if (added.length) lines.push(`追加: ${added.join(', ')}`);
  if (changed.length) lines.push(`値の変更: ${changed.join(', ')}`);
  for (const rename of renamed) {
    lines.push(`名前の変更: ${str(rename['from'])} → ${str(rename['to'])}`);
  }
  if (removed.length) lines.push(`削除: ${removed.join(', ')}`);
  return lines;
}

function taskDetails(details: Record<string, unknown>): string[] {
  const lines: string[] = [];
  const added = list(details['added']);
  const changed = list(details['changed']);
  const removed = list(details['removed']);
  if (added.length) lines.push(`追加: ${added.join(', ')}`);
  if (changed.length) lines.push(`変更: ${changed.join(', ')}`);
  if (removed.length) lines.push(`削除: ${removed.join(', ')}`);
  if (details['reordered'] === true) lines.push('並び順を変更');
  return lines;
}

function domainStatus(status: unknown): string {
  return status === 'active' ? '有効' : 'DNS の確認待ち';
}

function copyKind(kind: unknown): string {
  return kind === 'snapshot' ? 'バックアップから' : '稼働中から';
}

function memberName(details: Record<string, unknown>): string {
  const member = details['member'];
  if (member && typeof member === 'object') {
    const login = (member as Record<string, unknown>)['login'];
    if (typeof login === 'string' && login) return login;
  }
  return 'メンバー';
}

/** What changed in a saved registry credential; the token is never shown. */
function registryDetails(details: Record<string, unknown>): string[] {
  const lines: string[] = [];
  const from = details['from'];
  if (details['created'] === true) {
    lines.push(`ユーザー名: ${str(details['username'])}`);
  } else if (from && typeof from === 'object') {
    const before = from as Record<string, unknown>;
    if (str(before['registry']) !== str(details['registry'])) {
      lines.push(
        `レジストリ: ${str(before['registry'])} → ${str(details['registry'])}`,
      );
    }
    if (str(before['username']) !== str(details['username'])) {
      lines.push(
        `ユーザー名: ${str(before['username'])} → ${str(details['username'])}`,
      );
    }
  }
  if (details['created'] !== true && details['token_replaced'] === true) {
    lines.push('トークンを入れ替え');
  }
  return lines;
}

/** A workflow run's conclusion, as the reason a push was not deployed. */
const CONCLUSION_LABELS: Record<string, string> = {
  failure: '失敗した',
  cancelled: '取り消された',
  timed_out: 'タイムアウトした',
  action_required: '承認を待っている',
  startup_failure: '起動できなかった',
  skipped: 'スキップされた',
  stale: '古くなった',
  neutral: '成功しなかった',
};

function conclusionLabel(value: unknown): string {
  return CONCLUSION_LABELS[str(value)] ?? '成功しなかった';
}

function role(value: unknown): string {
  return roleLabel(typeof value === 'string' ? (value as AppRole) : null);
}

/** The sentence for an entry; an unknown action is shown as it is. */
export function describeActivity(entry: ActivityEntry): ActivityText {
  const d = entry.details ?? {};
  const hostname = str(d['hostname']);
  const snapshot = str(d['snapshot_id']).slice(0, 8);
  switch (entry.action) {
    case 'app.created': {
      const env = list(d['env_added']);
      return {
        summary: 'アプリを作成',
        details: env.length ? [`環境変数: ${env.join(', ')}`] : [],
      };
    }
    case 'app.settings_changed': {
      const changes = Array.isArray(d['changes']) ? d['changes'] : [];
      return {
        summary: '設定を変更',
        details: changes.map(settingChange).filter(Boolean),
      };
    }
    case 'app.started':
      return { summary: 'アプリを起動', details: [] };
    case 'app.stopped':
      return { summary: 'アプリを停止', details: [] };
    case 'app.restarted':
      return { summary: 'アプリを再起動', details: [] };
    case 'env.changed':
      return {
        summary:
          d['replaced'] === true
            ? '環境変数を .env 形式でまとめて保存'
            : '環境変数を変更',
        details: envDetails(d),
      };
    case 'domain.added':
      return {
        summary: `ドメイン ${hostname} を追加`,
        details: [domainStatus(d['status'])],
      };
    case 'domain.verified':
      return {
        summary: `ドメイン ${hostname} を再検証`,
        details: [`結果: ${domainStatus(d['status'])}`],
      };
    case 'domain.primary':
      return {
        summary:
          d['is_primary'] === true
            ? `${hostname} をプライマリに設定`
            : `${hostname} のプライマリ指定を解除`,
        details: [],
      };
    case 'domain.forced':
      return {
        summary:
          d['force_active'] === true
            ? `${hostname} を強制有効化`
            : `${hostname} の強制有効化を解除`,
        details: [],
      };
    case 'domain.deleted':
      return { summary: `ドメイン ${hostname} を削除`, details: [] };
    case 'registry.set':
      return {
        summary:
          d['created'] === true
            ? `コンテナレジストリ ${str(d['registry'])} の認証を設定`
            : `コンテナレジストリ ${str(d['registry'])} の認証を変更`,
        details: registryDetails(d),
      };
    case 'registry.removed':
      return {
        summary: `コンテナレジストリ ${str(d['registry'])} の認証を削除`,
        details: [],
      };
    case 'backup.restored':
      return {
        summary: 'volume をリストア',
        details: [`スナップショット ${snapshot}`],
      };
    case 'backup.downloaded':
      return {
        summary: 'DB のダンプをダウンロード',
        details: [`スナップショット ${snapshot}`],
      };
    case 'db_copy.started':
      return {
        summary: `${str(d['source'])} の DB をこのアプリへコピー`,
        details: [copyKind(d['source_kind'])],
      };
    case 'db_copy.sent':
      return {
        summary: 'この DB をステージングアプリへコピー',
        details: [copyKind(d['source_kind'])],
      };
    case 'task_run.started': {
      const name = str(d['task_name']);
      const command = `${str(d['service'])}: ${clip(str(d['command']))}`;
      return {
        summary: name ? `タスク「${name}」を実行` : 'コマンドを実行',
        details: [command],
      };
    }
    case 'tasks.changed':
      return { summary: '登録タスクを変更', details: taskDetails(d) };
    case 'member.role_set': {
      const who = memberName(d);
      return {
        summary: str(d['from'])
          ? `${who} のロールを${role(d['from'])}から${role(d['to'])}に変更`
          : `${who} に${role(d['to'])}のロールを付与`,
        details: [],
      };
    }
    case 'member.role_removed':
      return {
        summary: `${memberName(d)} のロール（${role(d['from'])}）を外す`,
        details: [],
      };
    case 'auto_deploy.skipped':
      return {
        summary: `ワークフローが${conclusionLabel(d['conclusion'])}ため自動デプロイせず`,
        details: [
          `ワークフロー: ${str(d['workflow'])}`,
          `コミット: ${str(d['commit_sha']).slice(0, 7)}（${str(d['branch'])}）`,
        ],
      };
    default:
      return { summary: entry.action, details: [] };
  }
}

/** Who did it: the GitHub login, or koroku for what it did by itself. */
export function activityActor(entry: ActivityEntry): string {
  return entry.actor?.login ?? 'koroku';
}
