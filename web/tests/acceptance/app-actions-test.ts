import { module, test } from 'qunit';
import { click, triggerKeyEvent, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, AppRole } from 'koroku-console/utils/types';

/**
 * Start, stop, restart and wake live in the app header (ARCHITECTURE 9.1):
 * a 「…」 menu next to 「デプロイ」 on every tab, and a visible button when
 * the app is stopped or asleep.
 */
module('Acceptance | app header actions', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
    });
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on('GET', '/api/apps/blog', () => app)
      .on('POST', '/api/apps/blog/restart', () => app)
      .on('POST', '/api/apps/blog/stop', () => {
        app = { ...app, desired_state: 'stopped' };
        return app;
      })
      .on('POST', '/api/apps/blog/start', () => {
        app = { ...app, desired_state: 'running' };
        return app;
      })
      .on('POST', '/api/apps/blog/wake', () => {
        app = { ...app, sleeping_since: null };
        return app;
      });
  });

  test('restart is in the menu on every tab; stop asks first', async function (assert) {
    for (const tab of ['', '/logs', '/env', '/settings']) {
      await visit(`/apps/blog${tab}`);
      assert.dom('[data-test-action-menu]').exists(`menu on "${tab}"`);
      assert
        .dom('[data-test-header-deploy]')
        .exists('next to the deploy button');
    }
    assert.dom('[data-test-control="start"]').doesNotExist();
    assert.dom('[data-test-control="restart"]').doesNotExist('closed at first');

    await click('[data-test-action-menu]');
    assert.dom('[data-test-action-menu]').hasAttribute('aria-expanded', 'true');
    await click('[data-test-control="restart"]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.strictEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/restart').length,
      1,
    );
    assert.dom('[data-test-notice]').hasText('再起動しました。');
    assert
      .dom('[data-test-action-menu-panel]')
      .doesNotExist('the menu closes once an item ran');

    await click('[data-test-action-menu]');
    await click('[data-test-control="stop"]');

    assert.dom('[data-test-stop-dialog]').includesText('503');
    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps/blog/stop'), []);
    assert.dom('[data-test-dialog-input]').doesNotExist();

    await click('[data-test-dialog-confirm]');

    assert.strictEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/stop').length,
      1,
    );
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.dom('[data-test-notice]').hasText('停止しました。');
    assert.dom('[data-test-app-state]').hasText('停止');
    assert
      .dom('[data-test-action-menu]')
      .doesNotExist('nothing to restart or stop on a stopped app');
    assert
      .dom('[data-test-control="start"]')
      .hasClass('button-primary', '起動 is the one main action now');
    assert.dom('[data-test-header-deploy]').doesNotHaveClass('button-primary');

    await click('[data-test-control="start"]');

    assert.dom('[data-test-app-state]').hasText('稼働中');
    assert.dom('[data-test-notice]').hasText('起動しました。');
    assert.dom('[data-test-control="start"]').doesNotExist();
    assert.dom('[data-test-action-menu]').exists();
  });

  test('stopping a production app names it as such', async function (assert) {
    app = { ...app, environment: 'production' };

    await visit('/apps/blog');
    await click('[data-test-action-menu]');
    await click('[data-test-control="stop"]');

    assert.dom('[data-test-dialog]').includesText('本番アプリ');

    await click('[data-test-dialog-cancel]');
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps/blog/stop'), []);
  });

  test('a sleeping app shows 「起こす」 in the header', async function (assert) {
    app = { ...app, sleeping_since: '2026-11-18T03:00:00Z' };

    await visit('/apps/blog/resources');
    assert.dom('[data-test-control="wake"]').hasText('起こす');
    assert.dom('[data-test-action-menu]').exists('restart and stop stay');

    await click('[data-test-control="wake"]');
    assert.strictEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/wake').length,
      1,
    );
    assert.dom('[data-test-notice]').hasText('起こしました。');
    assert.dom('[data-test-control="wake"]').doesNotExist();
  });

  test('an app that was never deployed has nothing to start or stop', async function (assert) {
    app = makeApp();

    await visit('/apps/blog');

    assert.dom('[data-test-app-state]').hasText('未デプロイ');
    assert.dom('[data-test-action-menu]').doesNotExist();
    assert.dom('[data-test-control]').doesNotExist();
    assert.dom('[data-test-header-deploy]').exists();
  });

  test('only the deploy role and up see the actions', async function (assert) {
    const cases: [AppRole, boolean][] = [
      ['view', false],
      ['deploy', true],
      ['manage', true],
    ];
    for (const [role, shown] of cases) {
      app = { ...app, role, desired_state: 'stopped' };
      await visit('/');
      await visit('/apps/blog');
      assert
        .dom('[data-test-control="start"]')
        .exists({ count: shown ? 1 : 0 }, role);
    }
  });

  test('Escape closes the menu and returns the focus', async function (assert) {
    await visit('/apps/blog');
    await click('[data-test-action-menu]');
    assert.dom('[data-test-action-menu-panel]').exists();

    await triggerKeyEvent(document, 'keydown', 'Escape');
    assert.dom('[data-test-action-menu-panel]').doesNotExist();
    assert.dom('[data-test-action-menu]').isFocused();
  });

  test('a refused operation says why', async function (assert) {
    fakes.api.fail('POST', '/api/apps/blog/restart', 409, 'busy');

    await visit('/apps/blog');
    await click('[data-test-action-menu]');
    await click('[data-test-control="restart"]');

    assert.dom('[data-test-error]').includesText('ほかの処理が実行中です');
    assert.dom('[data-test-notice]').doesNotExist();
  });

  test('a start refused by the policy lists the findings', async function (assert) {
    app = { ...app, desired_state: 'stopped' };
    fakes.api.fail('POST', '/api/apps/blog/start', 422, 'policy_violation', {
      violations: [
        {
          service: 'web',
          rule: 'privileged',
          message: 'privileged containers are not allowed',
          relaxable: true,
        },
      ],
    });

    await visit('/apps/blog');
    await click('[data-test-control="start"]');

    assert.dom('[data-test-error]').includesText('再デプロイ');
    assert
      .dom('[data-test-violations]')
      .hasText('web: privileged containers are not allowed');
  });
});
