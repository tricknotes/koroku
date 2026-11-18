import { module, test } from 'qunit';
import { click, fillIn, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';
import type { App } from 'paas-console/utils/types';

module('Acceptance | overview, logs, backups, system', function (hooks) {
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
      assert.dom('[data-test-overview-state]').hasText('停止中');
      assert.dom('[data-test-app-state]').hasText('停止中');
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

  module('logs', function (hooks) {
    hooks.beforeEach(function () {
      fakes.api
        .on('GET', '/api/apps/blog/services', {
          services: [
            { name: 'web', state: 'running', status: '', health: '' },
            { name: 'db', state: 'running', status: '', health: '' },
          ],
        })
        .on('GET', '/api/apps/blog/logs/archives', {
          archives: [
            {
              key: 'logs/blog/web/2026/11/18/04.log.gz',
              service: 'web',
              hour: '2026-11-18T04:00:00+09:00',
              size: 12345,
            },
          ],
        });
    });

    test('the live tail shows lines and follows the service filter', async function (assert) {
      await visit('/apps/blog/logs');

      const first = fakes.eventSource.last;
      assert.strictEqual(first.url, '/api/apps/blog/logs?tail=200');

      first.emit(
        JSON.stringify({
          service: 'web',
          time: '2026-11-18T03:00:00.123Z',
          line: 'Listening on 0.0.0.0:3000',
        }),
      );
      first.emit('not json');
      await settled();

      assert
        .dom('[data-test-live-text]')
        .includesText('web | Listening on 0.0.0.0:3000')
        .includesText('not json');
      assert.dom('[data-test-live-state]').hasText('受信中');

      await fillIn('[data-test-live-service]', 'db');

      assert.true(first.closed, 'the previous stream is closed');
      assert.strictEqual(
        fakes.eventSource.last.url,
        '/api/apps/blog/logs?service=db&tail=200',
      );
      assert.dom('[data-test-live-text]').doesNotIncludeText('Listening');

      await click('[data-test-live-toggle]');
      assert.true(fakes.eventSource.last.closed);
      assert.dom('[data-test-live-state]').hasText('停止中');

      await click('[data-test-live-toggle]');
      assert.false(fakes.eventSource.last.closed);
      assert.strictEqual(fakes.eventSource.streams.length, 3);
    });

    test('the stream is closed when leaving the tab', async function (assert) {
      await visit('/apps/blog/logs');
      const stream = fakes.eventSource.last;

      await click('[data-test-tab="env"]');

      assert.true(stream.closed);
    });

    test('a refused live stream explains itself', async function (assert) {
      await visit('/apps/blog/logs');
      fakes.eventSource.last.error(true);
      await settled();

      assert
        .dom('[data-test-live-state]')
        .includesText('ライブログに接続できませんでした');
      assert.true(fakes.eventSource.last.closed);
    });

    test('a reconnect does not duplicate the tail', async function (assert) {
      await visit('/apps/blog/logs');
      const stream = fakes.eventSource.last;
      const line = JSON.stringify({ service: 'web', time: '', line: 'boot' });

      stream.open();
      stream.emit(line);
      await settled();
      assert.dom('[data-test-live-text]').hasText('web | boot');

      stream.error(false);
      await settled();
      assert.dom('[data-test-live-state]').includesText('再接続中');
      assert.false(stream.closed, 'the browser reconnects by itself');

      // The server replays the last lines on the new connection.
      stream.open();
      stream.emit(line);
      await settled();
      assert.dom('[data-test-live-text]').hasText('web | boot');
    });

    test('the history search passes its filters', async function (assert) {
      fakes.api.on('GET', '/api/apps/blog/logs/history', {
        lines: [
          { service: 'web', time: '2026-11-17T01:00:00Z', line: 'GET /up 200' },
          {
            service: 'web',
            time: '2026-11-17T01:00:05Z',
            line: 'GET /posts 500',
          },
        ],
        truncated: true,
      });

      await visit('/apps/blog/logs');
      await fillIn('[data-test-search-service]', 'web');
      await fillIn('[data-test-search-from]', '2026-11-17T00:00');
      await fillIn('[data-test-search-keyword]', 'GET');
      await click('[data-test-search-submit]');

      const query = fakes.api.callsTo('GET', '/api/apps/blog/logs/history')[0]
        ?.query;
      assert.deepEqual(query, {
        service: 'web',
        from: new Date('2026-11-17T00:00').toISOString(),
        q: 'GET',
        limit: '1000',
      });
      assert
        .dom('[data-test-search-result]')
        .includesText('web | GET /up 200')
        .includesText('GET /posts 500');
      assert.dom('[data-test-search-truncated]').exists();
    });

    test('archived logs are downloaded through a presigned URL', async function (assert) {
      fakes.api.on('GET', '/api/apps/blog/logs/archives/url', {
        url: 'https://storage.example.org/logs/blog/web/04.log.gz?signature=abc',
      });

      await visit('/apps/blog/logs');

      assert
        .dom('[data-test-archive="logs/blog/web/2026/11/18/04.log.gz"]')
        .includesText('web')
        .includesText('12.1 KB');

      await click('[data-test-archive-download]');

      assert.deepEqual(
        fakes.api.callsTo('GET', '/api/apps/blog/logs/archives/url')[0]?.query,
        { key: 'logs/blog/web/2026/11/18/04.log.gz' },
      );
      assert.deepEqual(fakes.browser.assigned, [
        'https://storage.example.org/logs/blog/web/04.log.gz?signature=abc',
      ]);

      await fillIn('[data-test-archive-service]', 'db');
      assert.strictEqual(
        fakes.api.callsTo('GET', '/api/apps/blog/logs/archives').pop()?.query[
          'service'
        ],
        'db',
      );
    });

    test('archives without object storage render the placeholder', async function (assert) {
      fakes.api.fail(
        'GET',
        '/api/apps/blog/logs/archives',
        503,
        'not_configured',
      );

      await visit('/apps/blog/logs');

      assert
        .dom('[data-test-archives] [data-test-unavailable="not_configured"]')
        .includesText('この機能は設定されていません');
      assert.dom('[data-test-live]').exists('the rest of the page still works');
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
