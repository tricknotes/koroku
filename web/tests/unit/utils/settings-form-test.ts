import { module, test } from 'qunit';
import SettingsForm, {
  ExemptRow,
  LimitRow,
  MAX_EXEMPT_PATHS,
  exemptPathProblem,
  isValidMemoryLimit,
  isValidReservation,
} from 'koroku-console/utils/settings-form';
import { makeApp } from 'koroku-console/tests/helpers/fakes';

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

  test('exempt paths follow the rules of the API', function (assert) {
    for (const ok of [
      '/webhooks/',
      '/api/callback',
      '/.well-known/x',
      '/a@b:c+d~e',
    ]) {
      assert.strictEqual(exemptPathProblem(ok), null, ok);
    }
    for (const bad of [
      '',
      'x',
      '/',
      '/.koroku',
      '/.KOROKU/x',
      '//x',
      '/a/../b',
      '/a/./b',
      '/a*',
      '/a%2e',
      '/{x}',
      '/a b',
      `/${'a'.repeat(200)}`,
    ]) {
      assert.notStrictEqual(exemptPathProblem(bad), null, bad);
    }
  });

  test('the protection and its exempt paths are patched only when changed', function (assert) {
    const app = makeApp({
      access_protection: true,
      protection_exempt_paths: ['/webhooks/'],
    });
    const form = new SettingsForm(app);
    assert.deepEqual(form.patch(app), {});

    form.exemptPaths = [
      ...form.exemptPaths,
      new ExemptRow(' /webhooks/ '),
      new ExemptRow(''),
      new ExemptRow('/hook'),
    ];
    assert.deepEqual(form.patch(app), {
      protection_exempt_paths: ['/webhooks/', '/hook'],
    });

    form.accessProtection = false;
    assert.false(form.patch(app).access_protection);

    form.exemptPaths = Array.from(
      { length: MAX_EXEMPT_PATHS + 1 },
      (_, i) => new ExemptRow(`/p${i}/`),
    );
    assert.ok(form.validate()['protection_exempt_paths']);
  });

  test('an app from an older backend reads as unprotected', function (assert) {
    const app = makeApp();
    const form = new SettingsForm(app);
    assert.false(form.accessProtection);
    assert.deepEqual(form.exemptPaths, []);
    assert.deepEqual(form.patch(app), {});
  });

  test('memory reservations are edited like the limits, 0 meaning none', function (assert) {
    const app = makeApp({
      environment: 'production',
      service_memory_reservations: { worker: '0' },
    });
    const form = new SettingsForm(app);
    assert.deepEqual(form.patch(app), {}, 'untouched');

    form.memoryReservation = ' 256m ';
    form.reservations = [
      ...form.reservations,
      new LimitRow('db', '1g'),
      new LimitRow(),
    ];
    assert.deepEqual(form.validate(), {});
    assert.deepEqual(form.patch(app), {
      memory_reservation: '256m',
      service_memory_reservations: { worker: '0', db: '1g' },
    });

    form.memoryReservation = 'lots';
    form.reservations = [new LimitRow('db', '')];
    const errors = form.validate();
    assert.ok(errors['memory_reservation'], 'not a size');
    assert.ok(
      errors['service_memory_reservations'],
      'a named row needs a value',
    );

    assert.true(isValidReservation(''));
    assert.true(isValidReservation('0'));
    assert.true(isValidReservation('512m'));
    assert.false(isValidReservation('-1m'));
  });
});
