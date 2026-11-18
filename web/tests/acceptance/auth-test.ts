import { module, test } from 'qunit';
import { click, currentURL, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
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
