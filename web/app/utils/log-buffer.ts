import { tracked } from '@glimmer/tracking';
import { cancel, later } from '@ember/runloop';
import { formatLogStamp } from 'koroku-console/utils/logs';
import type { LogLine } from 'koroku-console/utils/types';

type Timer = ReturnType<typeof later>;

const FLUSH_DELAY = 100;

/** A log line as the list renders it. */
export interface LogRow {
  /** Stable key of the row for as long as the buffer lives. */
  id: number;
  service: string;
  /** The time as the API sent it (RFC 3339, up to nanoseconds). */
  time: string;
  /** The time column, formatted once on arrival. */
  stamp: string;
  line: string;
}

/**
 * The rows of the log list. Lines from a stream are collected and published
 * together a few times a second, so a burst renders once, and only the
 * newest `limit` rows are kept (that is the cap on the DOM). While the
 * buffer is held, arriving lines wait and the rows stay as they are: the
 * reader has scrolled up and nothing moves under them.
 */
export default class LogBuffer {
  @tracked rows: LogRow[] = [];
  /** Older rows were dropped to stay within the limit. */
  @tracked dropped = false;
  /** Lines that arrived while held and are not shown yet. */
  @tracked waiting = 0;
  /** Service names seen so far, in order of first appearance. */
  @tracked services: string[] = [];

  private queue: LogRow[] = [];
  private timer: Timer | null = null;
  private held = false;
  private nextId = 1;

  constructor(readonly limit: number) {}

  /** Queues one line from a stream. */
  push(line: LogLine): void {
    this.queue.push(this.row(line));
    if (this.queue.length > this.limit) {
      this.queue.splice(0, this.queue.length - this.limit);
      if (!this.dropped) this.dropped = true;
    }
    // A run loop timer, so tests' `settled()` waits for the flush.
    // eslint-disable-next-line ember/no-runloop
    this.timer ??= later(() => this.flush(), FLUSH_DELAY);
  }

  /** Publishes the queued lines (or just their count, while held). */
  flush(): void {
    this.stop();
    if (this.held) {
      this.waiting = this.queue.length;
      return;
    }
    if (this.queue.length === 0) return;
    this.publish([...this.rows, ...this.queue]);
    this.queue = [];
    this.waiting = 0;
  }

  /** Keeps arriving lines back until `release()`. */
  hold(): void {
    this.held = true;
  }

  release(): void {
    this.held = false;
    this.flush();
  }

  /** Replaces everything with the given lines (a search result). */
  replace(lines: LogLine[]): void {
    this.clear();
    this.publish(lines.map((line) => this.row(line)));
  }

  /** Adds lines at the end at once (the next page of a search). */
  append(lines: LogLine[]): void {
    this.publish([...this.rows, ...lines.map((line) => this.row(line))]);
  }

  clear(): void {
    this.stop();
    this.queue = [];
    this.rows = [];
    this.dropped = false;
    this.waiting = 0;
  }

  /** Drops a pending flush (call when the owner goes away). */
  stop(): void {
    if (this.timer !== null) {
      // eslint-disable-next-line ember/no-runloop
      cancel(this.timer);
      this.timer = null;
    }
  }

  private publish(rows: LogRow[]): void {
    if (rows.length > this.limit) {
      rows = rows.slice(rows.length - this.limit);
      this.dropped = true;
    }
    this.rows = rows;
  }

  private row(line: LogLine): LogRow {
    if (line.service && !this.services.includes(line.service)) {
      this.services = [...this.services, line.service];
    }
    return {
      id: this.nextId++,
      service: line.service,
      time: line.time,
      stamp: formatLogStamp(line.time),
      line: line.line,
    };
  }
}
