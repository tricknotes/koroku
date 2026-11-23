import { module, test } from 'qunit';
import { click, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  SHA_A,
  SHA_B,
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, Deployment } from 'koroku-console/utils/types';

const SHA_C = 'c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00';

/** A step headline `seconds` after 03:00:00, as koroku writes it. */
function head(seconds: number, key: string, title: string): string {
  const time = new Date(Date.UTC(2026, 10, 18, 3, 0, seconds))
    .toISOString()
    .replace('.000', '');
  return `==> ${time} ${key} ${title}`;
}

function stepNames(): string[] {
  return [...document.querySelectorAll('[data-test-step] .step-name')].map(
    (name) => name.textContent?.trim() ?? '',
  );
}

module('Acceptance | rollback', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let deployments: Deployment[];
  let current: Deployment;

  /**
   * Newest first: #44 runs; #43 and #42 are kept; #41 is not kept any
   * more.
   */
  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    current = makeDeployment({
      id: 44,
      commit_sha: SHA_C,
      commit_message: 'Break checkout',
      rollback_available: true,
      source_deployment_id: null,
    });
    deployments = [
      current,
      makeDeployment({
        id: 43,
        commit_sha: SHA_B,
        commit_message: 'Add search',
        rollback_available: true,
        source_deployment_id: null,
      }),
      makeDeployment({
        id: 42,
        rollback_available: true,
        source_deployment_id: null,
      }),
      makeDeployment({
        id: 41,
        commit_message: 'Initial import',
        rollback_available: false,
        source_deployment_id: null,
      }),
    ];
    fakes.api
      .on('GET', '/api/repos/octo/blog/branches', {
        branches: [{ name: 'main', sha: SHA_C }],
      })
      .on('GET', '/api/repos/octo/blog/commits', { commits: [] });
  });

  function serveApp(overrides: Partial<App> = {}): void {
    const app = makeApp(overrides);
    fakes.api
      .on('GET', `/api/apps/${app.slug}`, () => ({
        ...app,
        current_deployment: current,
        latest_deployment: deployments[0] ?? null,
      }))
      .on('GET', `/api/apps/${app.slug}/deployments`, () => ({ deployments }))
      .on('POST', `/api/apps/${app.slug}/deployments`, () => {
        const created = makeDeployment({
          id: 45,
          app_slug: app.slug,
          ref: 'rollback',
          commit_sha: SHA_B,
          commit_message: 'Add search',
          status: 'queued',
          started_at: null,
          finished_at: null,
          source_deployment_id: 43,
          rollback_available: false,
        });
        deployments = [created, ...deployments];
        return created;
      });
  }

  test('kept releases offer "この版に戻す"; the running one and gone ones do not', async function (assert) {
    serveApp();
    await visit('/apps/blog/deploy');

    assert
      .dom('[data-test-deployment="44"] [data-test-rollback]')
      .doesNotExist();
    assert
      .dom('[data-test-deployment="44"] [data-test-redeploy]')
      .exists('the running release can be rebuilt');
    assert
      .dom('[data-test-deployment="43"] [data-test-rollback]')
      .hasText('この版に戻す');
    assert.dom('[data-test-deployment="42"] [data-test-rollback]').exists();
    assert
      .dom('[data-test-deployment="41"] [data-test-rollback]')
      .doesNotExist();
    assert
      .dom('[data-test-deployment="41"] [data-test-redeploy]')
      .hasAttribute('title', /保持していない/);
  });

  test('a staging rollback confirms what it does, then opens its live log', async function (assert) {
    serveApp({ slug: 'blog-staging' });
    await visit('/apps/blog-staging/deploy');

    await click('[data-test-deployment="43"] [data-test-rollback]');

    assert.dom('[data-test-dialog]').includesText('この版に戻しますか？');
    assert.dom('[data-test-rollback-production]').doesNotExist();
    assert
      .dom('[data-test-rollback-dialog-target]')
      .includesText('#43')
      .includesText(SHA_B.slice(0, 7))
      .includesText('Add search');
    assert.dom('[data-test-rollback-dialog]').includesText('ビルドはしません');
    assert.dom('[data-test-rollback-env]').includesText('環境変数');
    assert
      .dom('[data-test-rollback-migration]')
      .includesText('マイグレーション');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog-staging/deployments'),
      [],
      'nothing starts before confirming',
    );

    await click('[data-test-dialog-confirm]');

    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog-staging/deployments')[0]?.body,
      { rollback_to: 43 },
    );
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.dom('[data-test-deployment="45"] [data-test-log]').exists();
    assert.strictEqual(
      fakes.eventSource.last.url,
      '/api/deployments/45/logs',
      'its log streams in',
    );
    assert
      .dom('[data-test-deployment="45"] [data-test-rollback-badge]')
      .hasText('ロールバック');
    assert
      .dom('[data-test-deployment="45"] [data-test-rollback-source]')
      .hasText('デプロイ #43');

    fakes.eventSource.last.emit('Queued rollback to deployment #43 (x).');
    fakes.eventSource.last.emit(
      head(1, 'prepare', 'Reusing the release of deployment #43'),
    );
    await settled();
    assert.deepEqual(
      stepNames(),
      ['受付', '準備', '検証', '起動', '確認', '完了'],
      'no fetch, build or pull is waited for',
    );
  });

  test('a production rollback says it is production and asks to log in again when needed', async function (assert) {
    serveApp({ environment: 'production' });
    fakes.api.fail(
      'POST',
      '/api/apps/blog/deployments',
      403,
      'reauth_required',
    );
    await visit('/apps/blog/deploy');

    await click('[data-test-deployment="42"] [data-test-rollback]');
    assert
      .dom('[data-test-dialog]')
      .includesText('本番アプリをこの版に戻しますか？');
    assert.dom('[data-test-rollback-production]').hasText('本番');

    await click('[data-test-dialog-confirm]');

    assert
      .dom('[data-test-rollback-dialog]')
      .doesNotExist('the rollback dialog gives way');
    assert.dom('[data-test-reauth-dialog]').exists();
  });

  test('a release that went away meanwhile is explained in the dialog', async function (assert) {
    serveApp();
    fakes.api.fail(
      'POST',
      '/api/apps/blog/deployments',
      422,
      'release_unavailable',
    );
    await visit('/apps/blog/deploy');

    await click('[data-test-deployment="43"] [data-test-rollback]');
    // The release is cleaned up while the dialog is open.
    deployments = deployments.map((d) =>
      d.id === 43 ? { ...d, rollback_available: false } : d,
    );
    await click('[data-test-dialog-confirm]');

    assert
      .dom('[data-test-rollback-dialog] [data-test-error]')
      .includesText('もう保持していない')
      .includesText('再デプロイ');

    await click('[data-test-dialog-cancel]');
    assert
      .dom('[data-test-deployment="43"] [data-test-rollback]')
      .doesNotExist('the history was read again');
    assert.dom('[data-test-deployment="43"] [data-test-redeploy]').exists();
  });

  test('a release that is not kept explains the way back and offers it', async function (assert) {
    serveApp({ slug: 'blog-staging' });
    fakes.api.on('POST', '/api/apps/blog-staging/deployments', () =>
      makeDeployment({ id: 45, status: 'queued', finished_at: null }),
    );
    await visit('/apps/blog-staging/deploy');

    await click('[data-test-deployment="41"] [data-test-show-log]');
    assert
      .dom('[data-test-deployment="41"] [data-test-release-gone]')
      .includesText('もう保持していない')
      .includesText('再デプロイ');

    await click('[data-test-deployment="41"] [data-test-detail-redeploy]');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog-staging/deployments')[0]?.body,
      { ref: 'main', sha: SHA_A },
      'the commit is deployed again',
    );
  });

  test('a kept release offers both ways in its detail', async function (assert) {
    serveApp({ slug: 'blog-staging' });
    await visit('/apps/blog-staging/deploy');

    await click('[data-test-deployment="42"] [data-test-show-log]');
    assert.dom('[data-test-deployment="42"] [data-test-release-kept]').exists();
    assert
      .dom('[data-test-deployment="42"] [data-test-detail-redeploy]')
      .exists();

    await click('[data-test-deployment="42"] [data-test-detail-rollback]');
    assert.dom('[data-test-rollback-dialog-target]').includesText('#42');
  });

  test('a rollback record names the release it started again', async function (assert) {
    deployments = [
      makeDeployment({
        id: 45,
        ref: 'rollback',
        commit_sha: SHA_B,
        commit_message: 'Add search',
        source_deployment_id: 43,
        rollback_available: true,
      }),
      ...deployments,
    ];
    current = deployments[2] as Deployment;
    serveApp();
    await visit('/apps/blog/deploy');

    assert
      .dom('[data-test-deployment="45"] [data-test-rollback-summary]')
      .hasText('#43 の版を再起動');
    assert
      .dom('[data-test-deployment="45"]')
      .doesNotIncludeText('rollback', 'the ref is not shown as a branch');
    assert
      .dom('[data-test-deployment="45"] [data-test-rollback]')
      .doesNotExist('its release is the running one');
    assert.dom('[data-test-deployment="43"] [data-test-live]').exists();
    assert
      .dom('[data-test-deployment="44"] [data-test-rollback]')
      .exists('the release it replaced can be brought back');

    await click('[data-test-deployment="45"] [data-test-show-log]');
    assert
      .dom('[data-test-deployment="45"] [data-test-rollback-summary]')
      .exists({ count: 2 });
    assert
      .dom('[data-test-deployment="45"] [data-test-log]')
      .includesText('ビルドせずに起動し直すロールバック');
  });

  test('redeploying a rollback record deploys from the ref of its release', async function (assert) {
    deployments = [
      makeDeployment({
        id: 46,
        ref: 'rollback',
        commit_sha: SHA_B,
        commit_message: 'Add search',
        source_deployment_id: 43,
        rollback_available: true,
      }),
      ...deployments.map((d) => (d.id === 43 ? { ...d, ref: 'release' } : d)),
    ];
    current = deployments[2] as Deployment;
    serveApp({ slug: 'blog-staging' });
    await visit('/apps/blog-staging/deploy');

    await click('[data-test-deployment="46"] [data-test-redeploy]');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog-staging/deployments')[0]?.body,
      { ref: 'release', sha: SHA_B },
      'the marker of the rollback is not sent as a branch',
    );
  });
});
