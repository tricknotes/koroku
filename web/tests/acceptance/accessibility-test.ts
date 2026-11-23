import { module, test } from 'qunit';
import { click, fillIn, findAll, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import { accessibilityProblems } from 'koroku-console/tests/helpers/a11y';
import {
  SCREENS,
  makeBusy,
  serveConsole,
  type Console,
} from 'koroku-console/tests/helpers/console';
import { installFakes, type Fakes } from 'koroku-console/tests/helpers/fakes';

const STEP = '==> 2026-11-18T03:00:01Z build Building images';

/** Controls inside the page, below the top bar. */
function controlsInMain(): number {
  return findAll('main a[href], main button, main input, main select').length;
}

// One pass over every screen for what a test can tell about accessibility:
// every control (icon-only buttons included) has a name, labels and
// descriptions point at something, icons are hidden from assistive
// technology, and a state is always an icon plus words.
module('Acceptance | accessibility', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let state: Console;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    state = serveConsole(fakes);
  });

  for (const screen of SCREENS) {
    test(`${screen.name}: with its data`, async function (assert) {
      await visit(screen.url);

      assert
        .dom('[data-test-error], [data-test-unavailable]')
        .doesNotExist('the screen shows its data, not a failure');
      assert.true(controlsInMain() > 0, 'there is something to operate');
      assert.deepEqual(accessibilityProblems(), []);
    });

    test(`${screen.name}: while the backend fails`, async function (assert) {
      // The session and the app itself are known; everything else is down.
      fakes.api.failReads(500, 'internal', ['/api/session', '/api/apps/blog']);

      await visit(screen.url);

      assert.deepEqual(accessibilityProblems(), []);
    });
  }

  for (const screen of SCREENS) {
    test(`${screen.name}: while something is in progress`, async function (assert) {
      makeBusy(state);

      await visit(screen.url);

      assert.deepEqual(accessibilityProblems(), []);
    });
  }

  test('the login page', async function (assert) {
    fakes.api.on('GET', '/api/session', { authenticated: false });

    await visit('/?login_error=not_allowed');

    assert.dom('[data-test-login-link]').exists();
    assert.deepEqual(accessibilityProblems(), []);
  });

  test('the user menu, open', async function (assert) {
    await visit('/');
    await click('[data-test-user-menu]');

    assert.dom('[data-test-logout]').exists();
    assert.deepEqual(accessibilityProblems(), []);
  });

  test('a deployment with its steps and a log open', async function (assert) {
    makeBusy(state);
    await visit('/apps/blog/deploy');
    fakes.eventSource.last.emit(STEP);
    fakes.eventSource.last.emit('#1 [web 1/6] FROM ruby:3.4-slim');
    await settled();

    assert.dom('[data-test-step]').exists();
    assert.dom('[data-test-cancel]').exists();
    assert.deepEqual(accessibilityProblems(), []);
  });

  test('the log list with a row opened', async function (assert) {
    await visit('/apps/blog/logs?period=1h');
    await click('[data-test-log-row]');

    assert.dom('[data-test-log-detail]').exists();
    assert.deepEqual(accessibilityProblems(), []);
  });

  test('environment variables: revealed, being edited, being deleted, and as .env text', async function (assert) {
    await visit('/apps/blog/env');
    await click('[data-test-env-row]:nth-child(1) [data-test-env-reveal]');
    await click('[data-test-env-row]:nth-child(2) [data-test-env-edit]');
    await fillIn('[data-test-env-key-input]', '1BAD');

    assert.dom('[data-test-env-key-error]').exists();
    assert.deepEqual(accessibilityProblems(), []);

    await click('[data-test-env-cancel]');
    await click('[data-test-env-row]:nth-child(2) [data-test-env-remove]');
    assert.deepEqual(accessibilityProblems(), []);

    await click('[data-test-env-remove-cancel]');
    await click('[data-test-env-bulk]');
    await fillIn('[data-test-env-bulk-text]', 'not a line');

    assert.dom('[data-test-env-issue]').exists();
    assert.deepEqual(accessibilityProblems(), []);
  });

  test('the confirmation dialogs', async function (assert) {
    state.app = { ...state.app, environment: 'production' };

    await visit('/apps/blog/deploy');
    await click('[data-test-deploy-submit]');
    assert.dom('[data-test-dialog]').exists();
    assert.deepEqual(accessibilityProblems(), [], 'deploy to production');
    await click('[data-test-dialog-cancel]');

    await visit('/apps/blog/settings');
    await click('[data-test-delete]');
    assert.dom('[data-test-dialog-input]').exists();
    assert.deepEqual(accessibilityProblems(), [], 'delete the app');
    await click('[data-test-dialog-cancel]');

    await visit('/apps/blog/backups');
    await click('[data-test-snapshot-restore]');
    assert.dom('[data-test-dialog-input]').exists();
    assert.deepEqual(accessibilityProblems(), [], 'restore a volume');
    await click('[data-test-dialog-cancel]');

    await visit('/apps/blog/settings?section=domains');
    await click('[data-test-domain-delete]');
    assert.dom('[data-test-dialog]').exists();
    assert.deepEqual(accessibilityProblems(), [], 'delete a domain');
    await click('[data-test-dialog-cancel]');

    await visit('/system');
    await click('[data-test-prune-open]');
    assert.dom('[data-test-dialog]').exists();
    assert.deepEqual(accessibilityProblems(), [], 'prune');
  });

  test('an empty host and an unknown page', async function (assert) {
    fakes.api.on('GET', '/api/apps', { apps: [] });

    await visit('/');
    assert.dom('[data-test-empty]').exists();
    assert.deepEqual(accessibilityProblems(), []);

    await visit('/nope');
    assert.dom('[data-test-not-found]').exists();
    assert.deepEqual(accessibilityProblems(), []);
  });
});
