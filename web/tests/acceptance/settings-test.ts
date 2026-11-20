import { module, test } from 'qunit';
import { click, currentURL, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  apiError,
  installFakes,
  makeApp,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App } from 'koroku-console/utils/types';

function fieldError(name: string): string {
  return `[data-test-field-group="${name}"] [data-test-field-error]`;
}

module('Acceptance | settings', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
      service_memory_limits: { db: '1g' },
      backup_exclude_volumes: ['pgdata'],
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -U postgres app',
      backup_before_deploy: false,
    });
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on('GET', '/api/apps/blog', () => app)
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
        app = { ...app, ...(call.body as Partial<App>) };
        return app;
      })
      .on('DELETE', '/api/apps/blog', undefined);
  });

  test('it shows what the app is deployed from and how', async function (assert) {
    await visit('/apps/blog/settings');

    assert
      .dom('[data-test-repository] a')
      .hasText('octo/blog')
      .hasAttribute('href', 'https://github.com/octo/blog');
    assert.dom('[data-test-field="default_branch"]').hasValue('main');
    assert.dom('[data-test-field="compose_path"]').hasValue('');
    assert.dom('[data-test-field="web_service"]').hasValue('web');
    assert.dom('[data-test-field="web_port"]').hasValue('3000');
    assert.dom('[data-test-host-port]').hasText('127.0.0.1:20000');
    assert.dom('[data-test-field="healthcheck_path"]').hasValue('/');
    assert.dom('[data-test-environment-option="staging"]').isChecked();
    assert.dom('[data-test-field="memory_limit"]').hasValue('');
    assert.dom('[data-test-limit-service]').hasValue('db');
    assert.dom('[data-test-limit-value]').hasValue('1g');
    assert.dom('[data-test-field="relax_policy"]').isNotChecked();
    assert.dom('[data-test-auto-rollback]').includesText('有効');
    assert
      .dom('[data-test-field="dump_hook_service"]')
      .doesNotExist('the backup settings live on the backups tab');
  });

  test('only the changed fields are sent', async function (assert) {
    await visit('/apps/blog/settings');

    await click('[data-test-settings-save]');
    assert.dom('[data-test-settings-unchanged]').exists();
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

    await fillIn('[data-test-field="healthcheck_path"]', '/up');
    await fillIn('[data-test-field="memory_limit"]', '768m');
    await click('[data-test-environment-option="production"]');
    await click('[data-test-field="relax_policy"]');
    await click('[data-test-limit-add]');
    await fillIn(
      '[data-test-limit-row]:nth-child(2) [data-test-limit-service]',
      'web',
    );
    await fillIn(
      '[data-test-limit-row]:nth-child(2) [data-test-limit-value]',
      '512m',
    );
    await click('[data-test-settings-save]');

    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      healthcheck_path: '/up',
      environment: 'production',
      relax_policy: true,
      memory_limit: '768m',
      service_memory_limits: { db: '1g', web: '512m' },
    });
    assert.dom('[data-test-settings-saved]').exists();
    assert
      .dom('[data-test-app-environment]')
      .hasText('本番', 'the header follows the saved app');

    await click('[data-test-limit-row]:nth-child(1) [data-test-limit-remove]');
    assert
      .dom('[data-test-settings-saved]')
      .doesNotExist('a new change clears the note');
    await click('[data-test-settings-save]');

    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[1]?.body, {
      service_memory_limits: { web: '512m' },
    });
  });

  module('unsaved changes and deploying', function (hooks) {
    hooks.beforeEach(function () {
      fakes.api
        .on('GET', '/api/apps/blog/deployments', { deployments: [] })
        .on('GET', '/api/repos/octo/blog/branches', { branches: [] })
        .on('GET', '/api/repos/octo/blog/commits', { commits: [] });
    });

    test('without changes the deploy button goes straight on', async function (assert) {
      await visit('/apps/blog/settings');
      await click('[data-test-header-deploy]');
      assert.strictEqual(currentURL(), '/apps/blog/deploy');
      assert.dom('[data-test-unsaved-dialog]').doesNotExist();
    });

    test('unsaved changes are saved first when asked', async function (assert) {
      await visit('/apps/blog/settings');
      await fillIn('[data-test-field="healthcheck_path"]', '/up');
      await click('[data-test-header-deploy]');

      assert.dom('[data-test-unsaved-dialog]').exists();
      assert.strictEqual(currentURL(), '/apps/blog/settings', 'held back');

      await click('[data-test-dialog-cancel]');
      assert.dom('[data-test-unsaved-dialog]').doesNotExist();
      assert.strictEqual(currentURL(), '/apps/blog/settings');
      assert
        .dom('[data-test-field="healthcheck_path"]')
        .hasValue('/up', 'cancelling keeps the edit');

      await click('[data-test-tab="deploy"]');
      assert.dom('[data-test-unsaved-dialog]').exists('the tab asks too');
      await click('[data-test-dialog-confirm]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        healthcheck_path: '/up',
      });
      assert.strictEqual(currentURL(), '/apps/blog/deploy');
    });

    test('unsaved changes can be dropped', async function (assert) {
      await visit('/apps/blog/settings');
      await fillIn('[data-test-field="healthcheck_path"]', '/up');
      await click('[data-test-header-deploy]');
      await click('[data-test-dialog-alt]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
      assert.strictEqual(currentURL(), '/apps/blog/deploy');
    });

    test('a save that fails keeps the user on the settings', async function (assert) {
      fakes.api.fail('PATCH', '/api/apps/blog', 422, 'validation_failed', {
        healthcheck_path: 'must start with /',
      });
      await visit('/apps/blog/settings');
      await fillIn('[data-test-field="healthcheck_path"]', '/up');
      await click('[data-test-header-deploy]');
      await click('[data-test-dialog-confirm]');

      assert.strictEqual(currentURL(), '/apps/blog/settings');
      assert.dom(fieldError('healthcheck_path')).exists();
    });
  });

  test('the requirements check is shown and follows a saved change', async function (assert) {
    fakes.api.on(
      'GET',
      '/api/apps/blog/env/requirements',
      () => ({
        source: 'compose',
        status: 'ok',
        reason: '',
        ref: app.default_branch,
        commit_sha: 'a'.repeat(40),
        path: 'compose.yaml',
        variables:
          app.default_branch === 'next'
            ? [
                { key: 'DATABASE_URL', required: true, set: false },
                { key: 'SECRET_KEY_BASE', required: true, set: true },
              ]
            : [{ key: 'SECRET_KEY_BASE', required: true, set: true }],
        unlisted: [],
      }),
    );
    await visit('/apps/blog/settings');
    assert
      .dom('[data-test-settings-requirements="default"]')
      .includesText('1 件は、すべて設定済み');

    await fillIn('[data-test-field="default_branch"]', 'next');
    await click('[data-test-settings-save]');

    assert.strictEqual(
      fakes.api.callsTo('GET', '/api/apps/blog/env/requirements').length,
      2,
      'read again after the branch changed',
    );
    assert
      .dom('[data-test-settings-requirements="warning"]')
      .includesText('ブランチ next')
      .includesText('2 件のうち、1 件が未設定');
    assert.dom('[data-test-settings-requirements-link]').exists();

    await fillIn('[data-test-field="healthcheck_path"]', '/up');
    await click('[data-test-settings-save]');
    assert.strictEqual(
      fakes.api.callsTo('GET', '/api/apps/blog/env/requirements').length,
      2,
      'other settings do not read it again',
    );
  });

  test('relaxing the policy checks comes with a warning', async function (assert) {
    await visit('/apps/blog/settings');

    assert.dom('[data-test-relax-warning]').doesNotExist();

    await click('[data-test-field="relax_policy"]');

    assert
      .dom('[data-test-relax-warning]')
      .includesText('保護の多くが効きません')
      .includesText('root');

    await click('[data-test-field="relax_policy"]');
    assert.dom('[data-test-relax-warning]').doesNotExist();
  });

  test('an app that already runs relaxed shows the warning at once', async function (assert) {
    app = { ...app, relax_policy: true };

    await visit('/apps/blog/settings');

    assert.dom('[data-test-field="relax_policy"]').isChecked();
    assert.dom('[data-test-relax-warning]').exists();
  });

  test('validation errors are shown next to the fields', async function (assert) {
    await visit('/apps/blog/settings');

    await fillIn('[data-test-field="memory_limit"]', '512 MB');
    await fillIn('[data-test-limit-value]', 'lots');
    await click('[data-test-settings-save]');
    assert.dom(fieldError('memory_limit')).exists();
    assert.dom(fieldError('service_memory_limits')).exists();
    assert
      .dom('[data-test-field="memory_limit"]')
      .hasAttribute('aria-invalid', 'true');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

    fakes.api.on('PATCH', '/api/apps/blog', () => {
      throw apiError(422, 'validation_failed', {
        compose_path: 'must be a relative path',
        mystery: 'is not understood',
      });
    });
    await fillIn('[data-test-field="memory_limit"]', '');
    await fillIn('[data-test-limit-value]', '1g');
    await fillIn('[data-test-field="compose_path"]', '/etc/compose.yaml');
    await click('[data-test-settings-save]');

    assert.dom(fieldError('compose_path')).hasText('must be a relative path');
    assert
      .dom('[data-test-other-errors]')
      .hasText('mystery: is not understood');
    assert.dom('[data-test-settings-saved]').doesNotExist();
  });

  module('access protection', function () {
    test('a protected app edits the paths that skip the protection', async function (assert) {
      app = {
        ...app,
        access_protection: true,
        protection_exempt_paths: ['/webhooks/'],
      };
      await visit('/apps/blog/settings');

      assert.dom('[data-test-field="access_protection"]').isChecked();
      assert.dom('[data-test-app-protected]').hasText('保護');
      assert.dom('[data-test-exempt-path]').hasValue('/webhooks/');
      assert.dom('[data-test-protection-public]').doesNotExist();

      await click('[data-test-exempt-add]');
      await fillIn(
        '[data-test-exempt-row]:nth-child(2) [data-test-exempt-path]',
        ' /api/hook ',
      );
      await click('[data-test-settings-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        protection_exempt_paths: ['/webhooks/', '/api/hook'],
      });
      assert.dom('[data-test-settings-saved]').exists();

      await click(
        '[data-test-exempt-row]:nth-child(1) [data-test-exempt-remove]',
      );
      await click('[data-test-settings-save]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[1]?.body, {
        protection_exempt_paths: ['/api/hook'],
      });
    });

    test('paths that would open too much are refused before saving', async function (assert) {
      app = { ...app, access_protection: true, protection_exempt_paths: [] };
      await visit('/apps/blog/settings');

      for (const [path, message] of [
        ['/', '保護そのものを無効に'],
        ['webhooks', '/ で始まる'],
        ['/hooks/*', '* や %'],
        ['/.koroku/auth/', '/.koroku'],
        ['/a/../b', '..'],
      ]) {
        if (!document.querySelector('[data-test-exempt-row]')) {
          await click('[data-test-exempt-add]');
        }
        await fillIn('[data-test-exempt-path]', path!);
        await click('[data-test-settings-save]');
        assert
          .dom(fieldError('protection_exempt_paths'))
          .includesText(message!, path);
        assert
          .dom('[data-test-exempt-path]')
          .hasAttribute('aria-invalid', 'true');
      }
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
    });

    test('turning it off on a staging app saves at once and explains noindex', async function (assert) {
      app = { ...app, access_protection: true };
      await visit('/apps/blog/settings');

      await click('[data-test-field="access_protection"]');
      assert
        .dom('[data-test-field-group="protection_exempt_paths"]')
        .doesNotExist();
      assert.dom('[data-test-protection-public]').includesText('noindex');
      await click('[data-test-settings-save]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        access_protection: false,
      });
      assert.dom('[data-test-app-protected]').doesNotExist();
    });

    test('a production app asks before the protection changes', async function (assert) {
      app = { ...app, environment: 'production', access_protection: false };
      await visit('/apps/blog/settings');

      await click('[data-test-field="access_protection"]');
      await click('[data-test-settings-save]');
      assert.dom('[data-test-protection-confirm="on"]').includesText('blog');
      await click('[data-test-dialog-cancel]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

      await click('[data-test-settings-save]');
      await click('[data-test-dialog-confirm]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        access_protection: true,
      });
      assert.dom('[data-test-dialog]').doesNotExist();

      await click('[data-test-field="access_protection"]');
      await click('[data-test-settings-save]');
      assert
        .dom('[data-test-protection-confirm="off"]')
        .includesText('誰でも開ける');
      assert.dom('[data-test-dialog-confirm]').hasClass('button-danger');
    });
  });

  module('start, stop and restart', function () {
    test('restart is an ordinary action; stop sits in the danger zone and asks first', async function (assert) {
      fakes.api
        .on('POST', '/api/apps/blog/restart', () => app)
        .on('POST', '/api/apps/blog/stop', () => {
          app = { ...app, desired_state: 'stopped' };
          return app;
        })
        .on('POST', '/api/apps/blog/start', () => {
          app = { ...app, desired_state: 'running' };
          return app;
        });

      await visit('/apps/blog/settings');

      assert.dom('[data-test-settings-state]').hasText('稼働中');
      assert
        .dom('[data-test-danger-zone] [data-test-control="restart"]')
        .doesNotExist('restart is not a dangerous operation');
      assert.dom('[data-test-danger-zone] [data-test-control="stop"]').exists();
      assert.dom('[data-test-danger-zone] [data-test-delete]').exists();
      assert
        .dom('[data-test-settings-form] [data-test-control]')
        .doesNotExist('none of them is next to the save button');
      assert.dom('[data-test-control="start"]').doesNotExist();

      await click('[data-test-control="restart"]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/restart').length,
        1,
      );
      assert.dom('[data-test-notice]').hasText('再起動しました。');

      await click('[data-test-control="stop"]');

      assert.dom('[data-test-dialog]').includesText('503');
      assert.deepEqual(fakes.api.callsTo('POST', '/api/apps/blog/stop'), []);
      assert.dom('[data-test-dialog-input]').doesNotExist();

      await click('[data-test-dialog-confirm]');

      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/stop').length,
        1,
      );
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.dom('[data-test-notice]').hasText('停止しました。');
      assert.dom('[data-test-settings-state]').hasText('停止');
      assert
        .dom('[data-test-control="stop"]')
        .isDisabled('a stopped app cannot be stopped again');
      assert.dom('[data-test-control="restart"]').doesNotExist();

      await click('[data-test-control="start"]');

      assert.dom('[data-test-settings-state]').hasText('稼働中');
      assert.dom('[data-test-notice]').hasText('起動しました。');
      assert.dom('[data-test-control="stop"]').isNotDisabled();
    });

    test('stopping a production app names it as such', async function (assert) {
      app = { ...app, environment: 'production' };

      await visit('/apps/blog/settings');
      await click('[data-test-control="stop"]');

      assert.dom('[data-test-dialog]').includesText('本番アプリ');

      await click('[data-test-dialog-cancel]');
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(fakes.api.callsTo('POST', '/api/apps/blog/stop'), []);
    });

    test('an app that was never deployed cannot be started or stopped', async function (assert) {
      app = makeApp();

      await visit('/apps/blog/settings');

      assert.dom('[data-test-settings-state]').hasText('未デプロイ');
      assert.dom('[data-test-control="restart"]').isDisabled();
      assert.dom('[data-test-control="stop"]').isDisabled();
      assert.dom('[data-test-delete]').isNotDisabled();
    });

    test('a refused operation says why', async function (assert) {
      fakes.api.fail('POST', '/api/apps/blog/restart', 409, 'busy');

      await visit('/apps/blog/settings');
      await click('[data-test-control="restart"]');

      assert.dom('[data-test-error]').includesText('ほかの処理が実行中です');
      assert.dom('[data-test-notice]').doesNotExist();
      assert.dom('[data-test-control="restart"]').isNotDisabled();
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

      await visit('/apps/blog/settings');
      await click('[data-test-control="start"]');

      assert.dom('[data-test-error]').includesText('再デプロイ');
      assert
        .dom('[data-test-violations]')
        .hasText('web: privileged containers are not allowed');
    });
  });

  module('delete', function () {
    test('deleting requires typing the app name', async function (assert) {
      await visit('/apps/blog/settings');
      await click('[data-test-delete]');

      assert.dom('[data-test-dialog]').includesText('blog');
      assert.dom('[data-test-dialog-confirm]').isDisabled();
      assert.dom('[data-test-delete-volumes]').isNotChecked();

      await fillIn('[data-test-dialog-input]', 'blo');
      assert.dom('[data-test-dialog-confirm]').isDisabled();

      await fillIn('[data-test-dialog-input]', 'blog');
      assert.dom('[data-test-dialog-confirm]').isNotDisabled();
      assert.deepEqual(fakes.api.callsTo('DELETE', '/api/apps/blog'), []);

      await click('[data-test-dialog-confirm]');

      const calls = fakes.api.callsTo('DELETE', '/api/apps/blog');
      assert.strictEqual(calls.length, 1);
      assert.deepEqual(calls[0]?.query, {}, 'volumes are kept by default');
      assert.strictEqual(currentURL(), '/');
    });

    test('deleting with volumes is chosen in the confirmation', async function (assert) {
      app = { ...app, environment: 'production' };

      await visit('/apps/blog/settings');
      await click('[data-test-delete]');

      assert.dom('[data-test-dialog]').includesText('本番アプリ');
      assert.dom('[data-test-delete-volumes-warning]').doesNotExist();

      await click('[data-test-delete-volumes]');
      assert.dom('[data-test-delete-volumes-warning]').exists();

      await fillIn('[data-test-dialog-input]', 'blog');
      await click('[data-test-dialog-confirm]');

      assert.deepEqual(
        fakes.api.callsTo('DELETE', '/api/apps/blog')[0]?.query,
        { volumes: 'true' },
      );
      assert.strictEqual(currentURL(), '/');
    });

    test('a canceled or failed delete leaves the app alone', async function (assert) {
      await visit('/apps/blog/settings');
      await click('[data-test-delete]');
      await click('[data-test-delete-volumes]');
      await click('[data-test-dialog-cancel]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(fakes.api.callsTo('DELETE', '/api/apps/blog'), []);

      fakes.api.fail('DELETE', '/api/apps/blog', 409, 'busy');
      await click('[data-test-delete]');
      assert
        .dom('[data-test-delete-volumes]')
        .isNotChecked('the choice starts over each time');
      await fillIn('[data-test-dialog-input]', 'blog');
      await click('[data-test-dialog-confirm]');

      assert.strictEqual(currentURL(), '/apps/blog/settings');
      assert
        .dom('[data-test-danger-zone] [data-test-error]')
        .includesText('ほかの処理が実行中です');
    });
  });
});
