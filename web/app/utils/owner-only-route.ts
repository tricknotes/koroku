import Route from '@ember/routing/route';
import { service } from '@ember/service';
import type RouterService from '@ember/routing/router-service';
import type SessionService from 'koroku-console/services/session';

/**
 * A screen only the owner may use (ARCHITECTURE 10.7). Members are sent to
 * the app list; the API would refuse them anyway.
 */
export default class OwnerOnlyRoute extends Route {
  @service declare session: SessionService;
  @service declare router: RouterService;

  beforeModel(): void {
    if (!this.session.isOwner) this.router.replaceWith('index');
  }
}
