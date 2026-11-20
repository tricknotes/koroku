import { tracked } from '@glimmer/tracking';
import { assert } from '@ember/debug';
import type ApiService from 'koroku-console/services/api';
import type { App, Group } from 'koroku-console/utils/types';

/**
 * Model of the `app` route, shared by every tab: the app as last seen and a
 * way to refresh it after something changed it (deploy, start, settings).
 */
export default class AppContext {
  @tracked private current: App | null = null;
  @tracked error: unknown = null;
  /** The groups as last read; `null` until something asked for them. */
  @tracked groups: Group[] | null = null;

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

  /** The group the app belongs to, once the groups have been read. */
  get group(): Group | null {
    const id = this.current?.group_id ?? null;
    if (id === null) return null;
    return this.groups?.find((group) => group.id === id) ?? null;
  }

  /** The app is in a group whose name is not known yet. */
  get groupUnknown(): boolean {
    return (this.current?.group_id ?? null) !== null && this.group === null;
  }

  async loadGroups(): Promise<Group[]> {
    const groups = await this.api.listGroups();
    this.groups = groups;
    return groups;
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
