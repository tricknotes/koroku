import Service from '@ember/service';

/**
 * Things only the browser can do for the page: navigation that leaves the
 * SPA (downloads, full reloads) and the clipboard. Kept behind a service so
 * tests can observe them instead of leaving the test page.
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

  /** Puts text on the clipboard; rejects when the browser refuses. */
  async copy(text: string): Promise<void> {
    await navigator.clipboard.writeText(text);
  }
}

declare module '@ember/service' {
  interface Registry {
    browser: BrowserService;
  }
}
