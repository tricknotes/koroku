import { module, test } from 'qunit';
import { destroy } from '@ember/destroyable';
import { settled } from '@ember/test-helpers';
import { setupTest } from 'koroku-console/tests/helpers';
import type PollerService from 'koroku-console/services/poller';

/** Short enough to tick many times within a test, long enough to observe. */
const INTERVAL = 5;

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Waits for a condition; the limit only matters when the test is failing. */
async function until(condition: () => boolean, limit = 3000): Promise<void> {
  const start = performance.now();
  while (!condition()) {
    if (performance.now() - start > limit) throw new Error('timed out');
    await wait(1);
  }
}

// The one service every other test replaces with a hand-ticked fake: these
// run the real timers.
module('Unit | Service | poller', function (hooks) {
  setupTest(hooks);

  let poller: PollerService;
  let owner: object;

  hooks.beforeEach(function () {
    poller = this.owner.lookup('service:poller');
    owner = {};
  });

  hooks.afterEach(async function () {
    destroy(owner);
    await settled();
  });

  test('it calls back again and again, the first time after the interval', async function (assert) {
    let calls = 0;

    poller.every(owner, INTERVAL, () => {
      calls += 1;
    });

    assert.strictEqual(calls, 0, 'not at once: the owner has just loaded');

    await until(() => calls >= 3);

    assert.true(calls >= 3);
  });

  test('the next tick waits for a callback that is still running', async function (assert) {
    let calls = 0;
    let finish: () => void = () => {};

    poller.every(owner, INTERVAL, () => {
      calls += 1;
      return new Promise<void>((resolve) => (finish = resolve));
    });
    await until(() => calls === 1);
    await wait(INTERVAL * 6);

    assert.strictEqual(calls, 1, 'a slow request is not piled onto');

    finish();
    await until(() => calls === 2);

    assert.strictEqual(calls, 2);
  });

  test('a callback that fails is tried again on the next tick', async function (assert) {
    let calls = 0;

    poller.every(owner, INTERVAL, () => {
      calls += 1;
      if (calls === 1) throw new Error('thrown');
      if (calls === 2) return Promise.reject(new Error('rejected'));
      return undefined;
    });

    await until(() => calls >= 3);

    assert.true(calls >= 3, 'neither kind of failure stops the polling');
  });

  test('it stops when its owner is destroyed', async function (assert) {
    let calls = 0;
    poller.every(owner, INTERVAL, () => {
      calls += 1;
    });
    await until(() => calls >= 1);

    destroy(owner);
    await settled();
    const seen = calls;
    await wait(INTERVAL * 6);

    assert.strictEqual(calls, seen, 'no tick after the owner is gone');
  });

  test('an owner destroyed during a callback gets no further tick', async function (assert) {
    let calls = 0;
    let finish: () => void = () => {};
    poller.every(owner, INTERVAL, () => {
      calls += 1;
      return new Promise<void>((resolve) => (finish = resolve));
    });
    await until(() => calls === 1);

    destroy(owner);
    await settled();
    finish();
    await wait(INTERVAL * 6);

    assert.strictEqual(calls, 1);
  });

  test('each owner has its own schedule', async function (assert) {
    const other = {};
    let mine = 0;
    let theirs = 0;
    poller.every(owner, INTERVAL, () => {
      mine += 1;
    });
    poller.every(other, INTERVAL, () => {
      theirs += 1;
    });
    await until(() => mine >= 1 && theirs >= 1);

    destroy(other);
    await settled();
    const seen = theirs;
    const before = mine;
    await until(() => mine >= before + 2);

    assert.strictEqual(theirs, seen, 'the destroyed one stopped');
    assert.true(mine > before, 'the other one goes on');
  });
});
