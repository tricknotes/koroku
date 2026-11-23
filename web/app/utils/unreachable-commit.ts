// A deployment only takes a commit that its branch or tag reaches
// (ARCHITECTURE 7). The fetch step refuses any other commit, and koroku
// writes why in English; this reads that reason for the console.

import { isRollback } from './rollback';
import type { Deployment } from './types';

export interface UnreachableCommit {
  /** The abbreviated SHA koroku named. */
  sha: string;
  /** The branch or tag the commit was to be deployed from. */
  ref: string;
  /** The repository has no branch or tag of that name. */
  refMissing: boolean;
}

const NOT_REACHABLE = /commit ([0-9a-f]+) is not reachable from "([^"]+)"/;
const REF_MISSING =
  /no branch or tag named "([^"]+)" to deploy commit ([0-9a-f]+) from/;

/**
 * The commit a deployment refused because its ref does not reach it; null
 * for any other error.
 */
export function unreachableCommit(
  error: string | null | undefined,
): UnreachableCommit | null {
  const text = error ?? '';
  const notReachable = NOT_REACHABLE.exec(text);
  if (notReachable) {
    const [, sha = '', ref = ''] = notReachable;
    return { sha, ref, refMissing: false };
  }
  const refMissing = REF_MISSING.exec(text);
  if (refMissing) {
    const [, ref = '', sha = ''] = refMissing;
    return { sha, ref, refMissing: true };
  }
  return null;
}

/** The headline of such a failure, in the reader's words. */
export function unreachableCommitTitle(commit: UnreachableCommit): string {
  return commit.refMissing
    ? `ブランチかタグ「${commit.ref}」がリポジトリにないため、デプロイできません`
    : `コミット ${commit.sha} は「${commit.ref}」からたどれないため、デプロイできません`;
}

/**
 * The ref to deploy a history row's commit from again. A rollback record
 * carries a marker instead of a ref, so it takes the ref of the deployment
 * whose release it started; without one the server checks the commit
 * against the branch the app follows.
 */
export function redeployRef(
  deployment: Pick<Deployment, 'ref' | 'source_deployment_id'>,
  history: readonly Pick<Deployment, 'id' | 'ref' | 'source_deployment_id'>[],
): string | undefined {
  if (!isRollback(deployment)) return deployment.ref || undefined;
  const source = history.find((d) => d.id === deployment.source_deployment_id);
  return source && !isRollback(source) ? source.ref || undefined : undefined;
}
