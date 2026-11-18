import { module, test } from 'qunit';
import BackupForm, {
  parseList,
  volumeChoices,
} from 'paas-console/utils/backup-form';
import { makeApp, makeVolume } from 'paas-console/tests/helpers/fakes';
import type { Snapshot } from 'paas-console/utils/types';

function snapshot(target: string, time: string, kind = 'volume'): Snapshot {
  return {
    id: `${target}-${time}`,
    time,
    kind: kind as Snapshot['kind'],
    target,
  };
}

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

  module('volumeChoices', function () {
    const snapshots = [
      snapshot('uploads', '2026-11-16T18:00:12Z'),
      snapshot('uploads', '2026-11-17T18:00:12Z'),
      snapshot('gone', '2026-11-10T18:00:12Z'),
      // A dump is not a volume, whatever its target is called.
      snapshot('cache', '2026-11-17T18:00:30Z', 'dump'),
    ];

    test('the volume list decides what is offered', function (assert) {
      const choices = volumeChoices({
        listed: [
          makeVolume('uploads'),
          makeVolume('pgdata', { excluded: true }),
          makeVolume('cache'),
        ],
        snapshots,
        excluded: ['pgdata'],
      });

      assert.deepEqual(choices, [
        { name: 'cache', included: true, latest: null, missing: false },
        { name: 'pgdata', included: false, latest: null, missing: false },
        {
          name: 'uploads',
          included: true,
          latest: '2026-11-17T18:00:12Z',
          missing: false,
        },
      ]);
    });

    test('the form, not the list, says what is excluded', function (assert) {
      const listed = [
        makeVolume('pgdata', { excluded: true }),
        makeVolume('uploads'),
      ];

      const choices = volumeChoices({
        listed,
        snapshots: [],
        excluded: ['uploads'],
        also: ['pgdata'],
      });

      assert.deepEqual(
        choices.map(({ name, included }) => [name, included]),
        [
          ['pgdata', true],
          ['uploads', false],
        ],
        'unsaved changes show',
      );
    });

    test('names that match no volume are marked', function (assert) {
      const choices = volumeChoices({
        listed: [
          makeVolume('uploads'),
          makeVolume('old', { excluded: true, exists: false }),
        ],
        snapshots: [],
        excluded: ['old', 'typed'],
        also: ['put-back', 'uploads'],
      });

      assert.deepEqual(
        choices.map(({ name, included, missing }) => [name, included, missing]),
        [
          ['old', false, true],
          ['put-back', true, true],
          ['typed', false, true],
          ['uploads', true, false],
        ],
      );
    });

    test('without the list, the snapshots stand in for it', function (assert) {
      const choices = volumeChoices({
        listed: null,
        snapshots,
        excluded: ['pgdata'],
      });

      assert.deepEqual(choices, [
        {
          name: 'gone',
          included: true,
          latest: '2026-11-10T18:00:12Z',
          missing: false,
        },
        { name: 'pgdata', included: false, latest: null, missing: false },
        {
          name: 'uploads',
          included: true,
          latest: '2026-11-17T18:00:12Z',
          missing: false,
        },
      ]);
    });

    test('an app without volumes has nothing to choose', function (assert) {
      assert.deepEqual(
        volumeChoices({ listed: [], snapshots, excluded: [] }),
        [],
        'a snapshot of a volume that is gone does not bring it back',
      );
      assert.deepEqual(
        volumeChoices({ listed: null, snapshots: [], excluded: [] }),
        [],
      );
    });
  });
});
