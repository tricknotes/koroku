import { module, test } from 'qunit';
import { click, currentURL, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  SCREENS,
  makeBusy,
  serveConsole,
  type Console,
} from 'koroku-console/tests/helpers/console';
import {
  installFakes,
  makeApp,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';

// What a screen starts (polling, log streams) has to end when the reader
// leaves it: a console left open in a tab must not keep asking for a screen
// that is gone, or hold a stream of a tab that was closed.
module('Acceptance | lifecycle', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let state: Console;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    state = serveConsole(fakes);
    // The busiest state: every poller has a reason to ask.
    makeBusy(state);
  });

  /** Lets a polling interval pass and reports what was requested. */
  async function requestsOfOneTick(): Promise<string[]> {
    fakes.api.calls = [];
    await fakes.poller.tick();
    await settled();
    return fakes.api.calls.map((call: FakeCall) => call.path);
  }

  function openStreams(): string[] {
    return fakes.eventSource.streams
      .filter((stream) => !stream.closed)
      .map((stream) => stream.url);
  }

  for (const screen of SCREENS) {
    test(`${screen.name}: nothing is polled or streamed after leaving`, async function (assert) {
      // The form to create an app and the system's admin tab (members, the
      // GitHub App) have nothing to follow; everywhere else a tick asks for
      // something.
      const follows =
        screen.url !== '/apps/new' && screen.url !== '/system/admin';

      await visit(screen.url);
      const polled = await requestsOfOneTick();

      assert.strictEqual(
        polled.length > 0,
        follows,
        `while it is open: ${polled.join(', ') || 'no request'}`,
      );

      await visit('/nope');

      assert.dom('[data-test-not-found]').exists();
      assert.deepEqual(await requestsOfOneTick(), [], 'no request');
      assert.deepEqual(openStreams(), [], 'no open stream');
    });
  }

  test('the app list polls the apps and the host, and nothing else', async function (assert) {
    await visit('/');

    assert.deepEqual((await requestsOfOneTick()).sort(), [
      '/api/apps',
      '/api/backups',
      '/api/system',
    ]);
  });

  test('switching tabs hands the polling over to the new tab', async function (assert) {
    await visit('/apps/blog/deploy');
    const onDeploy = await requestsOfOneTick();
    assert.true(onDeploy.includes('/api/apps/blog/deployments'));
    assert.deepEqual(openStreams(), ['/api/deployments/43/logs']);

    await click('[data-test-tab="settings"]');
    const onSettings = await requestsOfOneTick();

    assert.deepEqual(openStreams(), [], 'the deploy log is let go');
    assert.false(
      onSettings.includes('/api/apps/blog/deployments'),
      'the history is no longer followed',
    );
    assert.deepEqual(
      onSettings.sort(),
      ['/api/apps/blog', '/api/apps/blog/domains'],
      'the header and the pending domain of the 公開 section are',
    );

    await click('[data-test-tab="env"]');

    assert.deepEqual(
      await requestsOfOneTick(),
      ['/api/apps/blog'],
      'a tab with nothing to follow leaves only the header',
    );
  });

  test('closing a deployment row closes its log stream', async function (assert) {
    await visit('/apps/blog/deploy');
    assert.deepEqual(openStreams(), ['/api/deployments/43/logs']);

    await click('[data-test-deployment="43"] [data-test-show-log]');

    assert.dom('[data-test-log]').doesNotExist();
    assert.deepEqual(openStreams(), []);

    await click('[data-test-deployment="42"] [data-test-show-log]');
    await click('[data-test-deployment="43"] [data-test-show-log]');

    assert.deepEqual(
      openStreams(),
      ['/api/deployments/43/logs'],
      'one stream at a time: opening another row closes the first',
    );
  });

  test('going straight to another app stops following the first', async function (assert) {
    fakes.api
      .on('GET', '/api/apps/docs', makeApp({ slug: 'docs' }))
      .on('GET', '/api/apps/docs/deployments', { deployments: [] });
    await visit('/apps/blog/deploy');

    await visit('/apps/docs/deploy');
    const paths = await requestsOfOneTick();

    assert.strictEqual(currentURL(), '/apps/docs/deploy');
    assert.deepEqual(openStreams(), []);
    assert.deepEqual(
      paths.filter((path) => path.startsWith('/api/apps/blog')),
      [],
      'nothing is asked about the app that was left',
    );
    assert.true(paths.includes('/api/apps/docs'), 'the new one is followed');
  });

  test('a live tail is closed on every way out of it', async function (assert) {
    await visit('/apps/blog/logs');
    assert.strictEqual(openStreams().length, 1);
    assert.true(openStreams()[0]?.startsWith('/api/apps/blog/logs?'));

    await click('[data-test-nav="system"]');
    assert.deepEqual(openStreams(), [], 'to another section');

    await visit('/apps/blog/logs');
    assert.strictEqual(openStreams().length, 1);

    await click('[data-test-wordmark]');
    assert.deepEqual(openStreams(), [], 'to the app list');
    assert.strictEqual(
      fakes.eventSource.streams.length,
      2,
      'and coming back opened exactly one new stream',
    );
  });

  test('logging out stops everything', async function (assert) {
    fakes.api.on('POST', '/auth/logout', undefined);
    await visit('/apps/blog/logs');
    assert.strictEqual(openStreams().length, 1);

    await click('[data-test-user-menu]');
    await click('[data-test-logout]');

    assert.strictEqual(currentURL(), '/login');
    assert.deepEqual(openStreams(), []);
    assert.deepEqual(await requestsOfOneTick(), []);
  });
});
