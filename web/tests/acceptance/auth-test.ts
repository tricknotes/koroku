import { module, test } from 'qunit';
import {
  click,
  currentURL,
  find,
  settled,
  visit,
  waitUntil,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';

module('Acceptance | auth', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    fakes.api.on('GET', '/api/apps', { apps: [makeApp()] });
  });

  test('an unauthenticated visitor gets the login page', async function (assert) {
    fakes.api.on('GET', '/api/session', { authenticated: false });

    await visit('/');

    assert.strictEqual(currentURL(), '/login');
    assert
      .dom('[data-test-login-link]')
      .hasText('GitHub でログイン')
      .hasAttribute('href', '/auth/github/login');
    assert.dom('[data-test-login-error]').doesNotExist();
    assert.dom('[data-test-nav]').doesNotExist();
    assert.deepEqual(
      fakes.api.callsTo('GET', '/api/apps'),
      [],
      'no protected API is called before login',
    );
  });

  test('deep links also end up on the login page', async function (assert) {
    fakes.api.on('GET', '/api/session', { authenticated: false });

    await visit('/apps/blog/env');

    assert.strictEqual(currentURL(), '/login');
    assert.deepEqual(fakes.api.callsTo('GET', '/api/apps/blog'), []);
  });

  test('the reason of a refused login is shown', async function (assert) {
    fakes.api.on('GET', '/api/session', { authenticated: false });

    await visit('/?login_error=not_allowed');

    assert.strictEqual(currentURL(), '/login');
    assert
      .dom('[data-test-login-error]')
      .hasText('このアカウントはログインを許可されていません。')
      .hasAttribute('role', 'alert');
    assert
      .dom('[data-test-login-hint]')
      .includesText('別のアカウント', 'it says what to do about it');
    assert
      .dom('[data-test-login-link]')
      .exists({ count: 1 }, 'and still offers the one button');
  });

  test('the login error is also read from the page URL', async function (assert) {
    fakes.api.on('GET', '/api/session', { authenticated: false });
    fakes.browser.query = '?login_error=state_mismatch';

    await visit('/');

    assert.dom('[data-test-login-error]').includesText('もう一度やり直して');
    assert.dom('[data-test-login-hint]').doesNotExist();
  });

  test('a 401 from any API call shows the login page', async function (assert) {
    fakes.api.fail('GET', '/api/apps', 401, 'unauthenticated');

    await visit('/');

    assert.strictEqual(currentURL(), '/login');
    assert.dom('[data-test-login-link]').exists();
    assert.dom('[data-test-user]').doesNotExist();
  });

  test('a session that ends while a screen is open leads to the login page', async function (assert) {
    await visit('/');
    assert.dom('[data-test-app="blog"]').exists();
    assert.dom('[data-test-user]').hasText('octo');

    // The cookie expired (or was revoked); the next refresh is refused.
    fakes.api.fail('GET', '/api/apps', 401, 'unauthenticated');
    await fakes.poller.tick();
    await settled();

    assert.strictEqual(currentURL(), '/login');
    assert.dom('[data-test-login-link]').exists();
    assert.dom('[data-test-user-menu]').doesNotExist();
    assert.dom('[data-test-nav]').doesNotExist();
    assert.dom('[data-test-app]').doesNotExist('nothing of the console stays');
    assert
      .dom('[data-test-session-error], [data-test-login-error]')
      .doesNotExist('an expired session is not an error to report');

    fakes.api.calls = [];
    await fakes.poller.tick();
    await settled();

    assert.deepEqual(fakes.api.calls, [], 'and the polling has stopped');
  });

  test('a session that ends during an action leads to the login page too', async function (assert) {
    const running = makeDeployment();
    fakes.api
      .on(
        'GET',
        '/api/apps/blog',
        makeApp({ current_deployment: running, latest_deployment: running }),
      )
      .fail('POST', '/api/apps/blog/restart', 401, 'unauthenticated');
    await visit('/apps/blog/settings');

    await click('[data-test-control="restart"]');

    assert.strictEqual(currentURL(), '/login');
    assert.dom('[data-test-login-link]').exists();
    assert.dom('[data-test-app-detail]').doesNotExist();
  });

  test('a 401 on a deep link shows the login page, not "app not found"', async function (assert) {
    fakes.api.fail('GET', '/api/apps/blog', 401, 'unauthenticated');

    await visit('/apps/blog/env');

    assert.strictEqual(currentURL(), '/login');
    assert.dom('[data-test-app-error]').doesNotExist();
    assert.dom('[data-test-login-link]').exists();
  });

  test('logging out sends one request and locks the button meanwhile', async function (assert) {
    const held = fakes.api.hold('POST', '/auth/logout');
    await visit('/');
    await click('[data-test-user-menu]');

    const leaving = click('[data-test-logout]');
    await waitUntil(() => find('[data-test-logout]:disabled'));
    (find('[data-test-logout]') as HTMLButtonElement).click();

    assert.strictEqual(currentURL(), '/', 'still here until it is done');

    held.release();
    await leaving;

    assert.strictEqual(fakes.api.callsTo('POST', '/auth/logout').length, 1);
    assert.strictEqual(currentURL(), '/login');
  });

  test('a logout that fails says so and keeps the session', async function (assert) {
    fakes.api.fail('POST', '/auth/logout', 0, 'network_error');
    await visit('/');
    await click('[data-test-user-menu]');

    await click('[data-test-logout]');

    assert.strictEqual(currentURL(), '/');
    assert
      .dom('[data-test-error]')
      .hasAttribute('role', 'alert')
      .hasText('サーバーに接続できませんでした。');
    assert.dom('[data-test-user]').hasText('octo');
    assert.dom('[data-test-app="blog"]').exists('the console is still usable');

    fakes.api.on('POST', '/auth/logout', undefined);
    await click('[data-test-logout]');

    assert.strictEqual(currentURL(), '/login', 'and it can be tried again');
  });

  test('the header shows the user and logs out', async function (assert) {
    fakes.api.on('POST', '/auth/logout', undefined);

    await visit('/');

    assert.strictEqual(currentURL(), '/');
    assert.dom('[data-test-user]').hasText('octo');
    assert.dom('[data-test-logout]').doesNotExist('the menu starts closed');

    await click('[data-test-user-menu]');
    assert.dom('[data-test-user-menu]').hasAttribute('aria-expanded', 'true');

    await click('[data-test-logout]');

    assert.strictEqual(fakes.api.callsTo('POST', '/auth/logout').length, 1);
    assert.strictEqual(currentURL(), '/login');
    assert.dom('[data-test-user]').doesNotExist();
  });

  test('a logged-in user is sent away from the login page', async function (assert) {
    await visit('/login');

    assert.strictEqual(currentURL(), '/');
  });

  test('a missing session endpoint is an error like any other', async function (assert) {
    fakes.api.fail('GET', '/api/session', 404, 'not_found');

    await visit('/');

    assert.strictEqual(currentURL(), '/login');
    assert.dom('[data-test-session-error]').exists();
    assert.dom('[data-test-login-link]').doesNotExist();
    assert.deepEqual(
      fakes.api.callsTo('GET', '/api/apps'),
      [],
      'the console is not shown without a session',
    );
  });

  test('a failing session check offers a reload', async function (assert) {
    fakes.api.fail('GET', '/api/session', 500, 'internal');

    await visit('/');

    assert.strictEqual(currentURL(), '/login');
    assert.dom('[data-test-session-error]').exists();
    assert.dom('[data-test-login-link]').doesNotExist();

    await click('.login button');
    assert.strictEqual(fakes.browser.reloads, 1);
  });

  test('unknown paths render the not-found page', async function (assert) {
    await visit('/nope');

    assert.dom('[data-test-not-found]').includesText('ページが見つかりません');

    await click('[data-test-back-to-list]');
    assert.strictEqual(currentURL(), '/', 'its one action leads back');
  });
});
