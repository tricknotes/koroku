import { tracked } from '@glimmer/tracking';
import { cancel, later } from '@ember/runloop';
import { isHeadline } from './deploy-steps';

type Timer = ReturnType<typeof later>;

const FLUSH_DELAY = 50;
/** How many lines are dropped at once when the limit is passed. */
const SLACK = 1000;

/**
 * Collects the lines of a deployment log as they stream in and publishes
 * them in batches, so a burst of lines renders once. When the log outgrows
 * `limit`, the oldest ordinary lines go first: the step headlines are kept,
 * since the step list is built from them.
 */
export default class DeployLogBuffer {
  /** Bumped on every publish; read `lines` after reading it. */
  @tracked private version = 0;
  @tracked dropped = false;

  private all: string[] = [];
  private timer: Timer | null = null;

  constructor(private readonly limit = 20000) {}

  /** The published lines. Treat as read-only. */
  get lines(): readonly string[] {
    void this.version;
    return this.all;
  }

  push(line: string): void {
    this.all.push(line);
    if (this.all.length > this.limit + SLACK) this.trim();
    // A run loop timer, so tests' `settled()` waits for the flush.
    // eslint-disable-next-line ember/no-runloop
    this.timer ??= later(() => this.flush(), FLUSH_DELAY);
  }

  flush(): void {
    this.stop();
    this.version++;
  }

  clear(): void {
    this.stop();
    this.all = [];
    this.dropped = false;
    this.version++;
  }

  /** Drops a pending flush (call when the owner goes away). */
  stop(): void {
    if (this.timer !== null) {
      // eslint-disable-next-line ember/no-runloop
      cancel(this.timer);
      this.timer = null;
    }
  }

  private trim(): void {
    let excess = this.all.length - this.limit;
    this.all = this.all.filter((line) => {
      if (excess <= 0 || isHeadline(line)) return true;
      excess--;
      return false;
    });
    this.dropped = true;
  }
}
