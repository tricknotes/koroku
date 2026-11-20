// What a user may do on an app (ARCHITECTURE 10.7). The server enforces
// every rule; the console only hides or disables what would be refused.

import type { App, AppRole, MemberRole } from './types';

const RANK: Record<AppRole, number> = {
  view: 1,
  deploy: 2,
  manage: 3,
  owner: 4,
};

/** The roles a member can hold, in order, with their labels. */
export const MEMBER_ROLES: { value: MemberRole; label: string }[] = [
  { value: 'view', label: '閲覧' },
  { value: 'deploy', label: 'デプロイ' },
  { value: 'manage', label: '管理' },
];

const LABELS: Record<AppRole, string> = {
  view: '閲覧',
  deploy: 'デプロイ',
  manage: '管理',
  owner: 'オーナー',
};

/** Japanese name of a role. */
export function roleLabel(role: AppRole | undefined | null): string {
  return role ? LABELS[role] : '';
}

/** One line on what a role allows, for the members screen. */
export const ROLE_HINTS: Record<MemberRole, string> = {
  view: '概要・デプロイの履歴・ログ・設定を見られます。環境変数は名前だけです。',
  deploy: '閲覧に加えて、デプロイ・ロールバック・起動と停止ができます。',
  manage:
    'デプロイに加えて、環境変数・設定・ドメイン・バックアップ・タスクを扱えます。',
};

/**
 * The user's role on the app. Older backends send none: their only user
 * is the owner.
 */
export function appRole(app: App | null | undefined): AppRole {
  return app?.role ?? 'owner';
}

/** Whether the user's role on the app is at least `needed`. */
export function can(app: App | null | undefined, needed: AppRole): boolean {
  return RANK[appRole(app)] >= RANK[needed];
}
