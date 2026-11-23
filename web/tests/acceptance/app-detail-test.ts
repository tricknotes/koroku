import { module, test } from 'qunit';
import { click, currentURL, find, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  SHA_A,
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App } from 'koroku-console/utils/types';

module('Acceptance | app detail', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const current = makeDeployment();
    app = makeApp({
      current_deployment: current,
      latest_deployment: current,
    });
    fakes.api
      .on('GET', '/api/apps/blog', () => app)
      .on('GET', '/api/repos/octo/blog/branches', {
        branches: [{ name: 'main', sha: SHA_A }],
      })
      .on('GET', '/api/repos/octo/blog/commits', { commits: [] })
      .on('GET', '/api/apps/blog/deployments', () => ({
        deployments: app.latest_deployment ? [app.latest_deployment] : [],
      }));
  });

  test('the header shows name, environment, state and the primary URL', async function (assert) {
    app = { ...app, primary_url: 'https://www.example.org' };

    await visit('/apps/blog');

    assert.dom('h1[data-test-app-title]').hasText('blog');
    assert
      .dom('[data-test-app-environment]')
      .hasText('ステージング')
      .hasClass('badge-env-staging');
    assert
      .dom('[data-test-app-state]')
      .hasText('稼働中')
      .hasAttribute('data-status', 'running');
    assert.dom('[data-test-app-state] svg').exists('an icon, not colour alone');
    assert
      .dom('[data-test-app-url]')
      .hasText('www.example.org')
      .hasAttribute('href', 'https://www.example.org')
      .hasAttribute('target', '_blank')
      .hasAttribute('rel', 'noopener noreferrer');
    assert.dom('[data-test-header-deploy]').hasText('デプロイ');
  });

  test('a production app is marked prominently on every tab', async function (assert) {
    app = { ...app, environment: 'production' };

    await visit('/apps/blog');
    assert
      .dom('[data-test-app-environment]')
      .hasText('本番')
      .hasClass('badge-env-production');

    await click('[data-test-tab="deploy"]');
    assert.dom('[data-test-app-environment]').hasText('本番');
  });

  test('an app that was never deployed says so', async function (assert) {
    app = { ...app, current_deployment: null, latest_deployment: null };

    await visit('/apps/blog');

    assert
      .dom('[data-test-app-state]')
      .hasText('未デプロイ')
      .hasAttribute('data-status', 'stopped');
  });

  test('the tabs are links, and the URL restores the tab', async function (assert) {
    await visit('/apps/blog/deploy');

    const tabs = [
      ...document.querySelectorAll<HTMLElement>('[data-test-tab]'),
    ].map((tab) => tab.textContent.trim());
    assert.deepEqual(tabs, [
      '概要',
      'デプロイ',
      'ログ',
      'リソース',
      'タスク',
      '環境変数',
      'バックアップ',
      '設定',
    ]);
    assert
      .dom('[data-test-tab="deploy"]')
      .hasClass('active')
      .hasAttribute('aria-current', 'page')
      .hasAttribute('href', '/apps/blog/deploy');
    assert.dom('[data-test-tab="index"]').doesNotHaveClass('active');
  });

  test('the header button opens the deploy tab with the focus on the form', async function (assert) {
    await visit('/apps/blog');
    assert
      .dom('[data-test-header-deploy]')
      .hasAttribute('href', '/apps/blog/deploy')
      .hasClass('button-primary', 'the main action of the page');

    await click('[data-test-header-deploy]');

    assert.strictEqual(currentURL(), '/apps/blog/deploy');
    assert.dom('[data-test-deploy-form] [data-deploy-focus]').isFocused();
    assert
      .dom('[data-test-header-deploy]')
      .doesNotHaveClass(
        'button-primary',
        "on the deploy tab the form's button is the main action",
      );

    // Once there, it still leads back to the form.
    (find('[data-deploy-focus]') as HTMLElement | null)?.blur();
    await click('[data-test-header-deploy]');
    assert.dom('[data-test-deploy-form] [data-deploy-focus]').isFocused();
  });

  test('the state follows the app without a reload', async function (assert) {
    await visit('/apps/blog/env');
    assert.dom('[data-test-app-state]').hasText('稼働中');

    app = {
      ...app,
      latest_deployment: makeDeployment({
        id: 43,
        status: 'running',
        finished_at: null,
      }),
    };
    await fakes.poller.tick();
    await settled();
    assert.dom('[data-test-app-state]').hasText('デプロイ中');

    app = {
      ...app,
      latest_deployment: makeDeployment({ id: 43, status: 'failed' }),
    };
    await fakes.poller.tick();
    await settled();
    assert.dom('[data-test-app-state]').hasText('失敗');

    // A failed refresh keeps what was on screen.
    fakes.api.fail('GET', '/api/apps/blog', 500, 'internal');
    await fakes.poller.tick();
    await settled();
    assert.dom('[data-test-app-state]').hasText('失敗');
    assert.dom('[data-test-app-error]').doesNotExist();
  });

  test('only the logs tab gets the full-size layout', async function (assert) {
    await visit('/apps/blog');
    assert.dom('[data-test-app-detail]').doesNotHaveClass('app-detail-full');
    const columnWidth =
      find('[data-test-app-detail]')?.getBoundingClientRect().width ?? 0;

    await click('[data-test-tab="logs"]');

    assert.dom('[data-test-app-detail]').hasClass('app-detail-full');
    assert
      .dom('[data-test-app-detail] > .app-detail-body')
      .exists('the element the tab fills');
    const header = find('.app-header')?.getBoundingClientRect().width ?? 0;
    assert.true(
      header <= columnWidth + 1,
      'the header keeps to the centred column',
    );

    await click('[data-test-tab="env"]');
    assert.dom('[data-test-app-detail]').doesNotHaveClass('app-detail-full');
  });

  test('an unknown app says so and leads back to the list', async function (assert) {
    fakes.api
      .fail('GET', '/api/apps/ghost', 404, 'app_not_found')
      .on('GET', '/api/apps', { apps: [] });

    await visit('/apps/ghost/deploy');

    assert
      .dom('[data-test-app-error]')
      .includesText('ghost')
      .includesText('アプリが見つかりません');
    assert.dom('[data-test-tab]').doesNotExist();
    assert
      .dom('[data-test-app-error] a, [data-test-app-error] button')
      .exists({ count: 1 }, 'exactly one way on');

    await click('[data-test-back]');
    assert.strictEqual(currentURL(), '/');
  });
});
