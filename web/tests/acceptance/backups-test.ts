import { module, test } from 'qunit';
import {
  click,
  fillIn,
  settled,
  triggerKeyEvent,
  visit,
  waitFor,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeBackups,
  makeDeployment,
  makeVolume,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type {
  App,
  AppVolume,
  BackupRun,
  Snapshot,
} from 'koroku-console/utils/types';

const VOLUME: Snapshot = {
  id: '4bba301e',
  time: '2026-11-17T18:00:12Z',
  kind: 'volume',
  target: 'uploads',
  size_bytes: 214748364,
};

const DUMP: Snapshot = {
  id: '77aa00ff',
  time: '2026-11-17T18:00:30Z',
  kind: 'dump',
  target: 'db',
  size_bytes: null,
};

function run(overrides: Partial<BackupRun> = {}): BackupRun {
  return {
    id: 12,
    trigger: 'scheduled',
    status: 'succeeded',
    started_at: '2026-11-17T18:00:00Z',
    finished_at: '2026-11-17T18:00:40Z',
    error: '',
    ...overrides,
  };
}

module('Acceptance | backups', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let snapshots: Snapshot[];
  /** The volumes "Docker has" for the app. */
  let existing: string[];

  /** The volume list as the backend builds it from those two sources. */
  function volumeList(): { volumes: AppVolume[] } {
    const excluded = app.backup_exclude_volumes ?? [];
    const names = [...new Set([...existing, ...excluded])].sort();
    return {
      volumes: names.map((name) =>
        makeVolume(name, {
          excluded: excluded.includes(name),
          exists: existing.includes(name),
        }),
      ),
    };
  }

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
      backup_exclude_volumes: ['pgdata'],
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -U postgres app',
      backup_before_deploy: false,
    });
    snapshots = [VOLUME, DUMP];
    // `cache` was added by a deployment after the last backup.
    existing = ['cache', 'pgdata', 'uploads'];
    fakes.api
      .on('GET', '/api/apps/blog', () => app)
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
        app = { ...app, ...(call.body as Partial<App>) };
        return app;
      })
      .on('GET', '/api/backups', makeBackups({ runs: [run()] }))
      .on('GET', '/api/apps/blog/backups', () => ({ snapshots }))
      .on('GET', '/api/apps/blog/volumes', volumeList);
  });

  module('status and snapshots', function () {
    test('it shows the last success, the next run and the snapshots', async function (assert) {
      await visit('/apps/blog/backups');

      assert.dom('[data-test-backup-state]').hasText('正常');
      assert
        .dom('[data-test-backup-last-success] time')
        .hasAttribute('datetime', '2026-11-17T18:00:40Z')
        .hasAttribute('title', '2026/11/18 03:00 JST');
      assert
        .dom('[data-test-backup-next-run] time')
        .hasAttribute('datetime', '2026-11-18T18:00:00Z')
        .hasAttribute('title', '2026/11/19 03:00 JST');
      assert
        .dom('[data-test-backup-run]')
        .isNotDisabled()
        .hasText('今すぐ実行');
      assert.dom('[data-test-backup-failed]').doesNotExist();
      assert.dom('[data-test-backup-warning]').doesNotExist();

      assert
        .dom('[data-test-snapshot="4bba301e"]')
        .includesText('uploads')
        .includesText('4bba301e');
      assert
        .dom('[data-test-snapshot="4bba301e"] [data-test-snapshot-kind]')
        .hasText('volume');
      assert
        .dom('[data-test-snapshot="4bba301e"] [data-test-snapshot-size]')
        .includesText('204.8 MB');
      assert
        .dom('[data-test-snapshot="77aa00ff"] [data-test-snapshot-kind]')
        .hasText('ダンプ');
      assert
        .dom('[data-test-snapshot="77aa00ff"] [data-test-snapshot-size]')
        .includesText('—', 'a size restic did not record is left out');
    });

    test('dumps download, volumes restore', async function (assert) {
      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-snapshot="77aa00ff"] [data-test-snapshot-download]')
        .hasAttribute('href', '/api/apps/blog/backups/77aa00ff/download')
        .hasAttribute('download');
      assert
        .dom('[data-test-snapshot="77aa00ff"] [data-test-snapshot-restore]')
        .doesNotExist();
      assert
        .dom('[data-test-snapshot="4bba301e"] [data-test-snapshot-download]')
        .doesNotExist();
      assert
        .dom('[data-test-dump-help]')
        .includesText(
          'docker compose -p koroku-blog exec -T db psql',
          'the example uses the service of the dump hook',
        );
    });

    test('a failed latest run and a stale backup stand out', async function (assert) {
      fakes.api.on(
        'GET',
        '/api/backups',
        makeBackups({
          warning: true,
          last_success_at: '2026-11-15T18:00:40Z',
          runs: [
            run({
              id: 14,
              status: 'failed',
              error: 'restic backup: no space left on device',
            }),
            run({ id: 13 }),
          ],
        }),
      );

      await visit('/apps/blog/backups');

      assert.dom('[data-test-backup-state]').hasText('直近の実行が失敗');
      assert
        .dom('[data-test-backup-state] [data-status]')
        .hasAttribute('data-status', 'failed');
      assert
        .dom('[data-test-backup-failed]')
        .includesText('直近のバックアップが失敗しました')
        .includesText('restic backup: no space left on device')
        .includesText('24 時間以上');
    });

    test('no success for a day is a warning of its own', async function (assert) {
      fakes.api.on(
        'GET',
        '/api/backups',
        makeBackups({ warning: true, runs: [run()] }),
      );

      await visit('/apps/blog/backups');

      assert.dom('[data-test-backup-state]').hasText('24 時間以上成功なし');
      assert
        .dom('[data-test-backup-warning]')
        .includesText('24 時間以上、バックアップが成功していません');
      assert.dom('[data-test-backup-failed]').doesNotExist();
    });
  });

  module('running a backup', function () {
    test('"run now" starts one run, shows it and follows it to the end', async function (assert) {
      let running = false;
      fakes.api
        .on('GET', '/api/backups', () =>
          makeBackups({
            running,
            runs: running ? [run({ id: 13, status: 'running' })] : [run()],
          }),
        )
        .on('POST', '/api/backups', () => {
          running = true;
          return run({ id: 13, trigger: 'manual', status: 'running' });
        });

      await visit('/apps/blog/backups');
      await click('[data-test-backup-run]');

      assert.strictEqual(fakes.api.callsTo('POST', '/api/backups').length, 1);
      assert.dom('[data-test-backup-state]').hasText('実行中');
      assert
        .dom('[data-test-backup-run]')
        .isDisabled('a second run cannot be started')
        .hasText('実行中…')
        .hasAttribute('aria-busy', 'true');

      // The run ends and leaves a new snapshot; nobody reloads the page.
      // A volume a deployment created in the meantime shows up with it.
      running = false;
      snapshots = [
        { ...VOLUME, id: 'aaaa1111', time: '2026-11-18T03:00:10Z' },
        ...snapshots,
      ];
      existing = [...existing, 'media'];
      await fakes.poller.tick();
      await settled();

      assert.dom('[data-test-backup-state]').hasText('正常');
      assert
        .dom('[data-test-backup-run]')
        .isNotDisabled()
        .hasText('今すぐ実行');
      assert.dom('[data-test-snapshot="aaaa1111"]').exists();
      assert.dom('[data-test-volume="media"]').exists();
      assert.strictEqual(fakes.api.callsTo('POST', '/api/backups').length, 1);
    });

    test('a run that is already going disables the button', async function (assert) {
      fakes.api.on(
        'GET',
        '/api/backups',
        makeBackups({ running: true, runs: [run({ status: 'running' })] }),
      );

      await visit('/apps/blog/backups');

      assert.dom('[data-test-backup-state]').hasText('実行中');
      assert.dom('[data-test-backup-run]').isDisabled();
    });

    test('a refused run says why', async function (assert) {
      fakes.api.fail('POST', '/api/backups', 409, 'busy');

      await visit('/apps/blog/backups');
      await click('[data-test-backup-run]');

      assert.dom('[data-test-error]').includesText('ほかの処理が実行中です');
      assert.dom('[data-test-backup-run]').isNotDisabled();
    });
  });

  module('restore', function () {
    test('restoring a volume needs the app name typed in', async function (assert) {
      fakes.api.on('POST', '/api/apps/blog/backups/restore', {
        restored: true,
      });

      await visit('/apps/blog/backups');
      await click(
        '[data-test-snapshot="4bba301e"] [data-test-snapshot-restore]',
      );

      assert
        .dom('[data-test-dialog]')
        .includesText('uploads')
        .includesText('4bba301e')
        .includesText('停止', 'it says the app is stopped meanwhile')
        .includesText('503')
        .includesText('退避');
      assert.dom('[data-test-dialog-confirm]').isDisabled();

      await fillIn('[data-test-dialog-input]', 'blo');
      assert.dom('[data-test-dialog-confirm]').isDisabled();
      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/backups/restore'),
        [],
      );

      await fillIn('[data-test-dialog-input]', 'blog');
      await click('[data-test-dialog-confirm]');

      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/backups/restore')[0]?.body,
        { snapshot_id: '4bba301e' },
      );
      assert.dom('[data-test-restored]').includesText('uploads');
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.strictEqual(
        fakes.api.callsTo('GET', '/api/apps/blog').length,
        2,
        'the app is read again: the restore stopped and started it',
      );
    });

    test('a production app is named as such', async function (assert) {
      app = { ...app, environment: 'production' };

      await visit('/apps/blog/backups');
      await click('[data-test-snapshot-restore]');

      assert.dom('[data-test-dialog]').includesText('本番アプリ');
    });

    test('a canceled or failed restore changes nothing', async function (assert) {
      await visit('/apps/blog/backups');
      await click('[data-test-snapshot-restore]');
      await click('[data-test-dialog-cancel]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/backups/restore'),
        [],
      );

      fakes.api.fail('POST', '/api/apps/blog/backups/restore', 409, 'busy');
      await click('[data-test-snapshot-restore]');
      await fillIn('[data-test-dialog-input]', 'blog');
      await click('[data-test-dialog-confirm]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.dom('[data-test-restored]').doesNotExist();
      assert.dom('[data-test-error]').includesText('ほかの処理が実行中です');
    });
  });

  module('settings', function () {
    test('every volume of the app is listed, with its newest snapshot', async function (assert) {
      await visit('/apps/blog/backups');

      assert.strictEqual(
        fakes.api.callsTo('GET', '/api/apps/blog/volumes').length,
        1,
      );
      assert.dom('[data-test-volume]').exists({ count: 3 });
      assert
        .dom('[data-test-volume="uploads"] [data-test-volume-include]')
        .isChecked();
      assert
        .dom('[data-test-volume="uploads"]')
        .includesText('最終取得', 'with the time of its newest snapshot');
      assert
        .dom('[data-test-volume="uploads"] time')
        .hasAttribute('datetime', '2026-11-17T18:00:12Z');
      assert
        .dom('[data-test-volume="pgdata"] [data-test-volume-include]')
        .isNotChecked();
      assert
        .dom('[data-test-volume="pgdata"] [data-test-volume-hint]')
        .hasText('バックアップしません');
      assert
        .dom('[data-test-volume="cache"] [data-test-volume-include]')
        .isChecked('a volume without a snapshot is listed all the same');
      assert
        .dom('[data-test-volume="cache"] [data-test-volume-hint]')
        .hasText('次回のバックアップから取ります');
      assert.dom('[data-test-volumes-error]').doesNotExist();
      assert.dom('[data-test-volumes-loading]').doesNotExist();
      assert.dom('[data-test-field="dump_hook_service"]').hasValue('db');
      assert
        .dom('[data-test-field="dump_hook_command"]')
        .hasValue('pg_dump -U postgres app');
      assert.dom('[data-test-field="backup_before_deploy"]').isNotChecked();
    });

    test('only the changed backup settings are saved', async function (assert) {
      await visit('/apps/blog/backups');

      await click('[data-test-backup-save]');
      assert.dom('[data-test-backup-unchanged]').exists();
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

      await click('[data-test-volume="uploads"] [data-test-volume-include]');
      await click('[data-test-field="backup_before_deploy"]');
      await click('[data-test-backup-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        backup_exclude_volumes: ['pgdata', 'uploads'],
        backup_before_deploy: true,
      });
      assert.dom('[data-test-backup-saved]').exists();

      // Putting one back in is a change again.
      await click('[data-test-volume="pgdata"] [data-test-volume-include]');
      assert.dom('[data-test-backup-saved]').doesNotExist();
      await click('[data-test-backup-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[1]?.body, {
        backup_exclude_volumes: ['uploads'],
      });
    });

    test('a volume that never had a snapshot is excluded with its checkbox', async function (assert) {
      await visit('/apps/blog/backups');
      await click('[data-test-volume="cache"] [data-test-volume-include]');

      assert
        .dom('[data-test-volume="cache"] [data-test-volume-hint]')
        .hasText('バックアップしません');

      await click('[data-test-backup-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        backup_exclude_volumes: ['pgdata', 'cache'],
      });
      assert.dom('[data-test-exclude-name]').hasValue('', 'nothing was typed');
      assert.strictEqual(
        fakes.api.callsTo('GET', '/api/apps/blog/volumes').length,
        2,
        'the list is read again after the save',
      );
      assert.dom('[data-test-volume]').exists({ count: 3 });
      assert
        .dom('[data-test-volume="cache"] [data-test-volume-include]')
        .isNotChecked();
    });

    test('an excluded name that matches no volume is listed and can be put back', async function (assert) {
      // `pgdata` is excluded, but Docker has no such volume (any more).
      existing = ['uploads'];

      await visit('/apps/blog/backups');

      assert.dom('[data-test-volume]').exists({ count: 2 });
      assert
        .dom('[data-test-volume="pgdata"] [data-test-volume-include]')
        .isNotChecked();
      assert
        .dom('[data-test-volume="pgdata"] [data-test-volume-hint]')
        .includesText('バックアップしません')
        .includesText('いまはありません');

      await click('[data-test-volume="pgdata"] [data-test-volume-include]');
      assert
        .dom('[data-test-volume="pgdata"] [data-test-volume-hint]')
        .includesText('作られたらバックアップします');

      await click('[data-test-backup-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        backup_exclude_volumes: [],
      });
      assert
        .dom('[data-test-volume="pgdata"]')
        .doesNotExist('no longer excluded and not a volume: off the list');
      assert.dom('[data-test-volume="uploads"]').exists();
    });

    test('a volume that does not exist yet is excluded by name', async function (assert) {
      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-field-group="backup_exclude_volumes"]')
        .includesText('一覧にない volume')
        .includesText('まだ作られていない volume');

      await fillIn('[data-test-exclude-name]', 'media');
      await click('[data-test-exclude-add]');

      assert
        .dom('[data-test-volume="media"] [data-test-volume-include]')
        .isNotChecked('a volume typed in by name is listed as excluded');
      assert
        .dom('[data-test-volume="media"] [data-test-volume-hint]')
        .includesText('いまはありません');
      assert.dom('[data-test-exclude-name]').hasValue('');

      await click('[data-test-backup-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        backup_exclude_volumes: ['pgdata', 'media'],
      });
      assert
        .dom('[data-test-volume="media"] [data-test-volume-include]')
        .isNotChecked('it stays listed: the backend reports excluded names');
    });

    test('Enter in the volume name field adds the name and does not save', async function (assert) {
      await visit('/apps/blog/backups');
      await fillIn('[data-test-exclude-name]', 'cache, tmp');
      await triggerKeyEvent('[data-test-exclude-name]', 'keydown', 'Enter');

      assert
        .dom('[data-test-volume="cache"] [data-test-volume-include]')
        .isNotChecked('the name of a listed volume unchecks that volume');
      assert.dom('[data-test-volume="cache"]').exists({ count: 1 });
      assert
        .dom('[data-test-volume="tmp"] [data-test-volume-include]')
        .isNotChecked();
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
    });

    test('skeleton lines stand in while the volume list loads', async function (assert) {
      let release: (value: unknown) => void = () => {};
      fakes.api.on(
        'GET',
        '/api/apps/blog/volumes',
        () => new Promise((resolve) => (release = resolve)),
      );

      // `visit` would wait for the request that is held open.
      void visit('/apps/blog/backups');
      await waitFor('[data-test-volumes-loading]');
      await waitFor('[data-test-snapshot]');

      assert
        .dom('[data-test-volumes-loading]')
        .hasAttribute('role', 'status')
        .hasAttribute('aria-label', 'volume の一覧を読み込み中');
      assert.dom('[data-test-volumes-loading] .skeleton').exists();
      assert.dom('[data-test-volume]').doesNotExist();
      assert.dom('[data-test-no-volumes]').doesNotExist();
      assert
        .dom('[data-test-field="dump_hook_service"]')
        .hasValue('db', 'the rest of the settings does not wait');

      release(volumeList());
      await settled();

      assert.dom('[data-test-volumes-loading]').doesNotExist();
      assert.dom('[data-test-volume]').exists({ count: 3 });
    });

    test('a failing volume list keeps the tab and shows what is known', async function (assert) {
      fakes.api.fail('GET', '/api/apps/blog/volumes', 500, 'internal');

      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-volumes-error]')
        .hasAttribute('role', 'status')
        .includesText('volume の一覧を取得できませんでした')
        .includesText('サーバーでエラーが発生しました');
      assert.dom('[data-test-error]').doesNotExist();
      assert.dom('[data-test-backup-summary]').exists();
      assert.dom('[data-test-snapshot]').exists({ count: 2 });
      // What the snapshots and the exclusions tell stands in for the list.
      assert
        .dom('[data-test-volume="uploads"] [data-test-volume-include]')
        .isChecked();
      assert
        .dom('[data-test-volume="pgdata"] [data-test-volume-include]')
        .isNotChecked();
      assert
        .dom('[data-test-volume="pgdata"] [data-test-volume-hint]')
        .hasText('バックアップしません', 'nothing is said about existence');
      assert.dom('[data-test-volume="cache"]').doesNotExist();

      // The settings can still be changed and saved.
      await click('[data-test-field="backup_before_deploy"]');
      await click('[data-test-backup-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        backup_before_deploy: true,
      });
      assert.dom('[data-test-backup-saved]').exists();
      assert.dom('[data-test-volumes-error]').exists();

      fakes.api.on('GET', '/api/apps/blog/volumes', volumeList);
      await click('[data-test-volumes-retry]');

      assert.dom('[data-test-volumes-error]').doesNotExist();
      assert.dom('[data-test-volume]').exists({ count: 3 });
      assert.dom('[data-test-volume="cache"]').exists();
    });

    test('a backend without the volume list works from the snapshots', async function (assert) {
      fakes.api.fail('GET', '/api/apps/blog/volumes', 404, 'not_found');

      await visit('/apps/blog/backups');

      assert.dom('[data-test-volumes-error]').doesNotExist();
      assert.dom('[data-test-unavailable]').doesNotExist();
      assert.dom('[data-test-volume]').exists({ count: 2 });
      assert.dom('[data-test-volume="uploads"]').exists();
      assert.dom('[data-test-volume="pgdata"]').exists();
    });

    test('an app without named volumes says so', async function (assert) {
      existing = [];
      app = { ...app, backup_exclude_volumes: [] };

      await visit('/apps/blog/backups');

      assert.dom('[data-test-volumes]').doesNotExist();
      assert
        .dom('[data-test-no-volumes]')
        .includesText('このアプリには named volume がありません');
      assert.dom('[data-test-exclude-name]').exists();
    });

    test('the dump hook is saved as a pair', async function (assert) {
      await visit('/apps/blog/backups');
      await fillIn('[data-test-field="dump_hook_command"]', '');
      await click('[data-test-backup-save]');

      assert
        .dom(
          '[data-test-field-group="dump_hook_command"] [data-test-field-error]',
        )
        .includesText('コマンドも入力してください');
      assert
        .dom('[data-test-field="dump_hook_command"]')
        .hasAttribute('aria-invalid', 'true');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

      await fillIn('[data-test-field="dump_hook_service"]', '');
      await click('[data-test-backup-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        dump_hook_service: '',
        dump_hook_command: '',
      });
    });

    test('a rejected save shows the reason next to the field', async function (assert) {
      fakes.api.fail('PATCH', '/api/apps/blog', 422, 'validation_failed', {
        dump_hook_service: 'is not a service of the app',
      });

      await visit('/apps/blog/backups');
      await fillIn('[data-test-field="dump_hook_service"]', 'database');
      await click('[data-test-backup-save]');

      assert
        .dom(
          '[data-test-field-group="dump_hook_service"] [data-test-field-error]',
        )
        .hasText('is not a service of the app');
      assert.dom('[data-test-error]').hasText('入力内容に誤りがあります。');
      assert.dom('[data-test-backup-saved]').doesNotExist();
    });

    test('a backend that does not report the settings gets none', async function (assert) {
      app = makeApp();

      await visit('/apps/blog/backups');

      assert.dom('[data-test-backup-settings-unavailable]').exists();
      assert.dom('[data-test-backup-form]').doesNotExist();
    });
  });

  module('empty, missing and failing', function () {
    test('without snapshots there is exactly one next step', async function (assert) {
      snapshots = [];
      existing = [];
      app = { ...app, backup_exclude_volumes: [] };
      fakes.api.on(
        'GET',
        '/api/backups',
        makeBackups({ last_success_at: null, warning: true, runs: [] }),
      );

      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-no-snapshots]')
        .includesText('スナップショットはまだありません');
      assert
        .dom('[data-test-backup-run]')
        .exists({ count: 1 }, 'the one action is in the empty state');
      assert.dom('[data-test-no-snapshots] [data-test-backup-run]').exists();
      assert.dom('[data-test-backup-last-success]').hasText('まだありません');
      assert.dom('[data-test-backup-state]').hasText('まだ成功していません');
      assert.dom('[data-test-no-volumes]').exists();
      assert.dom('[data-test-dump-help]').doesNotExist();
    });

    test('backups that are not configured say what is missing', async function (assert) {
      fakes.api
        .on(
          'GET',
          '/api/backups',
          makeBackups({
            configured: false,
            last_success_at: null,
            next_run_at: null,
            runs: [],
          }),
        )
        .fail('GET', '/api/apps/blog/backups', 503, 'not_configured');

      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-backups-unconfigured]')
        .includesText('バックアップは設定されていません')
        .includesText('[storage]')
        .includesText('/etc/koroku/restic.key');
      assert.dom('[data-test-backup-run]').doesNotExist();
      assert.dom('[data-test-backup-form]').doesNotExist();
      assert.dom('[data-test-error]').doesNotExist();
      assert.deepEqual(
        fakes.api.callsTo('GET', '/api/apps/blog/backups'),
        [],
        'the snapshots are not asked for',
      );
      assert.deepEqual(
        fakes.api.callsTo('GET', '/api/apps/blog/volumes'),
        [],
        'nor are the volumes: there are no settings to show them in',
      );
      assert.dom('[data-test-tab="backups"]').hasClass('active');

      // Once the server is set up, checking again shows the tab.
      fakes.api
        .on('GET', '/api/backups', makeBackups())
        .on('GET', '/api/apps/blog/backups', { snapshots: [VOLUME] });
      await click('[data-test-backups-recheck]');

      assert.dom('[data-test-backups-unconfigured]').doesNotExist();
      assert.dom('[data-test-snapshot="4bba301e"]').exists();
      assert.dom('[data-test-volume]').exists({ count: 3 });
    });

    test('a failing status offers a retry', async function (assert) {
      fakes.api.fail('GET', '/api/backups', 500, 'internal');

      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-error]')
        .includesText('バックアップの状況を取得できませんでした');
      assert.dom('[data-test-backup-summary]').doesNotExist();
    });

    test('a failing snapshot list keeps the rest of the tab', async function (assert) {
      fakes.api.fail('GET', '/api/apps/blog/backups', 500, 'internal');

      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-error]')
        .includesText('スナップショットの一覧を取得できませんでした');
      assert.dom('[data-test-backup-summary]').exists();
      assert.dom('[data-test-backup-form]').exists();

      fakes.api.on('GET', '/api/apps/blog/backups', { snapshots: [DUMP] });
      await click('[data-test-error] button');

      assert.dom('[data-test-snapshot="77aa00ff"]').exists();
      assert.dom('[data-test-error]').doesNotExist();
    });

    test('a backend without backups renders the placeholder', async function (assert) {
      fakes.api.fail('GET', '/api/backups', 404, 'not_found');

      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-unavailable="unavailable"]')
        .includesText('この機能はまだ利用できません');
      assert.dom('[data-test-error]').doesNotExist();
    });

    test('a refresh that fails keeps the last state and says so', async function (assert) {
      let down = false;
      fakes.api.on('GET', '/api/backups', () => {
        if (down) throw new Error('offline');
        return makeBackups({ runs: [run()] });
      });

      await visit('/apps/blog/backups');
      down = true;
      await fakes.poller.tick();
      await settled();

      assert.dom('[data-test-stale]').exists();
      assert.dom('[data-test-backup-state]').hasText('正常');
      assert.dom('[data-test-snapshot]').exists({ count: 2 });
    });
  });
});
