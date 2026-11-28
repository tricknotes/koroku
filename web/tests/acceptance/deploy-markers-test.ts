import { module, test } from 'qunit';
import {
  click,
  find,
  findAll,
  focus,
  settled,
  triggerEvent,
  triggerKeyEvent,
  visit,
  waitUntil,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import { accessibilityProblems } from 'koroku-console/tests/helpers/a11y';
import {
  SHA_A,
  SHA_B,
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type {
  App,
  Deployment,
  MemoryHistory,
} from 'koroku-console/utils/types';

const MIB = 1024 ** 2;
const HOUR = 3600_000;
const NOW = Date.parse('2026-11-21T03:00:00Z');

function iso(ms: number): string {
  return new Date(ms).toISOString();
}

function history(): MemoryHistory {
  const points = [0, 1, 2].map((i) => ({
    at: iso(NOW - (3 - i) * 8 * HOUR),
    max_bytes: 300 * MIB,
    avg_bytes: 280 * MIB,
    limit_bytes: 512 * MIB,
  }));
  return {
    range: '24h',
    step_seconds: 300,
    from: iso(NOW - 24 * HOUR),
    to: iso(NOW),
    services: ['db', 'web'].map((name) => ({
      name,
      limit_bytes: 512 * MIB,
      peak_bytes: 300 * MIB,
      restarts: 0,
      points,
      suggestion: {
        basis_since: iso(NOW - 14 * 24 * HOUR),
        basis_hours: 336,
        peak_bytes: 300 * MIB,
        p95_bytes: 300 * MIB,
        ready: true,
        limit_bytes: 512 * MIB,
        limit: '512m',
        flag: null,
      },
    })),
  };
}

function finished(
  id: number,
  hoursAgo: number,
  overrides: Partial<Deployment> = {},
): Deployment {
  const at = iso(NOW - hoursAgo * HOUR);
  return makeDeployment({
    id,
    commit_sha: SHA_A,
    commit_message: `Change ${id}\n\nDetails`,
    queued_at: at,
    started_at: at,
    finished_at: at,
    actor: { id: 1, login: 'octo' },
    trigger: 'manual',
    pusher: '',
    source_deployment_id: null,
    promoted_from: null,
    containers_replaced: true,
    ...overrides,
  });
}

/** Lets the resize observer report and the page render what it reported. */
async function nextFrames(): Promise<void> {
  for (let i = 0; i < 2; i++) {
    await new Promise((resolve) => requestAnimationFrame(resolve));
  }
  await settled();
}

const MARKER = '[data-test-deploy-marker]';
const POPUP = '[data-test-deploy-marker-popup]';

module('Acceptance | deploy markers', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let deployments: Deployment[];

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const current = finished(40, 2, {
      commit_sha: SHA_B,
      commit_message: 'Add reading time',
    });
    deployments = [
      current,
      // A failed build: never touched the containers.
      finished(39, 6, { status: 'failed', containers_replaced: false }),
      finished(38, 10, { status: 'failed', commit_message: 'Bump Rails' }),
      finished(37, 20, {
        trigger: 'push',
        pusher: 'mia',
        actor: null,
        commit_message: 'Update dependencies',
      }),
      // Before the range: asked for with a margin, left out by the chart.
      finished(36, 30),
    ];
    app = makeApp({
      current_deployment: current,
      latest_deployment: current,
    });
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on('GET', '/api/apps/blog', () => app)
      .on('GET', '/api/apps/blog/memory', history())
      .on('GET', '/api/apps/blog/deployments', () => ({
        deployments,
        pending_auto_deploy: null,
      }));
  });

  test('it marks the deployments of the range and tells what each was', async function (assert) {
    await visit('/apps/blog/resources');

    const call = fakes.api.callsTo('GET', '/api/apps/blog/deployments')[0];
    assert.strictEqual(call?.query['limit'], '200');
    assert.ok(
      call?.query['finished_since'],
      'only the deployments of the range',
    );

    assert.dom(MARKER).exists({ count: 3 }, 'the failed build is not marked');
    assert
      .dom('[data-test-memory-service="web"] [data-test-memory-chart-marker]')
      .exists({ count: 3 }, 'every chart draws the lines');
    assert.dom('[data-test-memory-chart-marker="38"]').hasClass('is-reverted');
    assert.dom('[data-test-legend-reverted]').hasText('失敗して戻したデプロイ');

    const buttons = findAll(MARKER);
    assert.deepEqual(
      buttons.map((b) => b.getAttribute('aria-label')),
      [
        '11/20 16:00 のデプロイ 9fceb02',
        '11/21 02:00 の失敗して戻したデプロイ 9fceb02',
        '11/21 10:00 のデプロイ 1b2c3d4',
      ],
      'oldest first, named by time (JST), kind and commit',
    );
    assert.dom(POPUP).isNotVisible();

    // Hover shows the details; leaving hides them.
    const holder = '[data-test-deploy-marker-group="0"]';
    await triggerEvent(holder, 'pointerenter', { pointerType: 'mouse' });
    assert.dom(`${holder} ${POPUP}`).isVisible();
    assert.dom(`${holder} [data-test-deploy-marker-ref]`).hasText('main');
    assert
      .dom(`${holder} [data-test-deploy-marker-trigger]`)
      .hasText('push で自動デプロイ（mia）');
    assert
      .dom(`${holder} [data-test-deploy-marker-time]`)
      .hasText('2026/11/20 16:00 JST');
    assert.dom(`${holder} [data-test-deploy-marker-result]`).hasText('成功');
    assert
      .dom(`${holder} [data-test-deploy-marker-link]`)
      .hasAttribute('href', '/apps/blog/deploy?deployment=37')
      .hasText('デプロイ #37 を開く');
    await triggerEvent(holder, 'pointerleave', { pointerType: 'mouse' });
    assert.dom(`${holder} ${POPUP}`).isNotVisible();

    // Focus shows them too, and Escape closes them with focus kept.
    await focus(`[data-test-deploy-marker-group="1"] ${MARKER}`);
    assert
      .dom(`[data-test-deploy-marker-group="1"] ${MARKER}`)
      .hasAttribute('aria-expanded', 'true');
    assert
      .dom(
        '[data-test-deploy-marker-group="1"] [data-test-deploy-marker-result]',
      )
      .hasText('失敗して戻した');
    assert.deepEqual(accessibilityProblems(), []);
    await triggerKeyEvent(
      `[data-test-deploy-marker-group="1"] ${MARKER}`,
      'keydown',
      'Escape',
    );
    assert.dom(`[data-test-deploy-marker-group="1"] ${POPUP}`).isNotVisible();
    assert.dom(`[data-test-deploy-marker-group="1"] ${MARKER}`).isFocused();

    // The running release says so.
    await focus(`[data-test-deploy-marker-group="2"] ${MARKER}`);
    assert
      .dom('[data-test-deploy-marker-group="2"] [data-test-deploy-marker-live]')
      .hasText('稼働中');
  });

  test('a tap opens a marker until it is tapped again or elsewhere', async function (assert) {
    await visit('/apps/blog/resources');
    const button = `[data-test-deploy-marker-group="2"] ${MARKER}`;
    await click(button);
    assert.dom(`[data-test-deploy-marker-group="2"] ${POPUP}`).isVisible();
    await click(button);
    assert.dom(`[data-test-deploy-marker-group="2"] ${POPUP}`).isNotVisible();

    await click(button);
    await click('[data-test-memory-history] h2');
    assert.dom(`[data-test-deploy-marker-group="2"] ${POPUP}`).isNotVisible();
  });

  test('a width that keeps the groups keeps the open marker', async function (assert) {
    await visit('/apps/blog/resources');
    const button = `[data-test-deploy-marker-group="2"] ${MARKER}`;
    await click(button);
    const before = find(button);

    // The track's width is first reported after the page settles, so it can
    // land after the first tap; a resize does the same.
    find('[data-test-deploy-markers]')!.setAttribute('style', 'width: 900px');
    await nextFrames();
    assert.strictEqual(find(button), before, 'the same button');
    assert.dom(button).isFocused();
    assert.dom(`[data-test-deploy-marker-group="2"] ${POPUP}`).isVisible();
  });

  test('on a narrow chart, close markers share one button', async function (assert) {
    deployments = [
      finished(40, 2),
      finished(39, 3.5, { source_deployment_id: 38, ref: 'rollback' }),
      finished(38, 12),
    ];
    app = {
      ...app,
      current_deployment: deployments[2]!,
      latest_deployment: deployments[1]!,
    };
    await visit('/apps/blog/resources');
    assert.dom(MARKER).exists({ count: 3 });

    // A phone's track: 90 minutes of 24 hours is under 24px there.
    find('[data-test-deploy-markers]')!.setAttribute('style', 'width: 330px');
    await waitUntil(() => findAll(MARKER).length === 2);
    const shared = '[data-test-deploy-marker-group="1"]';
    assert
      .dom(`${shared} ${MARKER}`)
      .hasAttribute('aria-label', 'デプロイ 2 件（11/21 08:30〜11/21 10:00）')
      .hasText('2');
    await click(`${shared} ${MARKER}`);
    assert.deepEqual(
      findAll(`${shared} [data-test-deploy-marker-item]`).map((e) =>
        e.getAttribute('data-test-deploy-marker-item'),
      ),
      ['40', '39'],
      'newest first',
    );
    assert
      .dom(
        `${shared} [data-test-deploy-marker-item="39"] [data-test-deploy-marker-ref]`,
      )
      .hasText('#38 の版へ');
    assert
      .dom(
        `${shared} [data-test-deploy-marker-item="39"] [data-test-deploy-marker-trigger]`,
      )
      .hasText('ロールバック（octo）');
  });

  test('a viewer sees the markers and their links', async function (assert) {
    app = { ...app, role: 'view' };
    await visit('/apps/blog/resources');
    assert.dom(MARKER).exists({ count: 3 });
    await focus(MARKER);
    assert
      .dom('[data-test-deploy-marker-link]')
      .hasAttribute('href', '/apps/blog/deploy?deployment=37');
  });

  test('without the deployments the charts stay as they are', async function (assert) {
    fakes.api.fail('GET', '/api/apps/blog/deployments', 500, 'internal');
    await visit('/apps/blog/resources');
    assert.dom('[data-test-memory-service]').exists({ count: 2 });
    assert.dom('[data-test-deploy-markers]').doesNotExist();
    assert.dom('[data-test-memory-chart-marker]').doesNotExist();
    assert.dom('[data-test-memory-history] [data-test-error]').doesNotExist();
  });

  test('the usage now names the running release', async function (assert) {
    await visit('/apps/blog/resources');
    assert.dom('[data-test-current-release]').includesText('稼働中のリリース');
    assert
      .dom('[data-test-current-release-commit]')
      .hasText('1b2c3d4')
      .hasAttribute('href', `https://github.com/octo/blog/commit/${SHA_B}`);
    assert
      .dom('[data-test-current-release-link]')
      .hasAttribute('href', '/apps/blog/deploy?deployment=40');
    assert.dom('[data-test-current-release-time]').includesText('デプロイ');
  });

  test('after a rollback the release is dated by the rollback', async function (assert) {
    const release = finished(30, 100, { commit_message: 'Old release' });
    const back = finished(41, 1, {
      ref: 'rollback',
      source_deployment_id: 30,
    });
    app = { ...app, current_deployment: release, latest_deployment: back };
    await visit('/apps/blog/resources');
    assert.dom('[data-test-current-release]').includesText('Old release');
    assert.dom('[data-test-current-release-time]').includesText('ロールバック');
    assert
      .dom('[data-test-current-release-time] time')
      .hasAttribute('datetime', back.finished_at!);
    assert
      .dom('[data-test-current-release-link]')
      .hasAttribute('href', '/apps/blog/deploy?deployment=41');
  });
});
