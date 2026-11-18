import { tracked } from '@glimmer/tracking';
import { assert } from '@ember/debug';
import type ApiService from 'koroku-console/services/api';
import type { App } from 'koroku-console/utils/types';

/**
 * Model of the `app` route, shared by every tab: the app as last seen and a
 * way to refresh it after something changed it (deploy, start, settings).
 */
export default class AppContext {
  @tracked private current: App | null = null;
  @tracked error: unknown = null;

  constructor(
    private readonly api: ApiService,
    readonly slug: string,
  ) {}

  get loaded(): boolean {
    return this.current !== null;
  }

  get app(): App {
    assert('the app is loaded before a tab renders', this.current !== null);
    return this.current;
  }

  get isProduction(): boolean {
    return this.current?.environment === 'production';
  }

  async load(): Promise<void> {
    try {
      this.current = await this.api.getApp(this.slug);
      this.error = null;
    } catch (error) {
      if (this.current === null) this.error = error;
    }
  }

  reload = (): Promise<void> => this.load();

  set(app: App): void {
    this.current = app;
    this.error = null;
  }
}
