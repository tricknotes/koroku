import { module, test } from 'qunit';
import SettingsForm, {
  LimitRow,
  isValidMemoryLimit,
} from 'paas-console/utils/settings-form';
import { makeApp } from 'paas-console/tests/helpers/fakes';

module('Unit | Utility | settings-form', function () {
  test('an untouched form produces an empty patch', function (assert) {
    const app = makeApp({
      service_memory_limits: { db: '1g' },
      backup_exclude_volumes: ['pgdata'],
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -U postgres app',
      backup_before_deploy: true,
    });

    assert.deepEqual(new SettingsForm(app).patch(app), {});
    assert.deepEqual(new SettingsForm(app).validate(), {});
  });

  test('null collections from the API are treated as empty', function (assert) {
    const app = makeApp({ service_memory_limits: null });

    assert.deepEqual(new SettingsForm(app).patch(app), {});
  });

  test('the patch holds only what changed', function (assert) {
    const app = makeApp({ service_memory_limits: { db: '1g' } });
    const form = new SettingsForm(app);

    form.webPort = '8080';
    form.composePath = ' deploy/compose.yaml ';
    form.limits = [...form.limits, new LimitRow('web', '512m'), new LimitRow()];

    assert.deepEqual(form.patch(app), {
      compose_path: 'deploy/compose.yaml',
      web_port: 8080,
      service_memory_limits: { db: '1g', web: '512m' },
    });

    form.limits = [];
    assert.deepEqual(form.patch(app).service_memory_limits, {});
  });

  test('the backup settings are left to their own form', function (assert) {
    const app = makeApp({
      backup_exclude_volumes: ['pgdata'],
      backup_before_deploy: false,
    });
    const form = new SettingsForm(app);
    form.environment = 'production';

    assert.deepEqual(form.patch(app), { environment: 'production' });
  });

  test('validate checks the port and the memory limits', function (assert) {
    const form = new SettingsForm(makeApp());

    form.webPort = '0';
    form.memoryLimit = '512 MB';
    form.limits = [new LimitRow('db', '')];

    assert.deepEqual(Object.keys(form.validate()).sort(), [
      'memory_limit',
      'service_memory_limits',
      'web_port',
    ]);

    form.webPort = '3000';
    form.memoryLimit = '512m';
    form.limits = [new LimitRow('db', '1g'), new LimitRow('', '')];
    assert.deepEqual(form.validate(), {});
  });

  test('isValidMemoryLimit', function (assert) {
    assert.true(isValidMemoryLimit(''));
    assert.true(isValidMemoryLimit('512m'));
    assert.true(isValidMemoryLimit('1g'));
    assert.false(isValidMemoryLimit('1.5g'));
    assert.false(isValidMemoryLimit('512'));
    assert.false(isValidMemoryLimit('512MB'));
  });
});
