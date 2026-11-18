import { tracked } from '@glimmer/tracking';
import { cancel, later } from '@ember/runloop';

type Timer = ReturnType<typeof later>;

const FLUSH_DELAY = 50;

/**
 * Collects log lines from a stream and publishes them as one tracked string
 * at most every few milliseconds, so a burst of lines renders once. Keeps
 * only the newest `limit` lines.
 */
export default class LineBuffer {
  @tracked text = '';
  @tracked dropped = false;

  private lines: string[] = [];
  private timer: Timer | null = null;

  constructor(private readonly limit = 5000) {}

  push(line: string): void {
    this.lines.push(line);
    if (this.lines.length > this.limit) {
      this.lines.splice(0, this.lines.length - this.limit);
      this.dropped = true;
    }
    // A run loop timer, so tests' `settled()` waits for the flush.
    // eslint-disable-next-line ember/no-runloop
    this.timer ??= later(() => this.flush(), FLUSH_DELAY);
  }

  flush(): void {
    this.stop();
    this.text = this.lines.join('\n');
  }

  clear(): void {
    this.stop();
    this.lines = [];
    this.text = '';
    this.dropped = false;
  }

  /** Drops a pending flush (call when the owner goes away). */
  stop(): void {
    if (this.timer !== null) {
      // eslint-disable-next-line ember/no-runloop
      cancel(this.timer);
      this.timer = null;
    }
  }
}
