import { module, test } from 'qunit';
import { click, fillIn, visit } from '@ember/test-helpers';
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
import type { App, PitrStatus } from 'koroku-console/utils/types';

function status(overrides: Partial<PitrStatus> = {}): PitrStatus {
  return {
    service: '',
    state: 'off',
    error: '',
    warning: '',
    earliest_at: null,
    archived_until: null,
    gaps: [],
    ...overrides,
  };
}

module('Acceptance | point-in-time recovery', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let pitr: PitrStatus;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
      backup_exclude_volumes: [],
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump app',
      backup_before_deploy: false,
      pitr_service: '',
    });
    pitr = status();
    fakes.api
      .on('GET', '/api/apps', () => ({ apps: [app] }))
      .on('GET', '/api/apps/blog', () => app)
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
        app = { ...app, ...(call.body as Partial<App>) };
        pitr = status({
          service: app.pitr_service ?? '',
          state: app.pitr_service ? 'waiting' : 'off',
        });
        return app;
      })
      .on('GET', '/api/backups', makeBackups())
      .on('GET', '/api/apps/blog/backups/status', makeAppBackupStatus())
      .on('GET', '/api/apps/blog/backups', { snapshots: [] })
      .on('GET', '/api/apps/blog/volumes', { volumes: [] })
      .on('GET', '/api/apps/blog/pitr', () => pitr);
  });

  test('a staging app turns it on without a dialog and waits for a deployment', async function (assert) {
    await visit('/apps/blog/backups');
    assert.dom('[data-test-pitr-state]').hasText('使っていません');
    await fillIn('[data-test-field="pitr_service"]', 'db');
    await click('[data-test-pitr-save]');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      pitr_service: 'db',
    });
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.dom('[data-test-pitr-state]').hasText('次のデプロイから');
    assert.dom('[data-test-pitr-waiting]').includesText('次のデプロイ');
  });

  test('a production app is told that the database restarts', async function (assert) {
    app = { ...app, environment: 'production' };
    await visit('/apps/blog/backups');
    await fillIn('[data-test-field="pitr_service"]', 'db');
    await click('[data-test-pitr-save]');
    assert
      .dom('[data-test-dialog]')
      .includesText('時点指定の復旧を使いますか？')
      .includesText('数秒止まります');
    assert.strictEqual(fakes.api.callsTo('PATCH', '/api/apps/blog').length, 0);
    await click('[data-test-dialog-confirm]');
    assert.strictEqual(fakes.api.callsTo('PATCH', '/api/apps/blog').length, 1);

    // Turning it off restarts the database at once.
    await fillIn('[data-test-field="pitr_service"]', '');
    await click('[data-test-pitr-save]');
    assert
      .dom('[data-test-dialog]')
      .includesText('時点指定の復旧をやめますか？')
      .includesText('すぐに 1 回再起動');
  });

  test('a ready service shows its range, the shipped WAL and the gaps', async function (assert) {
    app = { ...app, pitr_service: 'db' };
    pitr = status({
      service: 'db',
      state: 'ready',
      earliest_at: '2026-11-21T03:00:00Z',
      archived_until: '2026-11-28T02:20:00Z',
      warning: 'PostgreSQL failed to archive WAL 2 times (pg_stat_archiver).',
      gaps: [
        {
          from: '2026-11-25T01:00:00Z',
          to: '2026-11-25T04:00:00Z',
          reason: 'mount_missing',
        },
        { from: '2026-11-27T10:00:00Z', to: null, reason: 'dropped' },
      ],
    });
    await visit('/apps/blog/backups');
    assert.dom('[data-test-pitr-state]').hasText('有効');
    assert.dom('[data-test-pitr-range]').includesText('2026/11/21 12:00 JST');
    assert.dom('[data-test-pitr-archived]').exists();
    assert.dom('[data-test-pitr-warning]').includesText('pg_stat_archiver');
    assert
      .dom('[data-test-pitr-gap="mount_missing"]')
      .includesText('2026/11/25 10:00 JST')
      .includesText('WAL の受け渡しがないリリース');
    assert.dom('[data-test-pitr-gap="dropped"]').includesText('現在');
  });

  test('a failed preparation says why', async function (assert) {
    app = { ...app, pitr_service: 'db' };
    pitr = status({
      service: 'db',
      state: 'failed',
      error: 'the data directory /data is not in a named volume',
    });
    await visit('/apps/blog/backups');
    assert
      .dom('[data-test-pitr-error]')
      .includesText('named volume')
      .includesText('もう一度デプロイ');
  });

  test('a member who views cannot change it', async function (assert) {
    app = { ...app, role: 'view', pitr_service: 'db' };
    pitr = status({ service: 'db', state: 'ready' });
    await visit('/apps/blog/backups');
    assert.dom('[data-test-pitr-form] fieldset').isDisabled();
    assert.dom('[data-test-pitr] [data-test-role-note="manage"]').exists();
  });
});

