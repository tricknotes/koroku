import { module, test } from 'qunit';
import { click, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  SHA_A,
  SHA_B,
  installFakes,
  makeApp,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, Deployment, Promotion } from 'koroku-console/utils/types';

const STAGING_RELEASE = makeDeployment({
  id: 50,
  app_slug: 'blog-staging',
  commit_sha: SHA_B,
  commit_message: 'Add search',
});

function preview(overrides: Partial<Promotion> = {}): Promotion {
  return {
    status: 'ready',
    reason: '',
    source_app: 'blog-staging',
    source_deployment: STAGING_RELEASE,
    current_deployment: makeDeployment({ id: 42 }),
    commits: {
      status: 'ok',
      list: [
        {
          sha: SHA_B,
          message: 'Add search',
          author: 'octo',
          date: '2026-11-18T03:00:00Z',
        },
      ],
    },
    env: { only_in_source: [], only_in_target: ['SENTRY_DSN'] },
    images: {
      status: 'checked',
      services: [
        { service: 'web', reuse: true, reason: '', differing_args: [] },
      ],
    },
    ...overrides,
  };
}

module('Acceptance | promotion', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let deployments: Deployment[];
  let promotion: Promotion;

  function serveApp(overrides: Partial<App> = {}): App {
    const app = makeApp({
      environment: 'production',
      promote_from: 'blog-staging',
      ...overrides,
    });
    fakes.api
      .on('GET', `/api/apps/${app.slug}`, () => ({
        ...app,
        latest_deployment: deployments[0] ?? null,
      }))
      .on('GET', `/api/apps/${app.slug}/deployments`, () => ({ deployments }))
      .on('GET', `/api/apps/${app.slug}/promotion`, () => promotion)
      .on('POST', `/api/apps/${app.slug}/deployments`, () => {
        const created = makeDeployment({
          id: 43,
          status: 'queued',
          commit_sha: SHA_B,
          commit_message: 'Add search',
          ref: '',
          started_at: null,
          finished_at: null,
          promoted_from: { deployment_id: 50, app_slug: 'blog-staging' },
        });
        deployments = [created, ...deployments];
        return created;
      });
    return app;
  }

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    deployments = [makeDeployment({ id: 42 })];
    promotion = preview();
    fakes.api
      .on('GET', '/api/repos/octo/blog/branches', {
        branches: [{ name: 'main', sha: SHA_A }],
      })
      .on('GET', '/api/repos/octo/blog/commits', { commits: [] });
  });

  test('a production app promotes the staging release after confirming', async function (assert) {
    serveApp();
    await visit('/apps/blog/deploy');

    assert
      .dom('[data-test-promotion-source]')
      .includesText('blog-staging')
      .includesText(SHA_B.slice(0, 7))
      .includesText('Add search');
    assert.dom('[data-test-promotion-current]').includesText(SHA_A.slice(0, 7));
    assert.dom('[data-test-promotion-commits]').includesText('1 件');
    assert.dom('[data-test-promotion-reuse]').includesText('イメージを再利用');
    assert.dom('[data-test-promotion-env-target]').hasText('SENTRY_DSN');

    await click('[data-test-promote]');
    assert.dom('[data-test-dialog]').includesText('本番アプリ');
    assert
      .dom('[data-test-dialog-target]')
      .includesText(SHA_B.slice(0, 7))
      .includesText('Add search');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/deployments'),
      [],
    );

    await click('[data-test-dialog-confirm]');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/deployments')[0]?.body,
      { promote_from_deployment: 50 },
    );
    assert
      .dom('[data-test-deployment="43"] [data-test-promoted-from]')
      .includesText('blog-staging から昇格');
  });

  test('a rebuild names the services and the differing build args', async function (assert) {
    promotion = preview({
      commits: { status: 'unknown', list: [] },
      env: { only_in_source: ['DEBUG_TOOLBAR'], only_in_target: [] },
      images: {
        status: 'checked',
        services: [
          {
            service: 'web',
            reuse: false,
            reason: 'args_differ',
            differing_args: ['ASSET_HOST', 'CDN_URL'],
          },
          { service: 'worker', reuse: true, reason: '', differing_args: [] },
        ],
      },
    });
    serveApp();
    await visit('/apps/blog/deploy');

    assert.dom('[data-test-promotion-rebuild]').exists();
    assert
      .dom('[data-test-promotion-rebuilt="web"]')
      .includesText('ASSET_HOST, CDN_URL');
    assert.dom('[data-test-promotion-reused="worker"]').exists();
    assert
      .dom('[data-test-promotion-commits]')
      .includesText('数えられませんでした');

    await click('[data-test-promote]');
    assert.dom('[data-test-dialog]').includesText('ビルドし直します（web）');
    assert.dom('[data-test-dialog-env]').includesText('DEBUG_TOOLBAR');
  });

  test('an unpaired production app points to the settings', async function (assert) {
    promotion = preview({
      status: 'not_paired',
      source_app: '',
      source_deployment: null,
    });
    serveApp({ promote_from: '' });
    await visit('/apps/blog/deploy');

    assert.dom('[data-test-promotion-unpaired]').exists();
    assert.dom('[data-test-promote]').doesNotExist();
    await click('[data-test-promotion-settings]');
    assert.dom('[data-test-promotion-group]').exists();
  });

  test('an unavailable promotion says why', async function (assert) {
    promotion = preview({
      status: 'unavailable',
      reason: 'no_source_release',
      source_deployment: null,
    });
    serveApp();
    await visit('/apps/blog/deploy');
    assert
      .dom('[data-test-promotion-unavailable]')
      .includesText('成功したリリースがありません');
    assert.dom('[data-test-promote]').doesNotExist();
  });

  test('a stale promotion reloads the preview', async function (assert) {
    serveApp();
    fakes.api.fail(
      'POST',
      '/api/apps/blog/deployments',
      409,
      'promotion_stale',
    );
    await visit('/apps/blog/deploy');
    const before = fakes.api.callsTo('GET', '/api/apps/blog/promotion').length;

    await click('[data-test-promote]');
    await click('[data-test-dialog-confirm]');

    assert
      .dom('[data-test-promotion] [data-test-error]')
      .includesText('更新されました');
    assert.true(
      fakes.api.callsTo('GET', '/api/apps/blog/promotion').length > before,
      'the preview is read again',
    );
  });

  test('a staging app has no promotion panel', async function (assert) {
    serveApp({
      slug: 'blog-staging',
      environment: 'staging',
      promote_from: '',
    });
    await visit('/apps/blog-staging/deploy');
    assert.dom('[data-test-promotion]').doesNotExist();
  });

  test('the deploy settings pair a production app with a staging app of its repository', async function (assert) {
    const app = serveApp({ promote_from: '' });
    fakes.api
      .on('GET', '/api/apps', {
        apps: [
          app,
          makeApp({ slug: 'blog-staging', environment: 'staging' }),
          makeApp({ slug: 'blog-review', environment: 'staging' }),
          makeApp({ slug: 'blog-two', environment: 'production' }),
          makeApp({
            slug: 'shop-staging',
            environment: 'staging',
            repo_full_name: 'octo/shop',
          }),
        ],
      })
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => ({
        ...app,
        ...(call.body as object),
      }));
    await visit('/apps/blog/deploy');

    const options = [
      ...document.querySelectorAll('[data-test-field="promote_from"] option'),
    ].map((o) => (o as HTMLOptionElement).value);
    assert.deepEqual(options, ['', 'blog-staging', 'blog-review']);

    await fillIn('[data-test-field="promote_from"]', 'blog-staging');
    await click('[data-test-deploy-settings-save]');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      promote_from: 'blog-staging',
    });
  });

  test('the promotion source is offered to production apps only', async function (assert) {
    serveApp({ slug: 'blog', environment: 'staging', promote_from: '' });
    fakes.api.on('GET', '/api/apps', { apps: [] });
    await visit('/apps/blog/deploy');
    assert.dom('[data-test-promotion-group]').doesNotExist();

    serveApp({ promote_from: '' });
    await visit('/');
    await visit('/apps/blog/deploy?section=promotion');
    assert.dom('[data-test-promotion-group]').exists();
    assert.dom('#promotion').isFocused('a link to the section lands there');
    assert.dom('[data-test-no-promotion-sources]').exists();
  });
});
