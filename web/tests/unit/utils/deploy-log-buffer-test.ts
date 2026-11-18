import { module, test } from 'qunit';
import { settled } from '@ember/test-helpers';
import { createCache, getValue } from '@glimmer/tracking/primitives/cache';
import DeployLogBuffer from 'koroku-console/utils/deploy-log-buffer';

const HEADLINE = '==> 2026-11-18T03:00:01Z build Building images';

/**
 * What a template sees: the number of lines as of the last publish. It only
 * changes when the buffer says its lines changed.
 */
function rendered(buffer: DeployLogBuffer): () => number {
  const cache = createCache(() => buffer.lines.length);
  return () => getValue(cache) as number;
}

module('Unit | Utility | deploy-log-buffer', function () {
  test('a burst of lines is published once, a moment after it arrives', async function (assert) {
    const buffer = new DeployLogBuffer();
    const count = rendered(buffer);
    assert.strictEqual(count(), 0);

    buffer.push('one');
    buffer.push('two');

    assert.strictEqual(count(), 0, 'nothing renders per line');

    await settled();

    assert.strictEqual(count(), 2);
    assert.deepEqual([...buffer.lines], ['one', 'two']);
    assert.false(buffer.dropped);
  });

  test('flush() publishes at once, for the end of a stream', function (assert) {
    const buffer = new DeployLogBuffer();
    const count = rendered(buffer);
    assert.strictEqual(count(), 0);

    buffer.push('last line');
    buffer.flush();

    assert.strictEqual(count(), 1);
  });

  test('a log past the limit loses its oldest ordinary lines, never a headline', function (assert) {
    // The buffer lets 1000 lines pile up past its limit, then trims back.
    const limit = 10;
    const buffer = new DeployLogBuffer(limit);

    buffer.push(HEADLINE);
    for (let i = 0; i < limit + 999; i++) buffer.push(`line ${i}`);
    assert.false(buffer.dropped, 'nothing is dropped up to the slack');
    assert.strictEqual(buffer.lines.length, limit + 1000);

    buffer.push('the newest line');
    buffer.flush();

    assert.true(buffer.dropped);
    assert.strictEqual(buffer.lines.length, limit);
    assert.strictEqual(buffer.lines[0], HEADLINE, 'the step still has a start');
    assert.strictEqual(buffer.lines[1], 'line 1001', 'the oldest lines went');
    assert.strictEqual(buffer.lines[limit - 1], 'the newest line');
  });

  test('headlines are kept even when they alone pass the limit', function (assert) {
    const buffer = new DeployLogBuffer(2);

    for (let i = 0; i < 1003; i++) buffer.push(HEADLINE);
    buffer.flush();

    assert.strictEqual(buffer.lines.length, 1003);
  });

  test('clear() starts over, for a reconnect', function (assert) {
    const buffer = new DeployLogBuffer(1);
    const count = rendered(buffer);
    for (let i = 0; i < 1002; i++) buffer.push(`line ${i}`);
    buffer.flush();
    assert.true(buffer.dropped);
    assert.strictEqual(count(), 1);

    buffer.clear();

    assert.strictEqual(count(), 0);
    assert.false(buffer.dropped);
  });

  test('stop() drops the pending publish when the owner goes away', async function (assert) {
    const buffer = new DeployLogBuffer();
    const count = rendered(buffer);
    assert.strictEqual(count(), 0);

    buffer.push('late');
    buffer.stop();
    await settled();

    assert.strictEqual(count(), 0, 'nothing is published after stop()');
  });
});
