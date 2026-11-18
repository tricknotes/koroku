import { module, test } from 'qunit';
import { click, fillIn, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  SHA_A,
  SHA_B,
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';
import type { App, Deployment } from 'paas-console/utils/types';

module('Acceptance | deploy', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let deployments: Deployment[];

  /** Serves one app whose history is the mutable `deployments` array. */
  function serveApp(overrides: Partial<App> = {}): void {
    const app = makeApp(overrides);
    fakes.api
      .on('GET', `/api/apps/${app.slug}`, () => ({
        ...app,
        latest_deployment: deployments[0] ?? null,
      }))
      .on('GET', `/api/apps/${app.slug}/deployments`, () => ({ deployments }))
      .on('POST', `/api/apps/${app.slug}/deployments`, () => {
        const created = makeDeployment({
          id: 43,
          app_slug: app.slug,
          status: 'queued',
          started_at: null,
          finished_at: null,
        });
        deployments = [created, ...deployments];
        return created;
      });
  }

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    deployments = [makeDeployment({ id: 42 })];
    fakes.api
      .on('GET', '/api/repos/octo/blog/branches', {
        branches: [
          { name: 'main', sha: SHA_A },
          { name: 'feature/search', sha: SHA_B },
        ],
      })
      .on('GET', '/api/repos/octo/blog/commits', {
        commits: [
          {
            sha: SHA_A,
            message: 'Fix login',
            author: 'octo',
            date: '2026-11-18T03:00:00Z',
          },
          {
            sha: SHA_B,
            message: 'Add search',
            author: 'octo',
            date: '2026-11-17T03:00:00Z',
          },
        ],
      });
  });

  test('a staging app deploys the branch head without a dialog', async function (assert) {
    serveApp({ slug: 'blog-staging' });

    await visit('/apps/blog-staging/deploy');
    assert.dom('[data-test-deploy-branch]').hasValue('main');

    await click('[data-test-deploy-submit]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog-staging/deployments')[0]?.body,
      { ref: 'main' },
    );
    assert.dom('[data-test-deployment="43"]').exists();
  });

  test('a production app deploys only after confirming the dialog', async function (assert) {
    serveApp({ environment: 'production' });

    await visit('/apps/blog/deploy');
    await fillIn('[data-test-deploy-branch]', 'feature/search');
    assert.strictEqual(
      fakes.api.callsTo('GET', '/api/repos/octo/blog/commits').pop()?.query[
        'ref'
      ],
      'feature/search',
      'commits are listed for the chosen branch',
    );
    await fillIn('[data-test-deploy-commit]', SHA_B);
    await click('[data-test-deploy-submit]');

    assert.dom('[data-test-dialog]').exists();
    assert.dom('[data-test-dialog]').includesText('本番アプリ');
    assert
      .dom('[data-test-dialog-target]')
      .includesText('feature/search')
      .includesText(SHA_B.slice(0, 7));
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/deployments'),
      [],
      'nothing is deployed while the dialog is open',
    );

    await click('[data-test-dialog-cancel]');
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/deployments'),
      [],
    );

    await click('[data-test-deploy-submit]');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/deployments')[0]?.body,
      { ref: 'feature/search', sha: SHA_B },
    );
  });

  test('live log lines appear and the stream is closed on end', async function (assert) {
    serveApp();

    await visit('/apps/blog/deploy');
    assert.strictEqual(
      fakes.eventSource.streams.length,
      0,
      'no stream while nothing is selected or running',
    );

    await click('[data-test-deploy-submit]');

    const stream = fakes.eventSource.last;
    assert.strictEqual(stream.url, '/api/deployments/43/logs');
    assert.dom('[data-test-log-status]').hasText('待機中');
    assert.dom('[data-test-app-state]').hasText('デプロイ中');
    assert.dom('[data-test-stream-state]').hasText('ログを受信中…');

    stream.emit('Cloning octo/blog');
    stream.emit('Building web');
    await settled();

    assert
      .dom('[data-test-log-text]')
      .includesText('Cloning octo/blog')
      .includesText('Building web');
    assert.false(stream.closed);

    deployments = [
      makeDeployment({ id: 43, status: 'succeeded' }),
      ...deployments.slice(1),
    ];
    stream.emit('Deployed');
    stream.end('succeeded');
    await settled();

    assert.true(stream.closed, 'close() is called so EventSource stops');
    assert.dom('[data-test-log-text]').includesText('Deployed');
    assert.dom('[data-test-stream-state]').hasText('ログの終わりです。');
    assert
      .dom('[data-test-log-status]')
      .hasText('成功', 'the status is refreshed when the log ends');
    assert.dom('[data-test-deployment="43"] [data-test-cancel]').doesNotExist();
  });

  test('a deployment in progress is followed and polled', async function (assert) {
    deployments = [
      makeDeployment({ id: 50, status: 'running', finished_at: null }),
      makeDeployment({ id: 42 }),
    ];
    serveApp();

    await visit('/apps/blog/deploy');

    assert.strictEqual(
      fakes.eventSource.last.url,
      '/api/deployments/50/logs',
      'the running deployment is selected on arrival',
    );
    assert
      .dom('[data-test-deployment="50"] [data-test-deployment-status]')
      .hasText('実行中');

    deployments = [
      makeDeployment({
        id: 50,
        status: 'failed',
        error: 'web did not become healthy',
      }),
      makeDeployment({ id: 42 }),
    ];
    await fakes.poller.tick();

    assert
      .dom('[data-test-deployment="50"] [data-test-deployment-status]')
      .hasText('失敗');
    assert
      .dom('[data-test-deployment-error]')
      .hasText('web did not become healthy');
    assert.dom('[data-test-app-state]').hasText('直近のデプロイに失敗');
  });

  test('a queued or running deployment can be canceled', async function (assert) {
    deployments = [
      makeDeployment({ id: 50, status: 'running', finished_at: null }),
    ];
    serveApp();
    fakes.api.on('POST', '/api/deployments/50/cancel', () => {
      deployments = [makeDeployment({ id: 50, status: 'canceled' })];
      return deployments[0];
    });

    await visit('/apps/blog/deploy');
    await click('[data-test-deployment="50"] [data-test-cancel]');

    assert.strictEqual(
      fakes.api.callsTo('POST', '/api/deployments/50/cancel').length,
      1,
    );
    assert
      .dom('[data-test-deployment="50"] [data-test-deployment-status]')
      .hasText('キャンセル');
    assert.dom('[data-test-cancel]').doesNotExist();
  });

  test('a history row redeploys its commit (rollback)', async function (assert) {
    deployments = [
      makeDeployment({
        id: 44,
        commit_sha: SHA_B,
        commit_message: 'Add search',
      }),
      makeDeployment({ id: 42 }),
    ];
    serveApp({ environment: 'production' });

    await visit('/apps/blog/deploy');
    await click('[data-test-deployment="42"] [data-test-redeploy]');

    assert
      .dom('[data-test-dialog-target]')
      .includesText(SHA_A.slice(0, 7), 'production still asks first');

    await click('[data-test-dialog-confirm]');

    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/deployments')[0]?.body,
      { ref: 'main', sha: SHA_A },
    );
  });

  test('a refused deployment explains why', async function (assert) {
    serveApp();
    fakes.api.fail('POST', '/api/apps/blog/deployments', 507, 'disk_full', {
      used_percent: 91.2,
      limit_percent: 85,
    });

    await visit('/apps/blog/deploy');
    await click('[data-test-deploy-submit]');

    assert
      .dom('[data-test-error]')
      .includesText('ディスク使用率が上限を超えている')
      .includesText('91.2%')
      .includesText('85%');
  });

  test('without branch lists a ref or SHA is typed by hand', async function (assert) {
    serveApp();
    fakes.api
      .fail('GET', '/api/repos/octo/blog/branches', 503, 'not_configured')
      .fail('GET', '/api/repos/octo/blog/commits', 503, 'not_configured');

    await visit('/apps/blog/deploy');

    assert.dom('[data-test-deploy-branch]').doesNotExist();
    assert.dom('[data-test-deploy-ref]').hasValue('main');

    await click('[data-test-deploy-submit]');
    await fillIn('[data-test-deploy-ref]', SHA_B);
    await click('[data-test-deploy-submit]');

    const bodies = fakes.api
      .callsTo('POST', '/api/apps/blog/deployments')
      .map((call) => call.body);
    assert.deepEqual(bodies, [{ ref: 'main' }, { sha: SHA_B }]);
  });
});
