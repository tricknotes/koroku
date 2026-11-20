// What the deploy tab needs to know about instant rollbacks (ARCHITECTURE
// 7.1): a rollback is a deployment of its own that starts the kept release
// of an earlier deployment again, without fetching or building anything.

import { isActiveDeployment } from './status';
import type { Deployment } from './types';

type RollbackFields = Pick<Deployment, 'id' | 'source_deployment_id'>;

/** A deployment made by "この版に戻す" rather than from a commit. */
export function isRollback(
  deployment: Pick<Deployment, 'source_deployment_id'>,
): boolean {
  return (
    deployment.source_deployment_id !== null &&
    deployment.source_deployment_id !== undefined
  );
}

/**
 * The deployment whose release a deployment runs: itself, or the one a
 * rollback started again. The app's current release is always such an ID.
 */
export function releaseId(deployment: RollbackFields): number {
  return deployment.source_deployment_id ?? deployment.id;
}

/** "#10 の版を再起動": what a rollback did, for its row and its detail. */
export function rollbackSummary(deployment: RollbackFields): string {
  return `#${releaseId(deployment)} の版を再起動`;
}

type Current = Pick<Deployment, 'id'> | null | undefined;

function isCurrentRelease(deployment: RollbackFields, current: Current) {
  return current?.id === releaseId(deployment);
}

/**
 * Whether "この版に戻す" applies: the release is kept, and it is not the
 * one that runs already.
 */
export function canRollBack(deployment: Deployment, current: Current): boolean {
  return (
    deployment.rollback_available === true &&
    !isActiveDeployment(deployment) &&
    !isCurrentRelease(deployment, current)
  );
}

/**
 * A release that once ran but is not kept any more: going back to it means
 * deploying its commit again, with fetch and build. Only said when the
 * backend tells (`rollback_available: false`), not when it is silent.
 */
export function isReleaseGone(
  deployment: Deployment,
  current: Current,
): boolean {
  return (
    deployment.status === 'succeeded' &&
    deployment.rollback_available === false &&
    !isCurrentRelease(deployment, current)
  );
}
