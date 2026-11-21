import { module, test } from 'qunit';
import { click, currentURL, visit, waitFor } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  SHA_A,
  SHA_B,
  installFakes,
  makeApp,
  makeAppBackupStatus,
  makeBackups,
  makeDeployment,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, ServiceStatus } from 'koroku-console/utils/types';

const MB = 1024 ** 2;

function makeService(overrides: Partial<ServiceStatus> = {}): ServiceStatus {
  return {
    name: 'web',
    state: 'running',
    status: 'Up 2 hours (healthy)',
    health: 'healthy',
    memory_bytes: 256 * MB,
    memory_limit_bytes: 512 * MB,
    ...overrides,
  };
}

module('Acceptance | overview', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let services: ServiceStatus[];

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const running = makeDeployment();
    app = makeApp({ current_deployment: running, latest_deployment: running });
    services = [
      makeService(),
      makeService({ name: 'db', memory_limit_bytes: 0 }),
      makeService({ name: 'worker', status: 'Up 2 hours', health: '' }),
    ];
    fakes.api
      .on('GET', '/api/apps/blog', () => app)
      .on('GET', '/api/apps/blog/services', () => ({ services }))
      .on('GET', '/api/apps/blog/deployments', {
        deployments: [
          running,
          makeDeployment({
            id: 41,
            commit_sha: SHA_B,
            commit_message: 'Add search\n\nLonger description',
            status: 'canceled',
          }),
        ],
      })
      .on('GET', '/api/apps/blog/domains', {
        default_hostname: 'blog.dev.example.com',
        domains: [],
      })
      .on('GET', '/api/backups', makeBackups())
      .on('GET', '/api/apps/blog/backups', {
        snapshots: [
          {
            id: 'aaaa1111',
            time: '2026-11-16T18:00:12Z',
            kind: 'volume',
            target: 'uploads',
          },
          {
            id: 'bbbb2222',
            time: '2026-11-17T18:00:12Z',
            kind: 'dump',
            target: 'db',
          },
        ],
      });
  });

  test('the running release: message, commit, branch and when', async function (assert) {
    await visit('/apps/blog');

    assert
      .dom('[data-test-current-deployment]')
      .includesText('Fix login')
      .includesText(SHA_A.slice(0, 7))
      .doesNotIncludeText(SHA_A, 'the SHA is shortened');
    assert.dom('[data-test-release-ref]').hasText('main');
    assert
      .dom('[data-test-current-deployment] time')
      .hasAttribute('datetime', '2026-11-18T03:01:10Z')
      .hasAttribute('title', '2026/11/18 12:01 JST');
    assert.dom('[data-test-current-deployment]').includesText('1 分 9 秒');
    assert.dom('[data-test-release]').includesText('稼働中のリリース');
    assert
      .dom('[data-test-current-deployment] [data-status]')
      .doesNotExist('the state is in the header and not repeated here');

    await click('[data-test-current-deployment] [data-test-copy]');
    assert.deepEqual(fakes.browser.copied, [SHA_A], 'the full SHA is copied');

    assert.dom('[data-test-latest-deployment]').doesNotExist();
    assert.dom('[data-test-stale]').doesNotExist();
    assert
      .dom('[data-test-control]')
      .doesNotExist('a running app has nothing to start here');
  });

  test('services show state, health and memory, quietly when all is well', async function (assert) {
    await visit('/apps/blog');

    assert.dom('[data-test-service]').exists({ count: 3 });
    assert.dom('[data-test-service][data-problem]').doesNotExist();
    assert
      .dom('[data-test-service="web"] [data-test-service-state]')
      .hasText('稼働中')
      .hasAttribute('data-status', 'running');
    assert
      .dom('[data-test-service="web"]')
      .includesText('Up 2 hours (healthy)');
    assert
      .dom('[data-test-service="web"] [data-test-service-health]')
      .includesText('正常');
    assert
      .dom('[data-test-service="web"] [data-test-service-memory]')
      .includesText('256.0 MB / 512.0 MB')
      .doesNotIncludeText('上限の');
    assert
      .dom('[data-test-service="web"] [role="meter"]')
      .hasAria('valuenow', '50')
      .hasAria('label', 'web のメモリ使用率')
      .doesNotHaveClass('is-warn');
    assert
      .dom('[data-test-service="db"] [role="meter"]')
      .doesNotExist('nothing to measure against without a limit');
    assert
      .dom('[data-test-service="db"] [data-test-service-memory]')
      .includesText('256.0 MB / 上限なし');
    assert
      .dom('[data-test-service="worker"] [data-test-service-health]')
      .includesText('なし');
  });

  test('a service in trouble stands out, with an icon and a label', async function (assert) {
    services = [
      makeService({
        status: 'Up 2 days (unhealthy)',
        health: 'unhealthy',
        memory_bytes: 490 * MB,
      }),
      makeService({
        name: 'worker',
        state: 'exited',
        status: 'Exited (137) 4 minutes ago',
        health: '',
        memory_bytes: null,
        memory_limit_bytes: null,
      }),
      makeService({
        name: 'migrate',
        state: 'exited',
        status: 'Exited (0) 2 days ago',
        health: '',
        memory_bytes: null,
        memory_limit_bytes: null,
      }),
    ];

    await visit('/apps/blog');

    assert.dom('[data-test-service][data-problem]').exists({ count: 2 });
    assert
      .dom('[data-test-service="web"] [data-test-service-state]')
      .hasText('異常')
      .hasAttribute('data-status', 'failed');
    assert
      .dom('[data-test-service="web"] [data-test-service-state] svg')
      .hasAttribute('data-icon', 'status-failed');
    assert
      .dom('[data-test-service="web"] [data-test-service-health]')
      .includesText('異常')
      .hasClass('is-bad');
    assert
      .dom('[data-test-service="web"] [data-test-service-memory]')
      .hasClass('is-high')
      .includesText('上限の 96%');
    assert.dom('[data-test-service="web"] [role="meter"]').hasClass('is-warn');
    assert
      .dom('[data-test-service="web"] [data-test-service-memory] svg')
      .hasAttribute('data-icon', 'warning', 'not colour alone');

    assert
      .dom('[data-test-service="worker"] [data-test-service-state]')
      .hasText('異常終了')
      .hasAttribute('data-status', 'failed');
    assert
      .dom('[data-test-service="worker"] [data-test-service-memory]')
      .includesText('—');

    assert
      .dom('[data-test-service="migrate"] [data-test-service-state]')
      .hasText('終了')
      .hasClass('status-quiet', 'a finished job is not a problem');
  });

  test('the last five deployments, with the running one marked', async function (assert) {
    await visit('/apps/blog');

    assert.strictEqual(
      fakes.api.callsTo('GET', '/api/apps/blog/deployments')[0]?.query['limit'],
      '5',
    );
    assert.dom('[data-test-recent-deployment]').exists({ count: 2 });
    assert
      .dom('[data-test-recent-deployment="42"]')
      .includesText('成功')
      .includesText(SHA_A.slice(0, 7))
      .includesText('Fix login');
    assert.dom('[data-test-recent-deployment="42"] [data-test-live]').exists();
    assert
      .dom(
        '[data-test-recent-deployment="41"] [data-test-recent-deployment-link]',
      )
      .hasAttribute(
        'href',
        '/apps/blog/deploy?deployment=41',
        'a deployment opens on the deploy tab',
      );
    assert
      .dom('[data-test-recent-deployment="41"]')
      .includesText('キャンセル')
      .includesText('Add search')
      .doesNotIncludeText('Longer description');
    assert
      .dom('[data-test-recent-deployment="41"] [data-test-live]')
      .doesNotExist();
    assert
      .dom('[data-test-recent-deployment="41"] time')
      .hasAttribute('datetime', '2026-11-18T03:01:10Z');

    await click('[data-test-all-deployments]');
    assert.strictEqual(currentURL(), '/apps/blog/deploy');
  });

  test('a row of the deployment list leads to the deploy tab', async function (assert) {
    await visit('/apps/blog');
    await click('[data-test-recent-deployment="41"] a');

    assert.strictEqual(currentURL(), '/apps/blog/deploy?deployment=41');
    assert
      .dom('[data-test-deployment="41"] [data-test-show-log]')
      .hasAttribute('aria-expanded', 'true', 'that deployment is open');
  });

  test('domains: the primary one first, pending ones flagged, the assigned one last', async function (assert) {
    fakes.api.on('GET', '/api/apps/blog/domains', {
      default_hostname: 'blog.dev.example.com',
      domains: [
        {
          id: 1,
          hostname: 'new.example.org',
          status: 'pending',
          is_primary: false,
          force_active: false,
          last_checked_at: null,
          last_error: 'no such host',
          records: [],
        },
        {
          id: 2,
          hostname: 'www.example.org',
          status: 'active',
          is_primary: true,
          force_active: false,
          last_checked_at: null,
          last_error: '',
          records: [],
        },
      ],
    });

    await visit('/apps/blog');

    const rows = [
      ...document.querySelectorAll('[data-test-overview-domain]'),
    ].map((row) => row.getAttribute('data-test-overview-domain'));
    assert.deepEqual(rows, [
      'www.example.org',
      'new.example.org',
      'blog.dev.example.com',
    ]);
    assert
      .dom('[data-test-overview-domain="www.example.org"] [data-test-primary]')
      .hasText('プライマリ');
    assert
      .dom('[data-test-overview-domain="new.example.org"] [data-test-pending]')
      .hasText('確認待ち')
      .hasAttribute('data-status', 'pending');
    assert
      .dom('[data-test-overview-domain="new.example.org"]')
      .doesNotIncludeText('no such host', 'the detail is on the domains tab');
    assert
      .dom('[data-test-overview-domain="blog.dev.example.com"]')
      .includesText('自動割当');
    assert.dom('[data-test-primary]').exists({ count: 1 });
    assert.dom('[data-test-pending]').exists({ count: 1 });

    await click('[data-test-overview-domains] a');
    assert.strictEqual(currentURL(), '/apps/blog/domains');
  });

  test('backup: the newest snapshot of this app and the next run', async function (assert) {
    await visit('/apps/blog');

    assert
      .dom('[data-test-backup-state] time')
      .hasAttribute('datetime', '2026-11-17T18:00:12Z');
    assert
      .dom('[data-test-next-backup]')
      .hasAttribute('datetime', '2026-11-18T18:00:00Z')
      .hasAttribute('title', '2026/11/19 03:00 JST');
    assert.dom('[data-test-backup-warning]').doesNotExist();
  });

  test("backup: this app's failure is flagged, a missing setup is said", async function (assert) {
    fakes.api
      .on('GET', '/api/backups', makeBackups({ warning: true }))
      .on('GET', '/api/apps/blog/backups', { snapshots: [] })
      .on(
        'GET',
        '/api/apps/blog/backups/status',
        makeAppBackupStatus({}, { status: 'failed' }),
      );

    await visit('/apps/blog');

    assert.dom('[data-test-backup-state]').includesText('まだありません');
    assert
      .dom('[data-test-backup-warning]')
      .includesText('このアプリのバックアップ')
      .includesText('直近の取得が失敗')
      .hasClass('is-failed');
    assert.dom('[data-test-backup-warning] svg').exists('not colour alone');

    fakes.api
      .on('GET', '/api/backups', makeBackups({ configured: false }))
      .on(
        'GET',
        '/api/apps/blog/backups/status',
        makeAppBackupStatus({ configured: false }),
      )
      .fail('GET', '/api/apps/blog/backups', 503, 'not_configured');
    await fakes.poller.tick();

    assert
      .dom('[data-test-backup-state]')
      .hasText('バックアップは設定されていません。');
    assert.dom('[data-test-backup-warning]').doesNotExist();
  });

  test('a failed latest deployment is announced above the release that still serves', async function (assert) {
    app = {
      ...app,
      latest_deployment: makeDeployment({
        id: 43,
        status: 'failed',
        commit_sha: SHA_B,
        commit_message: 'Upgrade the base image',
        error: 'health check failed: GET / returned 500',
      }),
    };

    await visit('/apps/blog');

    assert
      .dom('[data-test-latest-deployment]')
      .hasAttribute('role', 'alert')
      .includesText('直近のデプロイに失敗しました')
      .includesText(SHA_B.slice(0, 7))
      .includesText('Upgrade the base image')
      .includesText('以前のリリースが応答し続けています');
    assert
      .dom('[data-test-latest-deployment-error]')
      .hasText('health check failed: GET / returned 500');
    assert
      .dom('[data-test-current-deployment]')
      .includesText('Fix login', 'the release that serves is still described');
    assert.dom('[data-test-release]').includesText('稼働中のリリース');

    await click('[data-test-latest-deployment] a');
    assert.strictEqual(currentURL(), '/apps/blog/deploy');
  });

  test('a deployment in progress is announced, and its end is picked up by polling', async function (assert) {
    const next = makeDeployment({
      id: 43,
      status: 'running',
      commit_sha: SHA_B,
      commit_message: 'Add search',
      finished_at: null,
    });
    app = { ...app, latest_deployment: next };

    await visit('/apps/blog');

    assert
      .dom('[data-test-latest-deployment]')
      .hasAttribute('role', 'status')
      .includesText('デプロイを実行中です')
      .includesText('Add search');
    assert
      .dom('[data-test-current-deployment]')
      .includesText('Fix login')
      .doesNotIncludeText('Add search');

    const done = { ...next, status: 'succeeded' as const };
    app = { ...app, current_deployment: done, latest_deployment: done };
    services = [makeService({ status: 'Up 3 seconds' })];
    await fakes.poller.tick();

    assert.dom('[data-test-latest-deployment]').doesNotExist();
    assert.dom('[data-test-current-deployment]').includesText('Add search');
    assert
      .dom('[data-test-service]')
      .exists({ count: 1 }, 'the services were read again');
    assert.dom('[data-test-service="web"]').includesText('Up 3 seconds');
  });

  test('a stopped app says so and offers to start it, as the one action', async function (assert) {
    app = { ...app, desired_state: 'stopped' };
    services = [];
    fakes.api.on('POST', '/api/apps/blog/start', () => {
      app = { ...app, desired_state: 'running' };
      services = [makeService()];
      return app;
    });

    await visit('/apps/blog');

    assert.dom('[data-test-app-state]').hasText('停止');
    assert.dom('[data-test-release]').includesText('停止中のリリース');
    assert.dom('[data-test-stopped-hint]').includesText('503');
    assert.dom('[data-test-no-services]').exists();
    assert
      .dom('[data-test-live]')
      .doesNotExist('nothing answers while the app is stopped');
    assert
      .dom('[data-test-control]')
      .exists({ count: 1 })
      .hasText('起動')
      .doesNotHaveClass('button-primary', 'the header holds the main button');

    await click('[data-test-control="start"]');

    assert.strictEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/start').length,
      1,
    );
    assert.dom('[data-test-app-state]').hasText('稼働中');
    assert.dom('[data-test-release]').includesText('稼働中のリリース');
    assert.dom('[data-test-live]').exists();
    assert.dom('[data-test-notice]').hasText('起動しました。');
    assert.dom('[data-test-service="web"]').exists();
    assert.dom('[data-test-control]').doesNotExist();
    assert.dom('[data-test-stopped-hint]').doesNotExist();
  });

  test('a start that fails says why and leaves the app stopped', async function (assert) {
    app = { ...app, desired_state: 'stopped' };
    fakes.api.fail('POST', '/api/apps/blog/start', 409, 'busy');

    await visit('/apps/blog');
    await click('[data-test-control="start"]');

    assert.dom('[data-test-error]').includesText('ほかの処理が実行中です');
    assert.dom('[data-test-app-state]').hasText('停止');
    assert.dom('[data-test-stopped-hint]').exists();
    assert.dom('[data-test-control="start"]').isNotDisabled();
  });

  test('an app that was never deployed offers exactly one action: deploy', async function (assert) {
    app = makeApp();

    await visit('/apps/blog');

    assert
      .dom('[data-test-overview-empty]')
      .includesText('まだデプロイされていません');
    assert
      .dom('[data-test-overview-empty] a, [data-test-overview-empty] button')
      .exists({ count: 1 })
      .hasText('デプロイする')
      .hasClass('button')
      .doesNotHaveClass('button-primary', 'the header holds the main button');
    assert.dom('[data-test-release]').doesNotExist();
    assert.dom('[data-test-control]').doesNotExist();
    assert.deepEqual(
      fakes.api.callsTo('GET', '/api/apps/blog/services'),
      [],
      'there are no containers to ask about',
    );
    assert
      .dom('[data-test-overview-domain="blog.dev.example.com"]')
      .exists('the assigned domain is there from the start');

    await click('[data-test-overview-deploy]');
    assert.strictEqual(currentURL(), '/apps/blog/deploy');
  });

  test('a first deployment that failed or is running changes the empty state', async function (assert) {
    app = makeApp({
      latest_deployment: makeDeployment({
        status: 'failed',
        error: 'compose config: service "web" has no image',
      }),
    });

    await visit('/apps/blog');

    assert
      .dom('[data-test-overview-empty]')
      .includesText('最初のデプロイに失敗しました')
      .includesText('service "web" has no image');
    assert.dom('[data-test-overview-deploy]').hasText('デプロイを確認');
    assert
      .dom('[data-test-latest-deployment]')
      .doesNotExist('the empty state already says it');

    app = makeApp({
      latest_deployment: makeDeployment({
        status: 'running',
        finished_at: null,
      }),
    });
    await fakes.poller.tick();

    assert
      .dom('[data-test-overview-empty]')
      .includesText('最初のデプロイを実行中です');
    assert.dom('[data-test-overview-deploy]').hasText('進行状況を見る');

    // It finishes: the release and its services appear without a reload.
    const done = makeDeployment();
    app = makeApp({ current_deployment: done, latest_deployment: done });
    await fakes.poller.tick();

    assert.dom('[data-test-overview-empty]').doesNotExist();
    assert.dom('[data-test-current-deployment]').includesText('Fix login');
    assert.dom('[data-test-service="web"]').exists();
  });

  test('skeleton rows stand in while the services load', async function (assert) {
    let respond: (value: unknown) => void = () => {};
    fakes.api.on(
      'GET',
      '/api/apps/blog/services',
      () => new Promise((resolve) => (respond = resolve)),
    );

    const visiting = visit('/apps/blog');
    await waitFor('[data-test-loading]');

    assert.dom('[data-test-loading]').hasAttribute('role', 'status');
    assert.dom('[data-test-loading] .skeleton').exists();
    assert
      .dom('[data-test-current-deployment]')
      .includesText('Fix login', 'what is known is shown right away');
    assert.dom('[data-test-service]').doesNotExist();

    respond({ services });
    await visiting;

    assert.dom('[data-test-loading]').doesNotExist();
    assert.dom('[data-test-service]').exists({ count: 3 });
  });

  test('each part that fails to load says so and can be retried', async function (assert) {
    fakes.api
      .fail('GET', '/api/apps/blog/services', 500, 'internal')
      .fail('GET', '/api/apps/blog/domains', 500, 'internal');

    await visit('/apps/blog');

    assert
      .dom('[data-test-error]')
      .exists({ count: 1 })
      .hasAttribute('role', 'alert')
      .includesText('サービスの状態を取得できませんでした')
      .includesText('サーバーでエラーが発生しました');
    assert
      .dom('[data-test-overview-domains] [data-test-side-error]')
      .includesText('取得できませんでした');
    assert
      .dom('[data-test-recent-deployment]')
      .exists('the parts that loaded are shown');

    fakes.api.on('GET', '/api/apps/blog/services', { services });
    await click('[data-test-error] button');

    assert.dom('[data-test-error]').doesNotExist();
    assert.dom('[data-test-service]').exists({ count: 3 });

    fakes.api.on('GET', '/api/apps/blog/domains', {
      default_hostname: 'blog.dev.example.com',
      domains: [],
    });
    await click('[data-test-overview-domains] [data-test-side-error] button');

    assert.dom('[data-test-side-error]').doesNotExist();
    assert.dom('[data-test-overview-domain="blog.dev.example.com"]').exists();
  });

  test('a failed refresh keeps what is on screen and says it is stale', async function (assert) {
    await visit('/apps/blog');

    fakes.api.fail('GET', '/api/apps/blog/services', 500, 'internal');
    await fakes.poller.tick();

    assert.dom('[data-test-service]').exists({ count: 3 });
    assert
      .dom('[data-test-stale]')
      .includesText('最新の状態を取得できていません');
    assert.dom('[data-test-error]').doesNotExist();

    fakes.api.on('GET', '/api/apps/blog/services', { services });
    await fakes.poller.tick();

    assert.dom('[data-test-stale]').doesNotExist();
  });

  test('a service turning unhealthy shows up without a reload', async function (assert) {
    await visit('/apps/blog');
    assert.dom('[data-test-service][data-problem]').doesNotExist();

    services = [makeService({ health: 'unhealthy' })];
    await fakes.poller.tick();

    assert
      .dom('[data-test-service="web"]')
      .hasAttribute('data-problem', 'true');
  });

  test('a backend without the services endpoint does not break the page', async function (assert) {
    fakes.api.fail('GET', '/api/apps/blog/services', 404, 'not_found');

    await visit('/apps/blog');

    assert
      .dom('[data-test-unavailable="unavailable"]')
      .includesText('この機能はまだ利用できません');
    assert.dom('[data-test-current-deployment]').includesText('Fix login');
  });
});
