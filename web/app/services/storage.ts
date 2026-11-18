import Service from '@ember/service';

/**
 * Small per-browser preferences (`localStorage`), behind a service so tests
 * can register an in-memory fake. Storage can be unavailable (private mode,
 * blocked by policy): reads then answer `null` and writes are dropped.
 */
export default class StorageService extends Service {
  get(key: string): string | null {
    try {
      return window.localStorage.getItem(key);
    } catch {
      return null;
    }
  }

  set(key: string, value: string): void {
    try {
      window.localStorage.setItem(key, value);
    } catch {
      // The preference then lasts until the page is closed.
    }
  }

  remove(key: string): void {
    try {
      window.localStorage.removeItem(key);
    } catch {
      // Nothing was stored.
    }
  }
}

declare module '@ember/service' {
  interface Registry {
    storage: StorageService;
  }
}
