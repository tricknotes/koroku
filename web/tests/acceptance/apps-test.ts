import { module, test } from 'qunit';
import { click, currentURL, visit, waitFor } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  SHA_A,
  SHA_B,
  installFakes,
  makeApp,
  makeBackups,
  makeDeployment,
  makeSystem,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';

module('Acceptance | app list', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
  });

  test('it lists apps with state, environment, URL, running commit and latest deployment', async function (assert) {
    const succeeded = makeDeployment({ id: 1 });
    fakes.api.on('GET', '/api/apps', {
      apps: [
        makeApp({
          slug: 'blog',
          environment: 'production',
          primary_url: 'https://www.example.org',
          current_deployment: succeeded,
          latest_deployment: succeeded,
        }),
        makeApp({
          slug: 'blog-staging',
          current_deployment: succeeded,
          latest_deployment: makeDeployment({
            id: 2,
            status: 'running',
            commit_sha: SHA_B,
            commit_message: 'Add search\n\nLonger description',
            finished_at: null,
          }),
        }),
        makeApp({
          slug: 'docs',
          current_deployment: succeeded,
          latest_deployment: makeDeployment({
            id: 3,
            status: 'failed',
            commit_sha: SHA_B,
            commit_message: 'Upgrade the base image',
          }),
        }),
        makeApp({
          slug: 'paused',
          desired_state: 'stopped',
          current_deployment: succeeded,
          latest_deployment: succeeded,
        }),
        makeApp({ slug: 'fresh' }),
      ],
    });

    await visit('/');

    assert.dom('[data-test-app]').exists({ count: 5 });

    // A healthy production app: quiet, except for the production badge.
    assert
      .dom('[data-test-app="blog"] [data-test-state]')
      .hasText('稼働中')
      .hasAttribute('data-status', 'running');
    assert
      .dom('[data-test-app="blog"] [data-test-environment]')
      .hasText('本番')
      .hasClass('badge-env-production');
    assert
      .dom('[data-test-app="blog"] [data-test-url]')
      .hasText('www.example.org', 'the primary domain, without the scheme');
    assert
      .dom('[data-test-app="blog"] [data-test-open]')
      .hasAttribute('href', 'https://www.example.org')
      .hasAttribute('target', '_blank')
      .hasAttribute('rel', 'noopener noreferrer');
    assert
      .dom('[data-test-app="blog"] [data-test-current-deployment]')
      .includesText(SHA_A.slice(0, 7))
      .includesText('Fix login');
    assert
      .dom('[data-test-app="blog"] [data-test-deployment-status]')
      .hasText('成功')
      .hasClass('status-quiet');
    assert
      .dom('[data-test-app="blog"] [data-test-last-deployment]')
      .doesNotIncludeText(
        SHA_A.slice(0, 7),
        'the running commit is not repeated',
      );

    // Deploying: the running commit stays, the new one is shown beside it.
    assert
      .dom('[data-test-app="blog-staging"] [data-test-state]')
      .hasText('デプロイ中')
      .hasAttribute('data-status', 'deploying');
    assert
      .dom('[data-test-app="blog-staging"] [data-test-environment]')
      .hasText('ステージング')
      .hasClass('badge-env-staging');
    assert
      .dom('[data-test-app="blog-staging"] [data-test-url]')
      .hasText('blog-staging.dev.example.com');
    assert
      .dom('[data-test-app="blog-staging"] [data-test-current-deployment]')
      .includesText(SHA_A.slice(0, 7));
    assert
      .dom('[data-test-app="blog-staging"] [data-test-last-deployment]')
      .includesText('実行中')
      .includesText(SHA_B.slice(0, 7))
      .includesText('Add search')
      .doesNotIncludeText('Longer description');

    assert
      .dom('[data-test-app="docs"] [data-test-state]')
      .hasText('失敗')
      .hasAttribute('data-status', 'failed');
    assert
      .dom('[data-test-app="docs"] [data-test-last-deployment]')
      .includesText('失敗')
      .includesText(SHA_B.slice(0, 7))
      .includesText('Upgrade the base image');
    assert
      .dom('[data-test-app="docs"] [data-test-deployment-status]')
      .doesNotHaveClass('status-quiet');

    assert
      .dom('[data-test-app="paused"] [data-test-state]')
      .hasText('停止')
      .hasAttribute('data-status', 'stopped');

    assert
      .dom('[data-test-app="fresh"] [data-test-state]')
      .hasText('未デプロイ')
      .hasAttribute('data-status', 'stopped');
    assert
      .dom('[data-test-app="fresh"] [data-test-current-deployment]')
      .includesText('まだありません');
    assert
      .dom('[data-test-app="fresh"] [data-test-last-deployment]')
      .includesText('デプロイ履歴なし');
  });

  test('every state has an icon next to its label', async function (assert) {
    const ok = makeDeployment({ id: 1 });
    fakes.api.on('GET', '/api/apps', {
      apps: [
        makeApp({ slug: 'a', current_deployment: ok, latest_deployment: ok }),
        makeApp({
          slug: 'b',
          latest_deployment: makeDeployment({ id: 2, status: 'queued' }),
        }),
      ],
    });

    await visit('/');

    assert
      .dom('[data-test-app="a"] [data-test-state] svg')
      .hasAttribute('data-icon', 'status-running')
      .hasAttribute('aria-hidden', 'true');
    assert
      .dom('[data-test-app="b"] [data-test-state] svg')
      .hasAttribute('data-icon', 'status-deploying')
      .hasClass('icon-spin', 'the deploying icon is the animated one');
  });

  test('the row leads to the app, and "open" is the only other action', async function (assert) {
    const ok = makeDeployment({ id: 1 });
    fakes.api.on('GET', '/api/apps', {
      apps: [makeApp({ current_deployment: ok, latest_deployment: ok })],
    });
    fakes.api.on('GET', '/api/apps/blog', makeApp());

    await visit('/');

    assert
      .dom('[data-test-app="blog"] a, [data-test-app="blog"] button')
      .exists({ count: 2 }, 'the link to the app and the external link');
    assert.dom('[data-test-app-link]').hasClass('row-link');

    await click('[data-test-app-link]');

    assert.strictEqual(currentURL(), '/apps/blog');
  });

  test('the host status line shows disk, memory, last backup and log archive', async function (assert) {
    fakes.api.on('GET', '/api/apps', { apps: [makeApp()] });
    fakes.api.on('GET', '/api/system', makeSystem());
    fakes.api.on('GET', '/api/backups', makeBackups());

    await visit('/');

    assert
      .dom('[data-test-host="disk"]')
      .includesText('ディスク')
      .includesText('21.5%')
      .hasAttribute('data-level', 'ok');
    assert
      .dom('[data-test-host="memory"]')
      .includesText('メモリ')
      .includesText('2.0 GB / 4.0 GB');
    assert
      .dom('[data-test-host="backup"]')
      .includesText('最終バックアップ')
      .hasAttribute('data-level', 'ok');
    assert
      .dom('[data-test-host="backup"] time')
      .hasAttribute('datetime', '2026-11-17T18:00:40Z')
      .hasAttribute('title', '2026/11/18 03:00 JST');
    assert
      .dom('[data-test-host="log-archive"] time')
      .hasAttribute('datetime', '2026-11-18T03:10:05Z');

    assert
      .dom('[data-test-attention]')
      .doesNotExist('nothing is flagged while everything is fine');
  });

  test('the host status line degrades when its data is missing', async function (assert) {
    fakes.api.on('GET', '/api/apps', { apps: [makeApp()] });
    fakes.api.on(
      'GET',
      '/api/system',
      makeSystem({
        disk: null,
        log_archive: {
          configured: false,
          last_success_at: null,
          last_error: '',
        },
      }),
    );
    fakes.api.fail('GET', '/api/backups', 500, 'internal');

    await visit('/');

    assert.dom('[data-test-host="disk"] dd').hasText('—');
    assert.dom('[data-test-host="log-archive"] dd').hasText('未設定');
    assert.dom('[data-test-host="backup"] dd').hasText('—');
    assert.dom('[data-test-app="blog"]').exists('the list is still shown');
    assert.dom('[data-test-attention]').doesNotExist();
  });

  test('a banner lists what needs attention, with links', async function (assert) {
    const ok = makeDeployment({ id: 1 });
    fakes.api.on('GET', '/api/apps', {
      apps: [
        makeApp({
          slug: 'shop',
          environment: 'production',
          pending_domains: 2,
          current_deployment: ok,
          latest_deployment: ok,
        }),
        makeApp({
          slug: 'shop-staging',
          current_deployment: ok,
          latest_deployment: makeDeployment({ id: 2, status: 'failed' }),
        }),
      ],
    });
    fakes.api.on(
      'GET',
      '/api/system',
      makeSystem({
        disk: {
          total_bytes: 40 * 1024 ** 3,
          used_bytes: 35.4 * 1024 ** 3,
          used_percent: 88.4,
        },
      }),
    );
    fakes.api.on('GET', '/api/backups', makeBackups({ warning: true }));
    fakes.api.on('GET', '/api/apps/shop', makeApp({ slug: 'shop' }));

    await visit('/');

    assert.dom('[data-test-attention]').includesText('4 件');
    assert
      .dom('[data-test-attention-item="disk"]')
      .includesText('88.4%')
      .includesText('上限 85%');
    assert
      .dom('[data-test-attention-item="backup"]')
      .includesText('24 時間以上');
    assert
      .dom('[data-test-attention-item="deployment:shop-staging"]')
      .includesText('shop-staging')
      .includesText('失敗');
    assert
      .dom('[data-test-attention-item="domains:shop"]')
      .includesText('shop')
      .includesText('2 件');

    assert.dom('[data-test-host="disk"]').hasAttribute('data-level', 'danger');
    assert.dom('[data-test-host="disk"] svg').exists('not colour alone');
    assert.dom('[data-test-host="backup"]').hasAttribute('data-level', 'warn');

    await click('[data-test-attention-item="domains:shop"] a');

    assert.strictEqual(currentURL(), '/apps/shop/domains');
  });

  test('states refresh by polling, without a reload', async function (assert) {
    const running = makeDeployment({ status: 'running', finished_at: null });
    fakes.api.on('GET', '/api/apps', {
      apps: [makeApp({ latest_deployment: running })],
    });
    fakes.api.on('GET', '/api/system', makeSystem());
    fakes.api.on('GET', '/api/backups', makeBackups());

    await visit('/');
    assert.dom('[data-test-state]').hasText('デプロイ中');

    const done = makeDeployment();
    fakes.api.on('GET', '/api/apps', {
      apps: [makeApp({ current_deployment: done, latest_deployment: done })],
    });
    await fakes.poller.tick();

    assert.dom('[data-test-state]').hasText('稼働中');

    // A settled app can still change from elsewhere (another tab, a domain
    // check on the server), so polling goes on.
    fakes.api.on('GET', '/api/apps', {
      apps: [
        makeApp({
          pending_domains: 1,
          current_deployment: done,
          latest_deployment: done,
        }),
      ],
    });
    fakes.api.on('GET', '/api/backups', makeBackups({ warning: true }));
    await fakes.poller.tick();

    assert.dom('[data-test-attention-item="domains:blog"]').exists();
    assert.dom('[data-test-attention-item="backup"]').exists();
  });

  test('a failed refresh keeps the list and says it is stale', async function (assert) {
    fakes.api.on('GET', '/api/apps', { apps: [makeApp()] });

    await visit('/');
    assert.dom('[data-test-stale]').doesNotExist();

    fakes.api.fail('GET', '/api/apps', 500, 'internal');
    await fakes.poller.tick();

    assert.dom('[data-test-app="blog"]').exists();
    assert
      .dom('[data-test-stale]')
      .includesText('最新の状態を取得できていません');
    assert.dom('[data-test-error]').doesNotExist();

    fakes.api.on('GET', '/api/apps', { apps: [makeApp()] });
    await fakes.poller.tick();

    assert.dom('[data-test-stale]').doesNotExist();
  });

  test('skeleton rows stand in while the list loads', async function (assert) {
    let respond: (value: unknown) => void = () => {};
    fakes.api.on(
      'GET',
      '/api/apps',
      () => new Promise((resolve) => (respond = resolve)),
    );

    const visiting = visit('/');
    await waitFor('[data-test-loading]');

    assert.dom('[data-test-loading]').hasAttribute('role', 'status');
    assert.dom('[data-test-loading] .skeleton').exists();
    assert.dom('[data-test-app]').doesNotExist();
    assert.dom('[data-test-empty]').doesNotExist();

    respond({ apps: [makeApp()] });
    await visiting;

    assert.dom('[data-test-loading]').doesNotExist();
    assert.dom('[data-test-app="blog"]').exists();
  });

  test('an empty list offers exactly one action: create an app', async function (assert) {
    fakes.api.on('GET', '/api/apps', { apps: [] });

    await visit('/');

    assert.dom('[data-test-empty]').includesText('アプリはまだありません');
    assert
      .dom('[data-test-new-app]')
      .exists({ count: 1 }, 'the header button gives way to the empty state')
      .hasText('アプリを作成');
    assert
      .dom('[data-test-empty] a, [data-test-empty] button')
      .exists({ count: 1 });

    await click('[data-test-new-app]');
    assert.strictEqual(currentURL(), '/apps/new');
  });

  test('a failed list can be retried', async function (assert) {
    fakes.api.fail('GET', '/api/apps', 500, 'internal');

    await visit('/');

    assert
      .dom('[data-test-error]')
      .hasAttribute('role', 'alert')
      .includesText('サーバーでエラーが発生しました');

    fakes.api.on('GET', '/api/apps', { apps: [makeApp()] });
    await click('[data-test-error] button');

    assert.dom('[data-test-app="blog"]').exists();
  });

  test('a server that cannot be reached is told apart from one that failed', async function (assert) {
    fakes.api.fail('GET', '/api/apps', 0, 'network_error');
    fakes.api.fail('GET', '/api/system', 0, 'network_error');
    fakes.api.fail('GET', '/api/backups', 0, 'network_error');

    await visit('/');

    assert
      .dom('[data-test-error]')
      .exists({ count: 1 }, 'one message, not one per request')
      .includesText('アプリの一覧を取得できませんでした')
      .includesText('サーバーに接続できませんでした。');
    assert.dom('[data-test-host="disk"] dd').hasText('—');
    assert.dom('[data-test-empty]').doesNotExist('unknown is not "no apps"');
    assert.dom('[data-test-attention]').doesNotExist();

    fakes.api.on('GET', '/api/apps', { apps: [makeApp()] });
    await click('[data-test-error] button');

    assert.dom('[data-test-app="blog"]').exists();
    assert.dom('[data-test-error]').doesNotExist();
  });
});
