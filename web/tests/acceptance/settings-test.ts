import { module, test } from 'qunit';
import { click, currentURL, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  installFakes,
  makeApp,
  type FakeCall,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';
import type { App } from 'paas-console/utils/types';

module('Acceptance | settings', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    app = makeApp({
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

  test('only the changed fields are sent', async function (assert) {
    await visit('/apps/blog/settings');

    assert.dom('[data-test-field="default_branch"]').hasValue('main');
    assert.dom('[data-test-field="web_port"]').hasValue('3000');
    assert.dom('[data-test-limit-service]').hasValue('db');
    assert.dom('[data-test-limit-value]').hasValue('1g');
    assert.dom('[data-test-field="backup_exclude_volumes"]').hasValue('pgdata');
    assert
      .dom('[data-test-field="dump_hook_command"]')
      .hasValue('pg_dump -U postgres app');

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
    await fillIn('[data-test-field="backup_exclude_volumes"]', 'pgdata, cache');
    await click('[data-test-field="backup_before_deploy"]');
    await click('[data-test-settings-save]');

    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      healthcheck_path: '/up',
      environment: 'production',
      relax_policy: true,
      memory_limit: '768m',
      service_memory_limits: { db: '1g', web: '512m' },
      backup_exclude_volumes: ['pgdata', 'cache'],
      backup_before_deploy: true,
    });
    assert.dom('[data-test-settings-saved]').exists();
    assert
      .dom('[data-test-app-environment]')
      .hasText('本番', 'the header follows the saved app');
  });

  test('the backup fields are hidden when the backend does not report them', async function (assert) {
    app = makeApp();

    await visit('/apps/blog/settings');

    assert.dom('[data-test-backup-settings-unavailable]').exists();
    assert.dom('[data-test-field="dump_hook_service"]').doesNotExist();

    await fillIn('[data-test-field="web_port"]', '8080');
    await click('[data-test-settings-save]');

    assert.deepEqual(
      fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body,
      { web_port: 8080 },
      'no M5 field is sent to a backend that would reject it',
    );
  });

  test('validation errors are shown next to the fields', async function (assert) {
    await visit('/apps/blog/settings');

    await fillIn('[data-test-field="memory_limit"]', '512 MB');
    await click('[data-test-settings-save]');
    assert.dom('[data-test-field-error="memory_limit"]').exists();
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

    fakes.api.fail('PATCH', '/api/apps/blog', 422, 'validation_failed', {
      compose_path: 'must be a relative path',
    });
    await fillIn('[data-test-field="memory_limit"]', '');
    await fillIn('[data-test-field="compose_path"]', '/etc/compose.yaml');
    await click('[data-test-settings-save]');

    assert
      .dom('[data-test-field-error="compose_path"]')
      .hasText('must be a relative path');
    assert.dom('[data-test-settings-saved]').doesNotExist();
  });

  test('deleting requires typing the slug', async function (assert) {
    await visit('/apps/blog/settings');
    await click('[data-test-delete]');

    assert.dom('[data-test-dialog]').includesText('blog');
    assert.dom('[data-test-dialog-confirm]').isDisabled();

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

  test('deleting with volumes passes volumes=true', async function (assert) {
    app = makeApp({ environment: 'production' });

    await visit('/apps/blog/settings');
    await click('[data-test-delete-volumes]');
    await click('[data-test-delete]');

    assert.dom('[data-test-dialog]').includesText('本番アプリ');
    assert.dom('[data-test-delete-volumes-warning]').exists();

    await fillIn('[data-test-dialog-input]', 'blog');
    await click('[data-test-dialog-confirm]');

    assert.deepEqual(fakes.api.callsTo('DELETE', '/api/apps/blog')[0]?.query, {
      volumes: 'true',
    });
    assert.strictEqual(currentURL(), '/');
  });

  test('a canceled or failed delete leaves the app alone', async function (assert) {
    await visit('/apps/blog/settings');
    await click('[data-test-delete]');
    await click('[data-test-dialog-cancel]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(fakes.api.callsTo('DELETE', '/api/apps/blog'), []);

    fakes.api.fail('DELETE', '/api/apps/blog', 404, 'not_found');
    await click('[data-test-delete]');
    await fillIn('[data-test-dialog-input]', 'blog');
    await click('[data-test-dialog-confirm]');

    assert.strictEqual(currentURL(), '/apps/blog/settings');
    assert.dom('[data-test-error]').exists();
  });
});
