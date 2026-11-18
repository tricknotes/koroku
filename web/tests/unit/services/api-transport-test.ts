import { module, test } from 'qunit';
import Service from '@ember/service';
import { setupTest } from 'koroku-console/tests/helpers';
import type ApiService from 'koroku-console/services/api';
import { ApiError, errorMessage } from 'koroku-console/utils/errors';

interface Sent {
  url: string;
  init: RequestInit;
}

class StubSession extends Service {
  invalidations = 0;

  invalidate(): void {
    this.invalidations += 1;
  }
}

function json(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

async function failure(promise: Promise<unknown>): Promise<unknown> {
  try {
    await promise;
  } catch (error) {
    return error;
  }
  throw new Error('the request was expected to fail');
}

// The acceptance tests replace `transport` with a fake. These run the real
// one against a stubbed `fetch`: how a request is put on the wire and how
// each kind of answer comes back.
module('Unit | Service | api (transport)', function (hooks) {
  setupTest(hooks);

  let originalFetch: typeof fetch;
  let sent: Sent[];
  let respond: (sent: Sent) => Response | Promise<Response>;
  let api: ApiService;
  let session: StubSession;

  hooks.beforeEach(function () {
    // Put back as it was found, never called through this reference.
    // eslint-disable-next-line @typescript-eslint/unbound-method
    originalFetch = window.fetch;
    sent = [];
    respond = () => json(200, {});
    window.fetch = (input: RequestInfo | URL, init: RequestInit = {}) => {
      const url = input instanceof Request ? input.url : input.toString();
      const request = { url, init };
      sent.push(request);
      return Promise.resolve().then(() => respond(request));
    };

    this.owner.register('service:session', StubSession);
    session = this.owner.lookup('service:session') as unknown as StubSession;
    api = this.owner.lookup('service:api');
  });

  hooks.afterEach(function () {
    window.fetch = originalFetch;
  });

  function headers(request: Sent | undefined): Record<string, string> {
    return (request?.init.headers ?? {}) as Record<string, string>;
  }

  test('a read is a GET for JSON with the session cookie and no body', async function (assert) {
    respond = () => json(200, { apps: [{ slug: 'blog' }] });

    const apps = await api.listApps();

    assert.strictEqual(apps[0]?.slug, 'blog');
    assert.strictEqual(sent.length, 1);
    assert.strictEqual(sent[0]?.url, '/api/apps');
    assert.strictEqual(sent[0]?.init.method, 'GET');
    assert.strictEqual(sent[0]?.init.credentials, 'same-origin');
    assert.strictEqual(headers(sent[0])['Accept'], 'application/json');
    assert.strictEqual(sent[0]?.init.body, undefined);
    assert.strictEqual(headers(sent[0])['Content-Type'], undefined);
  });

  test('a change sends one JSON object, also when there is nothing to say', async function (assert) {
    respond = () => json(202, { id: 43 });

    await api.createDeployment('blog', { ref: 'main' });
    await api.cancelDeployment(43);

    assert.strictEqual(sent[0]?.init.method, 'POST');
    assert.strictEqual(headers(sent[0])['Content-Type'], 'application/json');
    assert.strictEqual(sent[0]?.init.body, '{"ref":"main"}');
    // The API refuses a mutation whose body is not JSON (415).
    assert.strictEqual(headers(sent[1])['Content-Type'], 'application/json');
    assert.strictEqual(sent[1]?.init.body, '{}');
  });

  test('a 204 and an empty body are answers without a value', async function (assert) {
    respond = () => new Response(null, { status: 204 });
    assert.strictEqual(await api.logout(), undefined);

    respond = () => new Response('', { status: 200 });
    assert.strictEqual(await api.request('GET', '/api/anything'), undefined);
  });

  test('an error answer becomes an ApiError with its code and details', async function (assert) {
    respond = () =>
      json(422, {
        error: {
          code: 'validation_failed',
          message: 'some fields are invalid',
          details: { web_port: 'must be between 1 and 65535' },
        },
      });

    const error = await failure(api.updateApp('blog', { web_port: 0 }));

    assert.true(error instanceof ApiError);
    const apiError = error as ApiError;
    assert.strictEqual(apiError.status, 422);
    assert.strictEqual(apiError.code, 'validation_failed');
    assert.deepEqual(apiError.details, {
      web_port: 'must be between 1 and 65535',
    });
  });

  test('an error page that is not from the API is still an ApiError', async function (assert) {
    respond = () =>
      new Response('<html>Bad Gateway</html>', {
        status: 502,
        headers: { 'Content-Type': 'text/html' },
      });

    const error = await failure(api.getSystem());

    assert.true(error instanceof ApiError);
    assert.strictEqual((error as ApiError).status, 502);
    assert.strictEqual((error as ApiError).code, 'unknown');
    assert.false(
      errorMessage(error).includes('<html>'),
      'the page itself is not shown to the reader',
    );
  });

  test('a request that never reaches the server is a network error', async function (assert) {
    respond = () => {
      throw new TypeError('Failed to fetch');
    };

    const error = await failure(api.listApps());

    assert.true(error instanceof ApiError);
    assert.strictEqual((error as ApiError).status, 0);
    assert.strictEqual((error as ApiError).code, 'network_error');
    assert.strictEqual(errorMessage(error), 'サーバーに接続できませんでした。');
    assert.strictEqual(session.invalidations, 0, 'the session is kept');
  });

  test('a 401 ends the session, whatever was being asked', async function (assert) {
    respond = () =>
      json(401, { error: { code: 'unauthenticated', message: 'log in' } });

    const read = await failure(api.getApp('blog'));
    const change = await failure(api.putEnv('blog', []));

    assert.strictEqual((read as ApiError).code, 'unauthenticated');
    assert.strictEqual((change as ApiError).status, 401);
    assert.strictEqual(session.invalidations, 2);
  });

  test('other refusals leave the session alone', async function (assert) {
    for (const [status, code] of [
      [403, 'forbidden_origin'],
      [404, 'app_not_found'],
      [409, 'busy'],
      [500, 'internal'],
      [503, 'not_configured'],
    ] as const) {
      respond = () => json(status, { error: { code, message: '' } });
      const error = await failure(api.getApp('blog'));
      assert.strictEqual((error as ApiError).code, code);
    }

    assert.strictEqual(session.invalidations, 0);
  });

  test('names and filters are encoded into the URL', async function (assert) {
    respond = () => json(200, {});

    await api.getApp('a b/c');
    await api.listCommits('octo/blog', 'feature/search');
    await api.deleteApp('blog', { volumes: true });
    await api.deleteApp('blog', { volumes: false });
    await api.searchLogs('blog', { service: '', q: 'a&b=c', limit: 1000 });

    assert.deepEqual(
      sent.map((request) => request.url),
      [
        '/api/apps/a%20b%2Fc',
        '/api/repos/octo/blog/commits?ref=feature%2Fsearch',
        '/api/apps/blog?volumes=true',
        '/api/apps/blog',
        '/api/apps/blog/logs/history?q=a%26b%3Dc&limit=1000',
      ],
    );
  });

  test('environment values travel in the body only', async function (assert) {
    respond = () => json(200, { vars: [] });

    await api.putEnv('blog', [{ key: 'DATABASE_URL', value: 'hunter2' }]);

    assert.strictEqual(sent[0]?.url, '/api/apps/blog/env');
    assert.strictEqual(sent[0]?.init.method, 'PUT');
    assert.strictEqual(
      sent[0]?.init.body,
      '{"vars":[{"key":"DATABASE_URL","value":"hunter2"}]}',
    );
    assert.false(JSON.stringify(headers(sent[0])).includes('hunter2'));
  });
});
