// Review apps of pull requests (ARCHITECTURE 9.9).
import type {
  App,
  PullRequestState,
  ReviewInfo,
} from 'koroku-console/utils/types';
import type { Tone } from 'koroku-console/utils/status';

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
 * Orders the app list so that each review app follows its parent, newest
 * pull request first. Review apps whose parent is not in the list keep
 * their place.
 */
export function groupReviewApps(apps: App[]): App[] {
  const slugs = new Set(apps.map((a) => a.slug));
  const children = new Map<string, App[]>();
  for (const app of apps) {
    const parent = app.review?.parent;
    if (parent && slugs.has(parent)) {
      const list = children.get(parent) ?? [];
      list.push(app);
      children.set(parent, list);
    }
  }
  const out: App[] = [];
  for (const app of apps) {
    const parent = app.review?.parent;
    if (parent && slugs.has(parent)) continue;
    out.push(app);
    const list = children.get(app.slug);
    if (list) {
      out.push(
        ...list.sort((a, b) => b.review!.pr_number - a.review!.pr_number),
      );
    }
  }
  return out;
}

/** A review app listed under its parent. */
export function isChildRow(app: App, apps: App[]): boolean {
  const parent = app.review?.parent;
  return !!parent && apps.some((a) => a.slug === parent);
}
