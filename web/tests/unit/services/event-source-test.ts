import { module, test } from 'qunit';
import { setupTest } from 'koroku-console/tests/helpers';
import type EventSourceService from 'koroku-console/services/event-source';

type Listener = (event: { data: string }) => void;

/** Stands in for the browser's `EventSource`, driven by the test. */
class StubEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;
  static instances: StubEventSource[] = [];

  readyState = StubEventSource.CONNECTING;
  onmessage: Listener | null = null;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  named = new Map<string, Listener>();

  constructor(readonly url: string) {
    StubEventSource.instances.push(this);
  }

  addEventListener(type: string, listener: Listener): void {
    this.named.set(type, listener);
  }

  close(): void {
    this.readyState = StubEventSource.CLOSED;
  }
}

// Every other test replaces this service with a fake that emits by hand:
// these check the wiring to the browser's own EventSource.
module('Unit | Service | event-source', function (hooks) {
  setupTest(hooks);

  let original: typeof EventSource;
  let service: EventSourceService;

  hooks.beforeEach(function () {
    original = window.EventSource;
    StubEventSource.instances = [];
    window.EventSource = StubEventSource as unknown as typeof EventSource;
    service = this.owner.lookup('service:event-source');
  });

  hooks.afterEach(function () {
    window.EventSource = original;
  });

  function source(): StubEventSource {
    const stub = StubEventSource.instances[0];
    if (!stub) throw new Error('no EventSource was created');
    return stub;
  }

  test('it connects to the URL as given and passes messages on', function (assert) {
    const messages: string[] = [];
    let opened = 0;

    service.open('/api/deployments/42/logs', {
      message: (data) => messages.push(data),
      open: () => (opened += 1),
    });

    assert.strictEqual(StubEventSource.instances.length, 1);
    assert.strictEqual(source().url, '/api/deployments/42/logs');

    source().onopen?.();
    source().onmessage?.({ data: 'Building images' });
    source().onmessage?.({ data: '' });

    assert.strictEqual(opened, 1);
    assert.deepEqual(messages, ['Building images', ''], 'empty lines count');
  });

  test("the server's `end` event is told apart from a log line", function (assert) {
    const messages: string[] = [];
    const ends: string[] = [];

    service.open('/api/deployments/42/logs', {
      message: (data) => messages.push(data),
      end: (data) => ends.push(data),
    });
    source().named.get('end')?.({ data: 'succeeded' });

    assert.deepEqual(ends, ['succeeded']);
    assert.deepEqual(messages, [], 'the final status is not a line of the log');
  });

  test('an error says whether the browser gave up or reconnects', function (assert) {
    const errors: boolean[] = [];
    service.open('/api/apps/blog/logs', {
      message: () => {},
      error: (closed) => errors.push(closed),
    });

    // A dropped connection: the browser is reconnecting by itself.
    source().readyState = StubEventSource.CONNECTING;
    source().onerror?.();
    // A refused one (a non-200 answer): it will not try again.
    source().readyState = StubEventSource.CLOSED;
    source().onerror?.();

    assert.deepEqual(errors, [false, true]);
  });

  test('close() closes the connection, and handlers may be left out', function (assert) {
    const stream = service.open('/api/apps/blog/logs', { message: () => {} });

    // No `open`, `end` or `error` handler was given: none of these throws.
    source().onopen?.();
    source().named.get('end')?.({ data: 'failed' });
    source().onerror?.();
    assert.strictEqual(source().readyState, StubEventSource.CONNECTING);

    stream.close();

    assert.strictEqual(source().readyState, StubEventSource.CLOSED);
  });
});
