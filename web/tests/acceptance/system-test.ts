import { module, test } from 'qunit';
import { click, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeBackups,
  makeSystem,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type {
  BackupRun,
  DockerUsageItem,
  SystemInfo,
} from 'koroku-console/utils/types';

const GB = 1024 ** 3;

function dockerUsage(reclaimable = true): { items: DockerUsageItem[] } {
  return {
    items: [
      {
        type: 'containers',
        total_bytes: 1048576,
        reclaimable_bytes: 0,
        count: 6,
      },
      {
        type: 'images',
        total_bytes: 5 * GB,
        reclaimable_bytes: reclaimable ? GB : 0,
        count: 12,
      },
      {
        type: 'volumes',
        total_bytes: 2 * GB,
        reclaimable_bytes: 0,
        count: 4,
      },
      {
        type: 'build_cache',
        total_bytes: reclaimable ? 3 * GB : 0,
        reclaimable_bytes: reclaimable ? 3 * GB : 0,
        count: reclaimable ? 58 : 0,
      },
    ],
  };
}

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

module('Acceptance | system', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let system: SystemInfo;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    system = makeSystem({
      swap: { total_bytes: 2 * GB, used_bytes: 0.5 * GB },
      log_archive: {
        configured: true,
        last_success_at: '2026-11-18T03:10:05Z',
        last_error: '',
        next_run_at: '2026-11-18T07:10:00Z',
      },
    });
    fakes.api
      .on('GET', '/api/system', () => system)
      .on('GET', '/api/system/docker', dockerUsage())
      .on('GET', '/api/backups', makeBackups({ runs: [run()] }));
  });

  module('resources', function () {
    test('disk, memory and swap are plain numbers with a meter', async function (assert) {
      await visit('/system');

      assert
        .dom('[data-test-resource="disk"]')
        .includesText('21.5%')
        .includesText('8.6 GB / 40.0 GB')
        .includesText('上限 85%');
      assert
        .dom('[data-test-resource="disk"] [role="meter"]')
        .hasAttribute('aria-valuenow', '21.5')
        .hasAttribute('aria-valuetext', '21.5%')
        .hasAttribute('aria-label', 'ディスクの使用率');
      assert
        .dom('[data-test-resource="disk"] .meter-bar-mark')
        .exists('the deploy limit is marked on the bar');
      assert
        .dom('[data-test-resource="memory"]')
        .includesText('50%')
        .includesText('2.0 GB / 4.0 GB')
        .includesText('空き 2.0 GB');
      assert
        .dom('[data-test-resource="swap"]')
        .includesText('25%')
        .includesText('512.0 MB / 2.0 GB');
      assert.dom('[data-test-resource-flag]').doesNotExist('quiet when fine');
      assert.dom('[data-test-disk-warning]').doesNotExist();
      assert.dom('[data-test-koroku-version]').hasText('v0.1.0');
      assert.dom('[data-test-docker-version]').hasText('29.8.0');
    });

    test('a disk above the deploy limit stands out', async function (assert) {
      system = makeSystem({
        disk: { total_bytes: 100, used_bytes: 90, used_percent: 90 },
        disk_limit_percent: 80,
      });

      await visit('/system');

      assert
        .dom('[data-test-disk-warning]')
        .includesText('ディスク使用率が上限を超えています')
        .includesText('80%')
        .includesText('デプロイを受け付けません');
      assert
        .dom('[data-test-resource="disk"]')
        .hasAttribute('data-tone', 'danger');
      assert
        .dom('[data-test-resource="disk"] [data-test-resource-flag]')
        .hasText('上限超過', 'said in words, not by colour alone');
    });

    test('memory that is nearly used up is flagged', async function (assert) {
      system = makeSystem({
        memory: { total_bytes: 4 * GB, available_bytes: 0.2 * GB },
      });

      await visit('/system');

      assert
        .dom('[data-test-resource="memory"] [data-test-resource-flag]')
        .hasText('残りわずか');
      assert
        .dom('[data-test-resource="memory"]')
        .hasAttribute('data-tone', 'warn');
    });

    test('values the host did not report degrade gracefully', async function (assert) {
      system = {
        version: 'v0.1.0',
        base_domain: 'dev.example.com',
        disk: null,
        memory: null,
        docker_version: '',
      };

      await visit('/system');

      assert.dom('[data-test-resource="disk"]').includesText('取得できません');
      assert
        .dom('[data-test-resource="memory"]')
        .includesText('取得できません');
      assert
        .dom('[data-test-resource="swap"]')
        .doesNotExist('an older backend does not report swap');
      assert.dom('[data-test-docker-version]').hasText('取得できません');
      assert.dom('[data-test-job="log-archive"]').doesNotExist();
      assert.dom('[data-test-job="caddy"]').doesNotExist();
    });

    test('a host without swap says so', async function (assert) {
      system = makeSystem({ swap: null });

      await visit('/system');

      assert.dom('[data-test-resource="swap"]').includesText('ありません');
    });

    test('a failing host status offers a retry', async function (assert) {
      fakes.api.fail('GET', '/api/system', 500, 'internal');

      await visit('/system');

      assert
        .dom('[data-test-error]')
        .includesText('ホストの状態を取得できませんでした');
      assert.dom('[data-test-docker]').exists('the rest still renders');

      fakes.api.on('GET', '/api/system', makeSystem());
      await click('[data-test-error] button');

      assert.dom('[data-test-resource="disk"]').exists();
      assert.dom('[data-test-error]').doesNotExist();
    });
  });

  module('Docker and prune', function () {
    test('the disk usage is broken down by kind', async function (assert) {
      await visit('/system');

      assert.dom('[data-test-docker-item]').exists({ count: 4 });
      assert
        .dom('[data-test-docker-item="images"]')
        .includesText('イメージ')
        .includesText('12')
        .includesText('5.0 GB')
        .includesText('1.0 GB');
      assert
        .dom('[data-test-docker-item="build_cache"]')
        .includesText('ビルドキャッシュ')
        .includesText('3.0 GB');
      assert
        .dom('[data-test-docker-item="volumes"] .docker-reclaimable')
        .includesText('—', 'volumes are never pruned');
      assert.dom('[data-test-docker-item="volumes"]').includesText('volume');
      assert
        .dom('[data-test-docker-item="containers"]')
        .includesText('コンテナ')
        .includesText('1.0 MB');
      assert.dom('[data-test-docker-total]').includesText('合計 10.0 GB');
    });

    test('pruning asks first, then refreshes the numbers', async function (assert) {
      let pruned = false;
      fakes.api
        .on('GET', '/api/system/docker', () => dockerUsage(!pruned))
        .on('POST', '/api/system/prune', () => {
          pruned = true;
          return {
            output: 'Total reclaimed space: 4.0GB',
            reclaimed_bytes: 4 * GB,
          };
        });

      await visit('/system');
      await click('[data-test-prune-open]');

      assert
        .dom('[data-test-dialog]')
        .includesText('不要なデータを削除しますか');
      assert.dom('[data-test-prune-images]').isChecked();
      assert.dom('[data-test-prune-builder]').isChecked();
      assert.deepEqual(fakes.api.callsTo('POST', '/api/system/prune'), []);

      await click('[data-test-prune-builder]');
      await click('[data-test-dialog-confirm]');

      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/system/prune')[0]?.body,
        { images: true, builder: false },
      );
      assert.dom('[data-test-dialog]').doesNotExist();
      assert
        .dom('[data-test-pruned]')
        .hasText(
          '不要なデータを削除しました。Docker の報告では 4.0 GB 空きました。',
        );
      assert
        .dom('[data-test-prune-output]')
        .hasText('Total reclaimed space: 4.0GB');
      assert
        .dom('[data-test-docker-item="build_cache"]')
        .includesText('0 B', 'the usage was read again');
      assert.strictEqual(
        fakes.api.callsTo('GET', '/api/system').length,
        2,
        'and so was the disk',
      );
    });

    test('a canceled prune does nothing; an empty choice is refused', async function (assert) {
      fakes.api.on('POST', '/api/system/prune', { output: '' });

      await visit('/system');
      await click('[data-test-prune-open]');
      await click('[data-test-dialog-cancel]');

      assert.dom('[data-test-dialog]').doesNotExist();

      await click('[data-test-prune-open]');
      await click('[data-test-prune-images]');
      await click('[data-test-prune-builder]');
      await click('[data-test-dialog-confirm]');

      assert.dom('[data-test-prune-nothing]').exists();
      assert.dom('[data-test-dialog]').exists();
      assert.deepEqual(fakes.api.callsTo('POST', '/api/system/prune'), []);
    });

    test('a failed prune shows what the command printed', async function (assert) {
      fakes.api.fail('POST', '/api/system/prune', 500, 'prune_failed', {
        output: 'Cannot connect to the Docker daemon',
      });

      await visit('/system');
      await click('[data-test-prune-open]');
      await click('[data-test-dialog-confirm]');

      assert.dom('[data-test-error]').hasText('prune に失敗しました。');
      assert
        .dom('[data-test-prune-output]')
        .hasText('Cannot connect to the Docker daemon');
      assert.dom('[data-test-pruned]').doesNotExist();
    });

    test('a backend without prune or disk usage says so', async function (assert) {
      fakes.api.fail('GET', '/api/system/docker', 404, 'not_found');

      await visit('/system');

      assert
        .dom('[data-test-unavailable="unavailable"]')
        .exists({ count: 1 }, 'the usage is a placeholder');

      await click('[data-test-prune-open]');
      await click('[data-test-dialog-confirm]');

      assert.dom('[data-test-prune-unavailable]').exists();
      assert.dom('[data-test-error]').doesNotExist();
    });

    test('a failing disk usage offers a retry', async function (assert) {
      fakes.api.fail('GET', '/api/system/docker', 500, 'internal');

      await visit('/system');

      assert
        .dom('[data-test-error]')
        .includesText('Docker のディスク使用量を取得できませんでした');
      assert.dom('[data-test-resource="disk"]').exists();
    });
  });

  module('background jobs', function () {
    test('backup, log archive and Caddy report last and next run', async function (assert) {
      await visit('/system');

      assert.dom('[data-test-backup-state]').hasText('正常');
      assert
        .dom('[data-test-backup-last-success] time')
        .hasAttribute('datetime', '2026-11-17T18:00:40Z');
      assert
        .dom('[data-test-backup-next-run] time')
        .hasAttribute('datetime', '2026-11-18T18:00:00Z')
        .hasAttribute('title', '2026/11/19 03:00 JST');
      assert.dom('[data-test-backup-error]').doesNotExist();

      assert.dom('[data-test-archive-state]').hasText('正常');
      assert
        .dom(
          '[data-test-job="log-archive"] time[datetime="2026-11-18T03:10:05Z"]',
        )
        .exists();
      assert
        .dom('[data-test-archive-next-run] time')
        .hasAttribute('datetime', '2026-11-18T07:10:00Z');

      assert.dom('[data-test-caddy-state]').hasText('正常');
      assert
        .dom('[data-test-job="caddy"] time[datetime="2026-11-18T03:00:02Z"]')
        .exists();
      assert.dom('[data-test-job] [data-status="failed"]').doesNotExist();
    });

    test('failures of the jobs stand out with their reason', async function (assert) {
      system = makeSystem({
        log_archive: {
          configured: true,
          last_success_at: '2026-11-17T03:10:05Z',
          last_error: 'put logs/blog/web/2026/11/18/00.log.gz: access denied',
          next_run_at: '2026-11-18T07:10:00Z',
        },
        caddy: {
          enabled: true,
          last_loaded_at: '2026-11-18T03:00:02Z',
          last_error: 'load caddy config: connection refused',
        },
      });
      fakes.api.on(
        'GET',
        '/api/backups',
        makeBackups({
          warning: true,
          runs: [
            run({ id: 14, status: 'failed', error: 'repository is locked' }),
            run({ id: 13, trigger: 'pre_deploy' }),
          ],
        }),
      );

      await visit('/system');

      assert.dom('[data-test-backup-state]').hasText('直近の実行が失敗');
      assert
        .dom('[data-test-backup-error]')
        .includesText('repository is locked');
      assert.dom('[data-test-archive-state]').hasText('直近の実行が失敗');
      assert.dom('[data-test-archive-error]').includesText('access denied');
      assert.dom('[data-test-caddy-state]').hasText('投入に失敗');
      assert.dom('[data-test-caddy-error]').includesText('connection refused');
      assert.dom('[data-test-job] [data-status="failed"]').exists({ count: 3 });

      assert
        .dom('[data-test-backup-run-row="14"]')
        .includesText('失敗')
        .includesText('定期')
        .includesText('repository is locked');
      assert
        .dom('[data-test-backup-run-row="13"]')
        .includesText('成功')
        .includesText('デプロイ前')
        .includesText('40 秒');
    });

    test('a stale backup is a warning', async function (assert) {
      fakes.api.on(
        'GET',
        '/api/backups',
        makeBackups({ warning: true, runs: [run()] }),
      );

      await visit('/system');

      assert.dom('[data-test-backup-state]').hasText('24 時間以上成功なし');
      assert
        .dom('[data-test-backup-state] [data-status]')
        .hasAttribute('data-status', 'pending');
    });

    test('a manual backup shows as running until it ends', async function (assert) {
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

      await visit('/system');
      await click('[data-test-backup-run]');

      assert.strictEqual(fakes.api.callsTo('POST', '/api/backups').length, 1);
      assert.dom('[data-test-backup-state]').hasText('実行中');
      assert.dom('[data-test-backup-run]').isDisabled().hasText('実行中…');
      assert.dom('[data-test-backup-run-row="13"]').includesText('実行中');

      running = false;
      await fakes.poller.tick();
      await settled();

      assert.dom('[data-test-backup-state]').hasText('正常');
      assert.dom('[data-test-backup-run]').isNotDisabled();
    });

    test('a refused backup says why', async function (assert) {
      fakes.api.fail('POST', '/api/backups', 409, 'busy');

      await visit('/system');
      await click('[data-test-backup-run]');

      assert.dom('[data-test-error]').includesText('ほかの処理が実行中です');
    });

    test('what is not configured says so plainly', async function (assert) {
      system = makeSystem({
        log_archive: {
          configured: false,
          last_success_at: null,
          last_error: '',
        },
        caddy: { enabled: false, last_loaded_at: null, last_error: '' },
      });
      fakes.api.on(
        'GET',
        '/api/backups',
        makeBackups({
          configured: false,
          last_success_at: null,
          next_run_at: null,
          runs: [],
        }),
      );

      await visit('/system');

      assert.dom('[data-test-backup-state]').hasText('未設定');
      assert
        .dom('[data-test-backup-unconfigured]')
        .includesText('[storage]')
        .includesText('/etc/koroku/restic.key');
      assert.dom('[data-test-backup-run]').doesNotExist();
      assert.dom('[data-test-backup-runs]').doesNotExist();
      assert.dom('[data-test-archive-state]').hasText('未設定');
      assert.dom('[data-test-job="log-archive"]').includesText('14 日分');
      assert.dom('[data-test-caddy-state]').hasText('無効');
      assert.dom('[data-test-job] [data-status="failed"]').doesNotExist();
    });

    test('a backup status that cannot be read keeps the rest', async function (assert) {
      fakes.api.fail('GET', '/api/backups', 500, 'internal');

      await visit('/system');

      assert.dom('[data-test-backup-load-error]').exists();
      assert.dom('[data-test-archive-state]').hasText('正常');

      fakes.api.on('GET', '/api/backups', makeBackups({ runs: [] }));
      await click('[data-test-job="backup"] button');

      assert.dom('[data-test-backup-state]').hasText('正常');
      assert.dom('[data-test-no-runs]').exists();
    });

    test('a backend without backups says so', async function (assert) {
      fakes.api.fail('GET', '/api/backups', 404, 'not_found');

      await visit('/system');

      assert.dom('[data-test-backup-unavailable]').exists();
      assert.dom('[data-test-backup-run]').doesNotExist();
    });

    test('a refresh that fails keeps the last figures and says so', async function (assert) {
      let down = false;
      fakes.api.on('GET', '/api/system', () => {
        if (down) throw new Error('offline');
        return system;
      });

      await visit('/system');
      down = true;
      await fakes.poller.tick();
      await settled();

      assert.dom('[data-test-stale]').exists();
      assert.dom('[data-test-resource="disk"]').includesText('21.5%');
    });
  });
});
