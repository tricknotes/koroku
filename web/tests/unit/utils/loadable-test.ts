import { module, test } from 'qunit';
import type ApiService from 'koroku-console/services/api';
import AppContext from 'koroku-console/utils/app-context';
import { ApiError } from 'koroku-console/utils/errors';
import Loadable from 'koroku-console/utils/loadable';
import type { App } from 'koroku-console/utils/types';
import { makeApp } from 'koroku-console/tests/helpers/fakes';

/** A request whose answer the test decides, and when. */
class Pending<T> {
  resolve: (value: T) => void = () => {};
  reject: (reason: unknown) => void = () => {};
  promise = new Promise<T>((resolve, reject) => {
    this.resolve = resolve;
    this.reject = reject;
  });
}

/** A fetcher that hands out one pending request per call, in order. */
function queue<T>(): { fetcher: () => Promise<T>; requests: Pending<T>[] } {
  const requests: Pending<T>[] = [];
  return {
    requests,
    fetcher: () => {
      const request = new Pending<T>();
      requests.push(request);
      return request.promise;
    },
  };
}

module('Unit | Utility | loadable', function () {
  test('it is loading until the first answer, then loaded', async function (assert) {
    const { fetcher, requests } = queue<string>();
    const data = new Loadable(fetcher);

    assert.false(data.loading, 'nothing is asked before load()');
    assert.false(data.loaded);
    assert.strictEqual(data.value, null);

    const loading = data.load();
    assert.true(data.loading);
    assert.false(data.loaded);

    requests[0]?.resolve('first');
    await loading;

    assert.false(data.loading);
    assert.true(data.loaded);
    assert.strictEqual(data.value, 'first');
    assert.strictEqual(data.error, null);
    assert.strictEqual(data.errorMessage, '');
    assert.false(data.stale);
  });

  test('a failed load is an error with a message, and a retry clears it', async function (assert) {
    let fail = true;
    const data = new Loadable(() =>
      fail
        ? Promise.reject(new ApiError(500, 'internal', 'boom'))
        : Promise.resolve('ok'),
    );

    await data.load();

    assert.false(data.loading, 'a failure ends the loading state too');
    assert.false(data.loaded);
    assert.true(data.error instanceof ApiError);
    assert.true(data.errorMessage.startsWith('サーバーでエラーが発生しました'));
    assert.strictEqual(data.unavailable, null);

    fail = false;
    await data.reload();

    assert.strictEqual(data.value, 'ok');
    assert.strictEqual(data.error, null);
    assert.strictEqual(data.errorMessage, '');
  });

  test('a quiet refresh that fails keeps the value and marks it stale', async function (assert) {
    let fail = false;
    let answer = 'one';
    const data = new Loadable(() =>
      fail
        ? Promise.reject(new ApiError(0, 'network_error', ''))
        : Promise.resolve(answer),
    );
    await data.load();

    fail = true;
    const refreshing = data.load({ quiet: true });
    assert.false(data.loading, 'a quiet refresh shows no loading state');
    await refreshing;

    assert.strictEqual(data.value, 'one', 'what was on screen stays');
    assert.strictEqual(data.error, null, 'and is not replaced by an error');
    assert.true(data.stale);

    fail = false;
    answer = 'two';
    await data.load({ quiet: true });

    assert.strictEqual(data.value, 'two');
    assert.false(data.stale, 'the next good answer clears the mark');
  });

  test('a quiet refresh with nothing to keep reports the error', async function (assert) {
    const data = new Loadable<string>(() =>
      Promise.reject(new ApiError(500, 'internal', '')),
    );

    await data.load({ quiet: true });

    assert.true(data.error instanceof ApiError);
    assert.false(data.stale, 'there is no older value to call stale');
    assert.false(data.loaded);
  });

  test('a loud reload that fails replaces the value on screen with the error', async function (assert) {
    let fail = false;
    const data = new Loadable(() =>
      fail
        ? Promise.reject(new ApiError(500, 'internal', ''))
        : Promise.resolve('one'),
    );
    await data.load();

    fail = true;
    await data.reload();

    assert.true(data.error instanceof ApiError);
    assert.false(data.stale);
  });

  test('an answer that arrives after a newer request is dropped', async function (assert) {
    const { fetcher, requests } = queue<string>();
    const data = new Loadable(fetcher);

    const first = data.load();
    const second = data.load();

    requests[1]?.resolve('newer');
    await second;
    assert.strictEqual(data.value, 'newer');
    assert.false(data.loading);

    requests[0]?.resolve('older');
    await first;

    assert.strictEqual(data.value, 'newer', 'the late answer does not win');
  });

  test('a failure that arrives after a newer request is dropped as well', async function (assert) {
    const { fetcher, requests } = queue<string>();
    const data = new Loadable(fetcher);

    const first = data.load();
    const second = data.load();
    assert.true(data.loading);

    requests[0]?.reject(new ApiError(500, 'internal', ''));
    await first;

    assert.strictEqual(data.error, null);
    assert.true(data.loading, 'the newer request is still on its way');

    requests[1]?.resolve('newer');
    await second;

    assert.strictEqual(data.value, 'newer');
    assert.false(data.loading);
  });

  test('set() wins over a request that is still running', async function (assert) {
    const { fetcher, requests } = queue<string>();
    const data = new Loadable(fetcher);

    const loading = data.load();
    data.set('from the mutation');

    assert.true(data.loaded);
    assert.false(data.loading);

    requests[0]?.resolve('from the older read');
    await loading;

    assert.strictEqual(data.value, 'from the mutation');
  });

  test('set() clears an error and the stale mark', async function (assert) {
    let fail = false;
    const data = new Loadable(() =>
      fail
        ? Promise.reject(new ApiError(500, 'internal', ''))
        : Promise.resolve('one'),
    );
    await data.load();
    fail = true;
    await data.load({ quiet: true });
    assert.true(data.stale);

    data.set('two');

    assert.false(data.stale);
    assert.strictEqual(data.error, null);
    assert.strictEqual(data.value, 'two');
  });

  test('unavailable tells a missing feature from a failure', async function (assert) {
    const failing = (error: Error) => {
      const data = new Loadable<string>(() => Promise.reject(error));
      return data.load().then(() => data.unavailable);
    };

    assert.strictEqual(
      await failing(new ApiError(503, 'not_configured', '')),
      'not_configured',
    );
    assert.strictEqual(
      await failing(new ApiError(404, 'not_found', '')),
      'unavailable',
    );
    assert.strictEqual(
      await failing(new ApiError(404, 'app_not_found', '')),
      null,
    );
    assert.strictEqual(await failing(new ApiError(500, 'internal', '')), null);
  });
});