module('Acceptance | point-in-time recovery | restore', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
      backup_exclude_volumes: [],
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump app',
      backup_before_deploy: false,
      pitr_service: 'db',
      environment: 'production',
    });
    let restores: unknown[] = [];
    fakes.api
      .on('GET', '/api/apps', () => ({ apps: [app] }))
      .on('GET', '/api/apps/blog', () => app)
      .on('GET', '/api/backups', makeBackups())
      .on('GET', '/api/apps/blog/backups/status', makeAppBackupStatus())
      .on('GET', '/api/apps/blog/backups', { snapshots: [] })
      .on('GET', '/api/apps/blog/volumes', { volumes: [] })
      .on('GET', '/api/apps/blog/pitr', () =>
        status({
          service: 'db',
          state: 'ready',
          earliest_at: '2026-11-21T03:00:00Z',
          archived_until: '2026-11-28T02:00:00Z',
        }),
      )
      .on('GET', '/api/apps/blog/pitr/restores', () => ({ restores }))
      .on('POST', '/api/apps/blog/pitr/restores', (call: FakeCall) => {
        const body = call.body as { target_time: string };
        restores = [
          {
            id: 4,
            service: 'db',
            target_time: body.target_time,
            base_snapshot_id: '9fceb02d',
            trash_path:
              '/var/lib/koroku/restore-trash/blog-pitr-20261128T030000Z',
            status: 'running',
            error: '',
            started_at: '2026-11-28T03:00:00Z',
            finished_at: null,
          },
        ];
        return restores[0];
      });
  });

  test('a time is restored to after the slug is typed', async function (assert) {
    await visit('/apps/blog/backups');
    await click('[data-test-pitr-restore-start]');
    assert
      .dom('[data-test-field-group="target_time"]')
      .includesText('日時を入力してください');
    assert.dom('[data-test-dialog]').doesNotExist();

    await fillIn('[data-test-field="target_time"]', '2026-11-27T10:15:30');
    await click('[data-test-pitr-restore-start]');
    assert
      .dom('[data-test-pitr-restore-dialog-target]')
      .includesText('2026/11/27 10:15 JST');
    assert.dom('[data-test-dialog]').includesText('503').includesText('7 日間');
    assert.dom('[data-test-dialog-confirm]').isDisabled();
    await fillIn('[data-test-dialog-input]', 'blog');
    await click('[data-test-dialog-confirm]');

    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/pitr/restores')[0]?.body,
      { target_time: '2026-11-27T01:15:30.000Z', confirm: 'blog' },
    );
    assert.dom('[data-test-pitr-restore="4"]').includesText('戻しています');
    assert.ok(
      fakes.eventSource.last.url.endsWith(
        '/api/apps/blog/pitr/restores/4/logs',
      ),
    );
  });

  test('a time before the range is refused before asking', async function (assert) {
    await visit('/apps/blog/backups');
    await fillIn('[data-test-field="target_time"]', '2026-11-01T00:00');
    await click('[data-test-pitr-restore-start]');
    assert
      .dom('[data-test-field-group="target_time"]')
      .includesText('戻せる期間より前');
    assert.dom('[data-test-dialog]').doesNotExist();
  });
});
