import { module, test } from 'qunit';
import { click, currentURL, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  SHA_A,
  SHA_B,
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';

module('Acceptance | app list', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
  });

  test('it lists apps with state, URL, last deployment and environment', async function (assert) {
    const succeeded = makeDeployment({ id: 1 });
    fakes.api.on('GET', '/api/apps', {
      apps: [
        makeApp({
          slug: 'blog',
          environment: 'production',
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
          latest_deployment: makeDeployment({ id: 3, status: 'failed' }),
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

    assert.dom('[data-test-app="blog"] [data-test-state]').hasText('稼働中');
    assert
      .dom('[data-test-app="blog"] [data-test-environment]')
      .hasText('本番');
    assert
      .dom('[data-test-app="blog"] [data-test-url]')
      .hasText('https://blog.dev.example.com')
      .hasAttribute('href', 'https://blog.dev.example.com');
    assert
      .dom('[data-test-app="blog"] [data-test-last-deployment]')
      .includesText(SHA_A.slice(0, 7))
      .includesText('Fix login');

    assert
      .dom('[data-test-app="blog-staging"] [data-test-state]')
      .hasText('デプロイ中');
    assert
      .dom('[data-test-app="blog-staging"] [data-test-environment]')
      .hasText('ステージング');
    assert
      .dom('[data-test-app="blog-staging"] [data-test-last-deployment]')
      .includesText(SHA_B.slice(0, 7))
      .includesText('Add search')
      .doesNotIncludeText('Longer description');

    assert
      .dom('[data-test-app="docs"] [data-test-state]')
      .hasText('直近のデプロイに失敗');
    assert.dom('[data-test-app="paused"] [data-test-state]').hasText('停止中');
    assert
      .dom('[data-test-app="fresh"] [data-test-state]')
      .hasText('未デプロイ');
    assert
      .dom('[data-test-app="fresh"] [data-test-last-deployment]')
      .hasText('デプロイ履歴なし');
  });

  test('it refreshes while an app is deploying', async function (assert) {
    const running = makeDeployment({ status: 'running', finished_at: null });
    fakes.api.on('GET', '/api/apps', {
      apps: [makeApp({ latest_deployment: running })],
    });

    await visit('/');
    assert.dom('[data-test-state]').hasText('デプロイ中');

    const done = makeDeployment();
    fakes.api.on('GET', '/api/apps', {
      apps: [makeApp({ current_deployment: done, latest_deployment: done })],
    });
    await fakes.poller.tick();

    assert.dom('[data-test-state]').hasText('稼働中');

    const calls = fakes.api.callsTo('GET', '/api/apps').length;
    await fakes.poller.tick();
    assert.strictEqual(
      fakes.api.callsTo('GET', '/api/apps').length,
      calls,
      'nothing is polled once every app is settled',
    );
  });

  test('an empty list points to the create page', async function (assert) {
    fakes.api.on('GET', '/api/apps', { apps: [] });

    await visit('/');

    assert.dom('[data-test-empty]').includesText('アプリはまだありません');

    await click('[data-test-new-app]');
    assert.strictEqual(currentURL(), '/apps/new');
  });

  test('a failed list can be retried', async function (assert) {
    fakes.api.fail('GET', '/api/apps', 500, 'internal');

    await visit('/');

    assert
      .dom('[data-test-error]')
      .includesText('サーバーでエラーが発生しました');

    fakes.api.on('GET', '/api/apps', { apps: [makeApp()] });
    await click('[data-test-error] button');

    assert.dom('[data-test-app="blog"]').exists();
  });

  test('an unknown app shows a not-found message', async function (assert) {
    fakes.api.fail('GET', '/api/apps/ghost', 404, 'app_not_found');

    await visit('/apps/ghost');

    assert.dom('[data-test-app-error]').includesText('アプリが見つかりません');
  });
});
