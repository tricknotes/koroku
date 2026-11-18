import { module, test } from 'qunit';
import BackupForm, { parseList } from 'paas-console/utils/backup-form';
import { makeApp } from 'paas-console/tests/helpers/fakes';

module('Unit | Utility | backup-form', function () {
  test('an untouched form produces an empty patch', function (assert) {
    const app = makeApp({
      backup_exclude_volumes: ['pgdata'],
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -U postgres app',
      backup_before_deploy: true,
    });

    assert.deepEqual(new BackupForm(app).patch(app), {});
    assert.deepEqual(new BackupForm(app).validate(), {});
  });

  test('null collections from the API are treated as empty', function (assert) {
    const app = makeApp({
      backup_exclude_volumes: null,
      backup_before_deploy: false,
    });
    const form = new BackupForm(app);

    assert.deepEqual(form.excluded, []);
    assert.deepEqual(form.patch(app), {});
  });

  test('volumes are taken out of the backup and put back', function (assert) {
    const app = makeApp({
      backup_exclude_volumes: ['pgdata'],
      backup_before_deploy: false,
    });
    const form = new BackupForm(app);

    assert.true(form.isExcluded('pgdata'));
    assert.false(form.isExcluded('uploads'));

    form.setIncluded('cache', false);
    form.setIncluded('cache', false);
    assert.deepEqual(form.patch(app), {
      backup_exclude_volumes: ['pgdata', 'cache'],
    });

    form.setIncluded('pgdata', true);
    assert.deepEqual(form.patch(app), { backup_exclude_volumes: ['cache'] });

    form.setIncluded('cache', true);
    form.setIncluded('pgdata', false);
    assert.deepEqual(form.patch(app), {}, 'back where it started');
  });

  test('the order of the excluded volumes is not a change', function (assert) {
    const app = makeApp({
      backup_exclude_volumes: ['a', 'b'],
      backup_before_deploy: false,
    });
    const form = new BackupForm(app);

    form.setIncluded('a', true);
    form.setIncluded('a', false);

    assert.deepEqual(form.excluded, ['b', 'a']);
    assert.deepEqual(form.patch(app), {});
  });

  test('the patch holds only what changed', function (assert) {
    const app = makeApp({ backup_before_deploy: false });
    const form = new BackupForm(app);

    form.dumpHookService = ' db ';
    form.dumpHookCommand = 'pg_dump -U postgres app';
    form.backupBeforeDeploy = true;

    assert.deepEqual(form.patch(app), {
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -U postgres app',
      backup_before_deploy: true,
    });
  });

  test('nothing is sent to a backend that does not report the fields', function (assert) {
    const app = makeApp();
    const form = new BackupForm(app);
    form.dumpHookService = 'db';
    form.backupBeforeDeploy = true;
    form.setIncluded('pgdata', false);

    assert.false(form.available);
    assert.deepEqual(form.patch(app), {});
  });

  test('validate wants the dump hook as a pair', function (assert) {
    const form = new BackupForm(makeApp({ backup_before_deploy: false }));

    form.dumpHookService = 'db';
    assert.deepEqual(Object.keys(form.validate()), ['dump_hook_command']);

    form.dumpHookService = '';
    form.dumpHookCommand = 'pg_dump -U postgres app';
    assert.deepEqual(Object.keys(form.validate()), ['dump_hook_service']);

    form.dumpHookService = 'db';
    assert.deepEqual(form.validate(), {});
  });

  test('parseList splits on commas and whitespace', function (assert) {
    assert.deepEqual(parseList(''), []);
    assert.deepEqual(parseList(' a, b ,,c\nd '), ['a', 'b', 'c', 'd']);
  });
});