module('Unit | Utility | app-context', function () {
  /** Only `getApp` is used by the context. */
  function apiReturning(answer: () => Promise<App>): ApiService {
    return { getApp: () => answer() } as unknown as ApiService;
  }

  test('it holds the app once loaded', async function (assert) {
    const context = new AppContext(
      apiReturning(() => Promise.resolve(makeApp())),
      'blog',
    );

    assert.false(context.loaded);
    assert.false(context.isProduction, 'unknown before it is loaded');

    await context.load();

    assert.true(context.loaded);
    assert.strictEqual(context.app.slug, 'blog');
    assert.strictEqual(context.error, null);
    assert.false(context.isProduction);
  });

  test('a first load that fails keeps the error for the page to show', async function (assert) {
    const context = new AppContext(
      apiReturning(() =>
        Promise.reject(new ApiError(404, 'app_not_found', '')),
      ),
      'ghost',
    );

    await context.load();

    assert.false(context.loaded);
    assert.true(context.error instanceof ApiError);
    assert.strictEqual(context.slug, 'ghost');
  });

  test('a reload that fails keeps the app that was on screen', async function (assert) {
    let fail = false;
    const context = new AppContext(
      apiReturning(() =>
        fail
          ? Promise.reject(new ApiError(500, 'internal', ''))
          : Promise.resolve(makeApp({ environment: 'production' })),
      ),
      'blog',
    );
    await context.load();

    fail = true;
    await context.reload();

    assert.true(context.loaded);
    assert.true(context.isProduction, 'still known to be production');
    assert.strictEqual(context.error, null, 'and no error page replaces it');
  });

  test('set() replaces the app and clears an earlier error', async function (assert) {
    const context = new AppContext(
      apiReturning(() => Promise.reject(new ApiError(500, 'internal', ''))),
      'blog',
    );
    await context.load();
    assert.true(context.error instanceof ApiError);

    context.set(makeApp({ environment: 'production' }));

    assert.true(context.loaded);
    assert.strictEqual(context.error, null);
    assert.true(context.isProduction);
  });
});
