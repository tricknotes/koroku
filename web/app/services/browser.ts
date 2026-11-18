import Service from '@ember/service';

/**
 * Page-level navigation that leaves the SPA (downloads, full reloads), kept
 * behind a service so tests can observe it instead of leaving the test page.
 */
export default class BrowserService extends Service {
  /** Query string of the page as it was loaded (`?login_error=...`). */
  get search(): string {
    return window.location.search;
  }

  assign(url: string): void {
    window.location.assign(url);
  }

  reload(): void {
    window.location.reload();
  }
}

declare module '@ember/service' {
  interface Registry {
    browser: BrowserService;
  }
}
