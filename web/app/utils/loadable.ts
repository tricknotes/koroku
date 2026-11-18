import { tracked } from '@glimmer/tracking';
import {
  errorMessage,
  unavailability,
  type Unavailability,
} from 'paas-console/utils/errors';

/**
 * Tracked state of one asynchronous read: the value, whether it is loading
 * and the error if it failed. `<AsyncView>` renders it.
 */
export default class Loadable<T> {
  @tracked value: T | null = null;
  @tracked error: unknown = null;
  @tracked loading = false;
  @tracked loaded = false;
  /**
   * The value on screen is older than it looks: the latest quiet refresh
   * failed and the previous value was kept.
   */
  @tracked stale = false;

  private sequence = 0;

  constructor(private readonly fetcher: () => Promise<T>) {}

  /**
   * Fetches the value. With `quiet` the previous value stays visible while
   * the request runs and a failure keeps it (used for polling).
   */
  async load(options: { quiet?: boolean } = {}): Promise<void> {
    const sequence = ++this.sequence;
    if (!options.quiet) this.loading = true;
    try {
      const value = await this.fetcher();
      if (sequence !== this.sequence) return;
      this.value = value;
      this.error = null;
      this.loaded = true;
      this.stale = false;
    } catch (error) {
      if (sequence !== this.sequence) return;
      if (options.quiet && this.loaded) {
        this.stale = true;
        return;
      }
      this.error = error;
    } finally {
      if (sequence === this.sequence) this.loading = false;
    }
  }

  reload = (): Promise<void> => this.load();

  /** Replaces the value without a request (after a mutation returned it). */
  set(value: T): void {
    this.sequence++;
    this.value = value;
    this.error = null;
    this.loaded = true;
    this.loading = false;
    this.stale = false;
  }

  get unavailable(): Unavailability | null {
    return unavailability(this.error);
  }

  get errorMessage(): string {
    return this.error ? errorMessage(this.error) : '';
  }
}
