import Service, { service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import type RouterService from '@ember/routing/router-service';
import type { SessionUser } from 'koroku-console/utils/types';
import type ApiService from './api';
import type BrowserService from './browser';

/** Where the OAuth flow starts; `return_to` brings the user back. */
export function loginUrl(returnTo?: string): string {
  if (!returnTo || returnTo === '/') return '/auth/github/login';
  return `/auth/github/login?return_to=${encodeURIComponent(returnTo)}`;
}

/** Who is logged in, as reported by `GET /api/session`. */
export default class SessionService extends Service {
  @service declare api: ApiService;
  @service declare router: RouterService;
  @service declare browser: BrowserService;

  @tracked authenticated = false;
  @tracked user: SessionUser | null = null;
  /** `code` of the `?login_error=` redirect, kept for the login page. */
  @tracked loginError: string | null = null;
  /** Set when the session could not be determined at all. */
  @tracked loadError: unknown = null;
  /**
   * Until when (ms since the epoch) dangerous operations work without
   * logging in again; `null` when the server does not ask for it.
   */
  @tracked reauthUntil: number | null = null;
  /** The "log in again" dialog is open. */
  @tracked reauthPrompt = false;

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
      this.reauthUntil = session.reauth_until
        ? Date.parse(session.reauth_until)
        : null;
    } catch (error) {
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

  /**
   * Whether a dangerous operation would be accepted now. Checked before a
   * plain link (a dump download) that cannot show the API's refusal.
   */
  get recentlyAuthenticated(): boolean {
    return this.reauthUntil === null || Date.now() < this.reauthUntil;
  }

  /** Asks the user to log in again (the API refused with `reauth_required`). */
  promptReauth(): void {
    this.reauthUntil = 0;
    this.reauthPrompt = true;
  }

  dismissReauth(): void {
    this.reauthPrompt = false;
  }

  /** Leaves for GitHub and comes back to the current screen. */
  reauthenticate(): void {
    this.browser.assign(loginUrl(this.router.currentURL ?? '/'));
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
