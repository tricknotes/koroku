import Service, { service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import type Owner from '@ember/owner';
import type StorageService from './storage';

export type ThemePreference = 'system' | 'light' | 'dark';

export const THEME_STORAGE_KEY = 'koroku.theme';

function parse(value: string | null): ThemePreference {
  return value === 'light' || value === 'dark' ? value : 'system';
}

/**
 * Light / dark theme. With `system` (the default) nothing is set and the
 * stylesheet follows the OS through `prefers-color-scheme`; `light` and
 * `dark` pin the theme with `data-theme` on the root element (see
 * styles/tokens.css) and are remembered in this browser.
 *
 * The pinned theme is applied when the app boots, from its own script: the
 * console is served with `script-src 'self'`, so there is no inline script
 * to set it earlier, and a pinned theme may show the OS one for an instant
 * while the page loads.
 */
export default class ThemeService extends Service {
  @service declare storage: StorageService;

  @tracked preference: ThemePreference;

  constructor(owner: Owner) {
    super(owner);
    this.preference = parse(this.storage.get(THEME_STORAGE_KEY));
    this.apply();
  }

  select(preference: ThemePreference): void {
    this.preference = preference;
    if (preference === 'system') {
      this.storage.remove(THEME_STORAGE_KEY);
    } else {
      this.storage.set(THEME_STORAGE_KEY, preference);
    }
    this.apply();
  }

  private apply(): void {
    const root = document.documentElement;
    if (this.preference === 'system') {
      root.removeAttribute('data-theme');
    } else {
      root.setAttribute('data-theme', this.preference);
    }
  }

  willDestroy(): void {
    super.willDestroy();
    // Only matters where apps come and go on one page (the test runner).
    document.documentElement.removeAttribute('data-theme');
  }
}

declare module '@ember/service' {
  interface Registry {
    theme: ThemeService;
  }
}
