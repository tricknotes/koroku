// What started a deployment (ARCHITECTURE 5.2): the console or the API, or
// a push to GitHub when the app deploys on push.

import type { Deployment } from './types';

type TriggerFields = Pick<Deployment, 'trigger' | 'pusher'>;

/** A deployment started by a GitHub push rather than from the console. */
export function isPush(deployment: TriggerFields): boolean {
  return deployment.trigger === 'push';
}

/** "octo の push で自動デプロイ": who started it, for a title or a fact. */
export function pushTitle(deployment: TriggerFields): string {
  return deployment.pusher
    ? `${deployment.pusher} の push で自動デプロイ`
    : 'push で自動デプロイ';
}
