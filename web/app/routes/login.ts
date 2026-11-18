import Route from '@ember/routing/route';
import { service } from '@ember/service';
import type RouterService from '@ember/routing/router-service';
import type SessionService from 'koroku-console/services/session';

export default class LoginRoute extends Route {
  @service declare session: SessionService;
  @service declare router: RouterService;

  beforeModel(): void {
    if (this.session.authenticated) {
      this.router.replaceWith('index');
    }
  }
}
