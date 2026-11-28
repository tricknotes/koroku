// The Compose file an app deploys: the repository's, or one written in
// koroku (ARCHITECTURE 6.3).

import type { App, ComposeSource } from './types';

export const COMPOSE_SOURCE_LABELS: Record<ComposeSource, string> = {
  repository: 'リポジトリのファイル',
  inline: 'koroku に書く',
};

/** The name the written file has in a release. */
export const INLINE_COMPOSE_FILE = 'koroku.compose.yaml';

/** The most the API stores, in bytes of UTF-8. */
export const MAX_COMPOSE_CONTENT_BYTES = 64 * 1024;

/**
 * What installs the koroku plugin in Claude Code, one command a line
 * (docs/USAGE.md).
 */
export const PLUGIN_SETUP_COMMANDS = [
  '/plugin marketplace add tricknotes/koroku',
  '/plugin install koroku@koroku',
];

/**
 * Asks the prepare-app Skill for a Compose file to paste here: written
 * without touching the repository and rehearsed on the local Docker
 * (plugins/koroku/skills/prepare-app).
 */
export const COMPOSE_PROMPT =
  '/koroku:prepare-app このリポジトリを koroku で動かしたい。' +
  'Compose ファイルはリポジトリに置かず、koroku の管理画面に書く（Compose の置き場所は「koroku に書く」）。' +
  'リポジトリのファイルは変えずに Compose を作り、rehearse.sh --inline でリハーサルして、' +
  '管理画面に貼る Compose の内容と、公開するサービス、コンテナ側のポート、ヘルスチェックのパス、必要な環境変数を報告して。';

export function composeSourceOf(app: App): ComposeSource {
  return app.compose_source ?? 'repository';
}

/** Why a written file cannot be saved, or null when it can. */
export function composeContentProblem(content: string): string | null {
  if (content.trim() === '') return 'Compose の内容を入力してください。';
  if (new TextEncoder().encode(content).length > MAX_COMPOSE_CONTENT_BYTES) {
    return '64 KiB 以内にしてください。';
  }
  return null;
}

/** The line ends a textarea reports, as the API stores them. */
export function normalizeCompose(content: string): string {
  return content.replace(/\r\n/g, '\n');
}
