import { module, test } from 'qunit';
import { click, currentURL, triggerKeyEvent, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';

function pinnedTheme(): string | null {
  return document.documentElement.getAttribute('data-theme');
}

module('Acceptance | shell', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    fakes.api.on('GET', '/api/apps', { apps: [makeApp()] });
  });

  test('the top bar has the wordmark, the two sections, the theme switch and the user', async function (assert) {
    await visit('/');

    assert.dom('.topbar [data-test-wordmark]').hasText('koroku');
    assert.dom('.topbar [data-test-nav]').exists({ count: 2 });
    assert.dom('[data-test-nav="apps"]').hasText('アプリ').hasClass('active');
    assert
      .dom('[data-test-nav="system"]')
      .hasText('システム')
      .doesNotHaveClass('active');
    assert.dom('.topbar [data-test-theme-switch]').exists();
    assert.dom('.topbar [data-test-user]').hasText('octo');

    await click('[data-test-nav="system"]');

    assert.strictEqual(currentURL(), '/system');
    assert.dom('[data-test-nav="system"]').hasClass('active');

    await click('[data-test-wordmark]');

    assert.strictEqual(currentURL(), '/');
  });

  test('the theme follows the OS until one is chosen, and the choice is remembered', async function (assert) {
    await visit('/');

    assert.strictEqual(pinnedTheme(), null, 'nothing is pinned by default');
    assert.dom('[data-test-theme="system"]').isChecked();

    await click('[data-test-theme="dark"]');

    assert.strictEqual(pinnedTheme(), 'dark');
    assert.strictEqual(fakes.storage.get('koroku.theme'), 'dark');
    assert.dom('[data-test-theme="dark"]').isChecked();

    await click('[data-test-theme="light"]');

    assert.strictEqual(pinnedTheme(), 'light');
    assert.strictEqual(fakes.storage.get('koroku.theme'), 'light');

    await click('[data-test-theme="system"]');

    assert.strictEqual(pinnedTheme(), null);
    assert.strictEqual(
      fakes.storage.get('koroku.theme'),
      null,
      'following the OS stores nothing',
    );
  });

  test('a remembered theme is applied on load', async function (assert) {
    fakes.storage.set('koroku.theme', 'dark');

    await visit('/');

    assert.strictEqual(pinnedTheme(), 'dark');
    assert.dom('[data-test-theme="dark"]').isChecked();
  });

  test('an unknown stored value falls back to the OS setting', async function (assert) {
    fakes.storage.set('koroku.theme', 'sepia');

    await visit('/');

    assert.strictEqual(pinnedTheme(), null);
    assert.dom('[data-test-theme="system"]').isChecked();
  });

  test('the theme switch is also there before login', async function (assert) {
    fakes.api.on('GET', '/api/session', { authenticated: false });

    await visit('/');

    assert.strictEqual(currentURL(), '/login');
    assert.dom('[data-test-theme-switch]').exists();
    assert.dom('[data-test-user-menu]').doesNotExist();

    await click('[data-test-theme="dark"]');

    assert.strictEqual(pinnedTheme(), 'dark');
  });

  test('the user menu closes on Escape and on a press outside', async function (assert) {
    await visit('/');

    await click('[data-test-user-menu]');
    assert.dom('[data-test-logout]').exists();

    await triggerKeyEvent(document.body, 'keydown', 'Escape');

    assert.dom('[data-test-logout]').doesNotExist();
    assert
      .dom('[data-test-user-menu]')
      .hasAttribute('aria-expanded', 'false')
      .isFocused('focus returns to the button');

    await click('[data-test-user-menu]');
    assert.dom('[data-test-logout]').exists();

    await click('h1');

    assert.dom('[data-test-logout]').doesNotExist();

    await click('[data-test-user-menu]');
    await click('[data-test-user-menu]');

    assert.dom('[data-test-logout]').doesNotExist('the button also closes it');
  });
});
