import { module, test } from 'qunit';
import { click, fillIn, select, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeAppBackupStatus,
  makeBackups,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type {
  App,
  AppBackupStatus,
  DbCopy,
  Snapshot,
} from 'koroku-console/utils/types';

const DUMP: Snapshot = {
  id: '77aa00ff11223344',
  time: '2026-11-17T09:00:12Z',
  kind: 'dump',
  target: 'db',
  size_bytes: 4194304,
};

const OTHER_DUMP: Snapshot = { ...DUMP, id: '99bb00ff', target: 'mysql' };

const VOLUME: Snapshot = {
  id: '4bba301e',
  time: '2026-11-17T18:00:12Z',
  kind: 'volume',
  target: 'uploads',
  size_bytes: 1024,
};

function makeRestore(overrides: Partial<DbCopy> = {}): DbCopy {
  return {
    id: 9,
    target_app: 'blog',
    source_app: 'blog',
    source_kind: 'snapshot',
    kind: 'restore',
    source_snapshot_id: DUMP.id,
    status: 'running',
    error: '',
    bytes: 0,
    backup_snapshot_id: '',
    started_at: '2026-11-18T01:00:00Z',
    finished_at: null,
    ...overrides,
  };
}

const LOG = [
  '==> 2026-11-18T01:00:00Z check Checking blog',
  '==> 2026-11-18T01:00:01Z backup Backing up the current data of blog',
  'Stored the current data as snapshot 4bba301e.',
  '==> 2026-11-18T01:00:03Z stop Stopping the other services of blog',
  '==> 2026-11-18T01:00:04Z load Loading the dump of 2026-11-17T09:00:12Z',
  'load | pg_restore: error: input file is too short',
  'Loading the dump failed: the load command failed: exit status 1',
  '==> 2026-11-18T01:00:09Z reload Loading the data of blog back from snapshot 4bba301e',
  'Loaded the data before the restore back.',
  '==> 2026-11-18T01:00:20Z failed Restore failed: the load command failed: exit status 1',
];

module('Acceptance | restoring a dump', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let status: AppBackupStatus;
  let copies: DbCopy[];

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
      backup_exclude_volumes: [],
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -Fc -Z0 app',
      backup_before_deploy: false,
      dump_interval_hours: 6,
      load_hook_service: 'db',
      load_hook_command: 'pg_restore --clean -d app',
      post_load_service: '',
      post_load_command: '',
    });
    status = makeAppBackupStatus({ dump_warning: false, dump_slow: false });
    copies = [];
    fakes.api
      .on('GET', '/api/apps', () => ({ apps: [app] }))
      .on('GET', '/api/apps/blog', () => app)
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
        app = { ...app, ...(call.body as Partial<App>) };
        return app;
      })
      .on('GET', '/api/backups', makeBackups())
      .on('GET', '/api/apps/blog/backups/status', () => status)
      .on('GET', '/api/apps/blog/backups', () => ({
        snapshots: [VOLUME, DUMP, OTHER_DUMP],
      }))
      .on('GET', '/api/apps/blog/volumes', { volumes: [] })
      .on('GET', '/api/apps/blog/db-copies', () => ({ copies }));
  });

  test('the dump interval is chosen with the dump hook and saved', async function (assert) {
    await visit('/apps/blog/backups');
    assert.dom('[data-test-field="dump_interval_hours"]').hasValue('6');
    await select('[data-test-field="dump_interval_hours"]', '1');
    await click('[data-test-backup-save]');
    const patch = fakes.api.callsTo('PATCH', '/api/apps/blog')[0];
    assert.deepEqual(patch?.body, { dump_interval_hours: 1 });
    assert.dom('[data-test-field="dump_interval_hours"]').hasValue('1');
  });

  test('late and slow dumps are pointed out', async function (assert) {
    status = makeAppBackupStatus(
      { dump_warning: true, dump_slow: true },
      {
        trigger: 'interval',
        targets: [
          {
            kind: 'dump',
            target: 'db',
            status: 'succeeded',
            error: '',
            finished_at: '2026-11-18T00:00:00Z',
            duration_ms: 200 * 60 * 1000,
          },
        ],
      },
    );
    await visit('/apps/blog/backups');
    assert
      .dom('[data-test-dump-late]')
      .includesText('ダンプが 12 時間以上取れていません')
      .includesText('6 時間ごと');
    assert
      .dom('[data-test-dump-slow]')
      .includesText('約 200 分')
      .includesText('間隔を長くしてください');
  });

  test('a dump of the dump hook service restores through a dialog', async function (assert) {
    fakes.api.on('POST', '/api/apps/blog/db-restores', () => {
      copies = [makeRestore()];
      return copies[0]!;
    });
    await visit('/apps/blog/backups');

    assert
      .dom(
        `[data-test-snapshot="${DUMP.id}"] [data-test-snapshot-restore-dump]`,
      )
      .exists()
      .hasAttribute('aria-label', 'db の DB をこのダンプの時点に戻す');
    assert
      .dom(
        `[data-test-snapshot="${OTHER_DUMP.id}"] [data-test-snapshot-restore-dump]`,
      )
      .doesNotExist('a dump of another service is not loaded back');
    assert
      .dom(
        `[data-test-snapshot="${VOLUME.id}"] [data-test-snapshot-restore-dump]`,
      )
      .doesNotExist();
    assert.dom('[data-test-restore-empty]').exists();

    await click(
      `[data-test-snapshot="${DUMP.id}"] [data-test-snapshot-restore-dump]`,
    );
    assert
      .dom('[data-test-restore-dialog-target]')
      .includesText('2026/11/17 18:00 JST')
      .includesText('77aa00ff');
    assert
      .dom('[data-test-dialog]')
      .includesText('db 以外のサービスを止めます')
      .includesText('いまの DB のダンプを取って');
    // A staging app is not asked for its slug.
    assert.dom('[data-test-dialog-input]').doesNotExist();
    await click('[data-test-dialog-confirm]');

    const call = fakes.api.callsTo('POST', '/api/apps/blog/db-restores')[0];
    assert.deepEqual(call?.body, { snapshot_id: DUMP.id, confirm: 'blog' });
    assert.dom('[data-test-restore="9"]').includesText('戻しています');
    assert.ok(
      fakes.eventSource.last.url.endsWith('/api/db-copies/9/logs'),
      'the log of the restore is followed',
    );

    copies = [
      makeRestore({
        status: 'failed',
        error: 'the load command failed: exit status 1',
        backup_snapshot_id: '4bba301e5d2c7a90',
        finished_at: '2026-11-18T01:00:20Z',
      }),
    ];
    for (const line of LOG) fakes.eventSource.last.emit(line);
    fakes.eventSource.last.end('failed');
    await settled();
    assert
      .dom('[data-test-copy-failed]')
      .includesText('ダンプから戻せませんでした')
      .includesText('「ダンプの流し込み」の手順で失敗しました。')
      .includesText('戻す前のデータはスナップショット');
    assert.dom('[data-test-copy-detail]').includesText('元のデータの流し込み');
    assert.dom('[data-test-copy-left-stopped]').doesNotExist();
  });

  test('a production app asks for its slug and sets its load hook here', async function (assert) {
    app = {
      ...app,
      environment: 'production',
      load_hook_service: '',
      load_hook_command: '',
    };
    await visit('/apps/blog/backups');
    assert
      .dom('[data-test-snapshot-restore-dump]')
      .doesNotExist('nothing to load the dump with');
    assert.dom('[data-test-restore-problem]').includesText('ロードフック');

    await fillIn(
      '[data-test-restore-settings] [data-test-field="load_hook_service"]',
      'db',
    );
    await fillIn(
      '[data-test-restore-settings] [data-test-field="load_hook_command"]',
      'pg_restore --clean -d app',
    );
    await click('[data-test-restore-settings-save]');
    const patch = fakes.api.callsTo('PATCH', '/api/apps/blog')[0];
    assert.deepEqual(patch?.body, {
      load_hook_service: 'db',
      load_hook_command: 'pg_restore --clean -d app',
    });

    await click(
      `[data-test-snapshot="${DUMP.id}"] [data-test-snapshot-restore-dump]`,
    );
    assert.dom('[data-test-dialog-confirm]').isDisabled();
    await fillIn('[data-test-dialog-input]', 'blog');
    assert.dom('[data-test-dialog-confirm]').isNotDisabled();
  });

  test('restores stay out of the copy history of a staging app', async function (assert) {
    copies = [
      makeRestore({ status: 'succeeded' }),
      {
        ...makeRestore({ id: 7, status: 'succeeded' }),
        kind: 'copy',
        source_app: 'blog-production',
        source_kind: 'live',
      },
    ];
    await visit('/apps/blog/backups');
    assert.dom('[data-test-restore]').exists({ count: 1 });
    assert.dom('[data-test-restore="9"]').exists();
    assert.dom('.copy-rows [data-test-copy]').exists({ count: 1 });
    assert.dom('[data-test-copy="7"]').exists();
  });

  test('a member who views only sees no restore', async function (assert) {
    app = { ...app, role: 'view' };
    await visit('/apps/blog/backups');
    assert.dom('[data-test-snapshot-restore-dump]').doesNotExist();
    assert.dom('[data-test-restore-settings]').doesNotExist();
  });
});
