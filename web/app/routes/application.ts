import Route from '@ember/routing/route';
import { service } from '@ember/service';
import type RouterService from '@ember/routing/router-service';
import type Transition from '@ember/routing/transition';
import type BrowserService from 'koroku-console/services/browser';
import type SessionService from 'koroku-console/services/session';
import type ThemeService from 'koroku-console/services/theme';

export default class ApplicationRoute extends Route {
  @service declare session: SessionService;
  @service declare router: RouterService;
  @service declare browser: BrowserService;
  @service declare theme: ThemeService;

  async beforeModel(transition: Transition): Promise<void> {
    // Creating the service applies a pinned theme; do it before anything
    // waits on the network.
    void this.theme.preference;

    // The OAuth callback redirects to `/?login_error=<code>` on refusal.
    const fromTransition = transition.to?.queryParams['login_error'];
    const code =
      typeof fromTransition === 'string'
        ? fromTransition
        : new URLSearchParams(this.browser.search).get('login_error');
    if (code) this.session.loginError = code;

    await this.session.load();

    if (!this.session.authenticated && transition.to?.name !== 'login') {
      this.router.replaceWith('login');
    }
  }
}
