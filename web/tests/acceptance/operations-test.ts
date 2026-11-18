import { module, test } from 'qunit';
import { click, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';
import type { App } from 'paas-console/utils/types';

module('Acceptance | overview, backups, system', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
    });
    fakes.api.on('GET', '/api/apps/blog', () => app);
  });

  module('overview', function () {
    test('it shows the state, the deployment and the services', async function (assert) {
      fakes.api.on('GET', '/api/apps/blog/services', {
        services: [
          {
            name: 'web',
            state: 'running',
            status: 'Up 2 hours',
            health: 'healthy',
          },
          { name: 'db', state: 'exited', status: 'Exited (1)', health: '' },
        ],
      });

      await visit('/apps/blog');

      assert.dom('[data-test-app-title]').hasText('blog');
      assert.dom('[data-test-overview-state]').hasText('稼働中');
      assert.dom('[data-test-current-deployment]').includesText('Fix login');
      assert.dom('[data-test-latest-deployment]').doesNotExist();
      assert
        .dom('[data-test-service="web"]')
        .includesText('running')
        .includesText('Up 2 hours')
        .includesText('healthy');
      assert.dom('[data-test-service="db"]').includesText('exited');
    });

    test('stop, start and restart call their endpoints', async function (assert) {
      fakes.api
        .on('GET', '/api/apps/blog/services', { services: [] })
        .on('POST', '/api/apps/blog/stop', () => {
          app = { ...app, desired_state: 'stopped' };
          return app;
        })
        .on('POST', '/api/apps/blog/start', () => {
          app = { ...app, desired_state: 'running' };
          return app;
        })
        .on('POST', '/api/apps/blog/restart', () => app);

      await visit('/apps/blog');
      await click('[data-test-control="stop"]');

      assert.dom('[data-test-dialog]').doesNotExist('staging stops directly');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/stop').length,
        1,
      );
      assert.dom('[data-test-overview-state]').hasText('停止');
      assert.dom('[data-test-app-state]').hasText('停止');
      assert.dom('[data-test-notice]').hasText('停止しました。');

      await click('[data-test-control="start"]');
      assert.dom('[data-test-overview-state]').hasText('稼働中');

      await click('[data-test-control="restart"]');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/restart').length,
        1,
      );
    });

    test('stopping a production app asks first', async function (assert) {
      app = { ...app, environment: 'production' };
      fakes.api
        .on('GET', '/api/apps/blog/services', { services: [] })
        .on('POST', '/api/apps/blog/stop', () => app);

      await visit('/apps/blog');
      await click('[data-test-control="stop"]');

      assert.dom('[data-test-dialog]').exists();
      assert.deepEqual(fakes.api.callsTo('POST', '/api/apps/blog/stop'), []);

      await click('[data-test-dialog-confirm]');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/stop').length,
        1,
      );
      assert.dom('[data-test-dialog]').doesNotExist();
    });

    test('controls that are not available yet do not break the page', async function (assert) {
      app = makeApp();
      fakes.api.fail('POST', '/api/apps/blog/start', 409, 'not_deployed');

      await visit('/apps/blog');

      assert.dom('[data-test-overview-state]').hasText('未デプロイ');
      assert
        .dom('[data-test-unavailable="unavailable"]')
        .includesText('この機能はまだ利用できません');

      await click('[data-test-control="start"]');
      assert
        .dom('[data-test-error]')
        .includesText('まだ一度もデプロイされていません');
    });
  });

  module('backups', function () {
    test('volume snapshots restore after a confirmation, dumps download', async function (assert) {
      fakes.api
        .on('GET', '/api/apps/blog/backups', {
          snapshots: [
            {
              id: '4bba301e',
              time: '2026-11-18T18:00:12Z',
              kind: 'volume',
              target: 'pgdata',
            },
            {
              id: '77aa00ff',
              time: '2026-11-18T18:00:30Z',
              kind: 'dump',
              target: 'db',
            },
          ],
        })
        .on('POST', '/api/apps/blog/backups/restore', { restored: true });

      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-snapshot="77aa00ff"] [data-test-snapshot-download]')
        .hasAttribute('href', '/api/apps/blog/backups/77aa00ff/download');
      assert
        .dom('[data-test-snapshot="77aa00ff"] [data-test-snapshot-restore]')
        .doesNotExist();

      await click(
        '[data-test-snapshot="4bba301e"] [data-test-snapshot-restore]',
      );

      assert
        .dom('[data-test-dialog]')
        .includesText('pgdata')
        .includesText('停止')
        .includesText('退避');
      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/backups/restore'),
        [],
      );

      await click('[data-test-dialog-confirm]');

      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/backups/restore')[0]?.body,
        { snapshot_id: '4bba301e' },
      );
      assert.dom('[data-test-restored]').exists();
      assert.dom('[data-test-dialog]').doesNotExist();
    });

    test('a feature that is not configured renders the placeholder', async function (assert) {
      fakes.api.fail('GET', '/api/apps/blog/backups', 503, 'not_configured');

      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-unavailable="not_configured"]')
        .includesText('この機能は設定されていません');
      assert.dom('[data-test-error]').doesNotExist();
      assert.dom('[data-test-tab="backups"]').hasClass('active');
    });

    test('a feature the backend does not have yet renders the placeholder', async function (assert) {
      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-unavailable="unavailable"]')
        .includesText('この機能はまだ利用できません');
    });
  });

  module('system', function () {
    test('it shows resources, prunes and reports backups', async function (assert) {
      let running = false;
      fakes.api
        .on('GET', '/api/system', {
          version: 'v0.1.0',
          base_domain: 'dev.example.com',
          disk: {
            total_bytes: 42949672960,
            used_bytes: 8589934592,
            used_percent: 21.5,
          },
          memory: { total_bytes: 4294967296, available_bytes: 2147483648 },
          docker_version: '29.8.0',
        })
        .on('POST', '/api/system/prune', {
          output: 'Total reclaimed space: 1.2GB',
        })
        .on('GET', '/api/backups', () => ({
          configured: true,
          running,
          last_success_at: '2026-11-16T18:00:40Z',
          warning: true,
          runs: [
            {
              id: 12,
              trigger: 'scheduled',
              status: 'failed',
              started_at: '2026-11-17T18:00:00Z',
              finished_at: '2026-11-17T18:00:05Z',
              error: 'repository is locked',
            },
          ],
        }))
        .on('POST', '/api/backups', () => {
          running = true;
          return {
            id: 13,
            trigger: 'manual',
            status: 'running',
            started_at: '2026-11-18T03:00:00Z',
            finished_at: null,
            error: '',
          };
        });

      await visit('/system');

      assert
        .dom('[data-test-disk]')
        .includesText('21.5%')
        .includesText('8.0 GB')
        .includesText('40.0 GB');
      assert.dom('[data-test-disk-warning]').doesNotExist();
      assert
        .dom('[data-test-memory]')
        .includesText('50%')
        .includesText('4.0 GB');
      assert.dom('[data-test-docker-version]').hasText('29.8.0');

      await click('[data-test-prune-builder]');
      await click('[data-test-prune-submit]');

      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/system/prune')[0]?.body,
        {
          images: true,
          builder: false,
        },
      );
      assert
        .dom('[data-test-prune-output]')
        .hasText('Total reclaimed space: 1.2GB');

      assert
        .dom('[data-test-backup-warning]')
        .includesText('24 時間以上バックアップに成功していません');
      assert
        .dom('[data-test-backup-run-row="12"]')
        .includesText('定期')
        .includesText('失敗')
        .includesText('repository is locked');

      await click('[data-test-backup-run]');

      assert.strictEqual(fakes.api.callsTo('POST', '/api/backups').length, 1);
      assert.dom('[data-test-backup-started]').exists();
      assert.dom('[data-test-backup-running]').hasText('実行中');
      assert.dom('[data-test-backup-run]').isDisabled();

      running = false;
      await fakes.poller.tick();
      assert.dom('[data-test-backup-running]').includesText('待機中');
    });

    test('missing values and missing features degrade gracefully', async function (assert) {
      fakes.api.on('GET', '/api/system', {
        version: 'v0.1.0',
        base_domain: 'dev.example.com',
        disk: null,
        memory: null,
        docker_version: '',
      });

      await visit('/system');

      assert.dom('[data-test-disk]').hasText('取得できません');
      assert.dom('[data-test-memory]').hasText('取得できません');
      assert.dom('[data-test-docker-version]').hasText('取得できません');
      assert
        .dom('[data-test-unavailable="unavailable"]')
        .exists({ count: 1 }, 'the backup section is a placeholder');

      await click('[data-test-prune-submit]');
      assert
        .dom('[data-test-unavailable="unavailable"]')
        .exists({ count: 2 }, 'and so is prune once it turned out missing');
    });

    test('a failed prune shows what the command printed', async function (assert) {
      fakes.api
        .on('GET', '/api/system', {
          version: 'v0.1.0',
          base_domain: 'dev.example.com',
          disk: null,
          memory: null,
          docker_version: '29.8.0',
        })
        .fail('POST', '/api/system/prune', 500, 'prune_failed', {
          output: 'Cannot connect to the Docker daemon',
        });

      await visit('/system');
      await click('[data-test-prune-submit]');

      assert.dom('[data-test-error]').hasText('prune に失敗しました。');
      assert
        .dom('[data-test-prune-output]')
        .hasText('Cannot connect to the Docker daemon');
    });

    test('backups that are not configured say so', async function (assert) {
      fakes.api
        .on('GET', '/api/system', {
          version: 'v0.1.0',
          base_domain: 'dev.example.com',
          disk: {
            total_bytes: 100,
            used_bytes: 90,
            used_percent: 90,
          },
          memory: null,
          docker_version: '29.8.0',
        })
        .on('GET', '/api/backups', {
          configured: false,
          running: false,
          last_success_at: null,
          warning: false,
          runs: [],
        })
        .fail('POST', '/api/backups', 409, 'busy');

      await visit('/system');

      assert.dom('[data-test-disk-warning]').exists();
      assert.dom('[data-test-unavailable="not_configured"]').exists();
      assert.dom('[data-test-backup-run]').doesNotExist();
    });
  });
});
