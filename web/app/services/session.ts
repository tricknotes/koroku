import Service, { service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import type RouterService from '@ember/routing/router-service';
import { ApiError } from 'paas-console/utils/errors';
import type { SessionUser } from 'paas-console/utils/types';
import type ApiService from './api';

/** Who is logged in, as reported by `GET /api/session`. */
export default class SessionService extends Service {
  @service declare api: ApiService;
  @service declare router: RouterService;

  @tracked authenticated = false;
  @tracked user: SessionUser | null = null;
  /** `code` of the `?login_error=` redirect, kept for the login page. */
  @tracked loginError: string | null = null;
  /** Set when the session could not be determined at all. */
  @tracked loadError: unknown = null;

  private pending: Promise<void> | null = null;

  /** Loads the session once; later calls reuse the first result. */
  load(): Promise<void> {
    this.pending ??= this.fetchSession();
    return this.pending;
  }

  private async fetchSession(): Promise<void> {
    try {
      const session = await this.api.getSession();
      this.authenticated = session.authenticated;
      this.user = session.authenticated ? (session.user ?? null) : null;
    } catch (error) {
      if (error instanceof ApiError && error.status === 404) {
        // A backend without the login feature (before M3) has no session
        // endpoint and does not protect its API either: show the console.
        this.authenticated = true;
        this.user = null;
        return;
      }
      this.pending = null;
      this.loadError = error;
    }
  }

  /** Forgets the session and shows the login page (also used on any 401). */
  invalidate(): void {
    this.authenticated = false;
    this.user = null;
    if (this.router.currentRouteName !== 'login') {
      this.router.transitionTo('login');
    }
  }

  async logout(): Promise<void> {
    await this.api.logout();
    this.invalidate();
  }
}

declare module '@ember/service' {
  interface Registry {
    session: SessionService;
  }
}
