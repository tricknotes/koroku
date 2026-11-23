// Review apps of pull requests (ARCHITECTURE 9.9).
import type {
  App,
  PullRequestState,
  ReviewInfo,
  ReviewSummary,
} from 'koroku-console/utils/types';
import type { StatusKind, Tone } from 'koroku-console/utils/status';

export const PR_STATE_LABELS: Record<PullRequestState, string> = {
  open: 'オープン',
  closed: 'クローズ',
  merged: 'マージ済み',
};

export const PR_STATE_TONES: Record<PullRequestState, Tone> = {
  open: 'ok',
  closed: 'muted',
  merged: 'info',
};

/** The pull request is done with: the review app can be deleted. */
export function prFinished(review: ReviewInfo): boolean {
  return review.pr_state !== 'open';
}

/** Why the parent's data was not copied, in words. */
const COPY_DETAILS: Record<string, string> = {
  no_dump_hook:
    '親アプリにダンプフックがありません（バックアップタブで設定します）。',
  no_load_hook:
    '親アプリにロードフックがありません（バックアップタブで設定します）。',
  source_not_running: '親アプリが停止しています。',
  target_not_running: 'このアプリが停止しています。',
  target_not_staging: 'このアプリが staging ではありません。',
  parent_missing: '親アプリがありません。',
  error: 'コピーを始められませんでした。',
};

export function copyDetail(detail: string): string {
  return COPY_DETAILS[detail] ?? COPY_DETAILS['error']!;
}

/** What the parent of review apps lacks, from a `not_review_parent` reason. */
export const PARENT_PROBLEMS: Record<string, string> = {
  not_staging: 'レビューアプリを作れるのは staging のアプリだけです。',
  no_github_app:
    'レビューアプリには、GitHub App で取得するリポジトリが要ります。',
  review_app: 'レビューアプリから、さらにレビューアプリは作れません。',
};

/** Why an app cannot be the parent of review apps, or null if it can. */
export function parentProblem(app: App): string | null {
  if (app.review) return PARENT_PROBLEMS['review_app']!;
  if (app.environment !== 'staging') return PARENT_PROBLEMS['not_staging']!;
  if (!app.repo_full_name || app.repo_url) {
    return PARENT_PROBLEMS['no_github_app']!;
  }
  return null;
}

/** Whether the parent has the hooks the copy of its data needs. */
export function parentCanCopy(app: App): string | null {
  if (!app.dump_hook_service || !app.dump_hook_command) {
    return copyDetail('no_dump_hook');
  }
  if (!app.load_hook_service || !app.load_hook_command) {
    return copyDetail('no_load_hook');
  }
  return null;
}

/**
 * Whether the app's deploy tab has the review apps section: an app review apps can be made
 * from, or one that still has some (it was made production since).
 */
export function hasReviewTab(app: App): boolean {
  if (app.review) return false;
  return parentProblem(app) === null || (app.review_apps?.count ?? 0) > 0;
}

/** `レビューアプリ 3`, or null without any. */
export function reviewCount(summary: ReviewSummary | null | undefined) {
  return summary && summary.count > 0 ? summary.count : null;
}

/** `1 件失敗`, or null when none failed. */
export function reviewFailed(summary: ReviewSummary | null | undefined) {
  return summary && summary.failed > 0 ? `${summary.failed} 件失敗` : null;
}

/** `1 件デプロイ中`, or null when none is deploying. */
export function reviewDeploying(summary: ReviewSummary | null | undefined) {
  return summary && summary.deploying > 0
    ? `${summary.deploying} 件デプロイ中`
    : null;
}

export interface CopyState {
  kind: StatusKind;
  label: string;
  hint: string;
}

/** The newest copy from the parent, as far as it is known. */
export interface LatestCopy {
  status: string;
  error?: string;
}

/**
 * The state of the copy of the parent's data: the newest copy from the
 * parent once there is one, before that what the review app was created
 * with.
 */
export function copyState(
  review: ReviewInfo,
  latest: LatestCopy | null = review.data_copy_status
    ? { status: review.data_copy_status }
    : null,
): CopyState {
  if (latest) {
    if (latest.status === 'running') {
      return {
        kind: 'deploying',
        label: 'コピー中',
        hint: '親アプリの DB を流し込んでいます。',
      };
    }
    if (latest.status === 'succeeded') {
      return {
        kind: 'running',
        label: 'コピー済み',
        hint: '親アプリのデータをコピーしました。',
      };
    }
    return {
      kind: 'failed',
      label: 'コピーに失敗',
      hint: latest.error || 'ログはバックアップタブで確認できます。',
    };
  }
  switch (review.data_copy) {
    case 'waiting':
      return {
        kind: 'pending',
        label: '待機中',
        hint: '最初のデプロイが成功したあとで、親アプリのデータをコピーします。',
      };
    case 'started':
      return {
        kind: 'deploying',
        label: 'コピー中',
        hint: '親アプリの DB を流し込んでいます。',
      };
    case 'failed':
      return {
        kind: 'failed',
        label: 'コピーできませんでした',
        hint: copyDetail(review.data_copy_detail),
      };
    case 'skipped':
      return {
        kind: 'stopped',
        label: 'コピーしていません',
        hint: copyDetail(review.data_copy_detail),
      };
    default:
      return {
        kind: 'stopped',
        label: 'コピーしていません',
        hint: '作成のときにコピーしないことを選びました。',
      };
  }
}
