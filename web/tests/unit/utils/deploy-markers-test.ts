import { module, test } from 'qunit';
import {
  deployMarkers,
  groupLabel,
  groupMarkers,
  liveDeploymentId,
  markerKind,
  rangeStart,
  refText,
  resultText,
  triggerText,
} from 'koroku-console/utils/deploy-markers';
import type { Deployment } from 'koroku-console/utils/types';

const RANGE = { from: '2026-11-20T00:00:00Z', to: '2026-11-21T00:00:00Z' };

function deployment(
  id: number,
  finishedAt: string | null,
  overrides: Partial<Deployment> = {},
): Deployment {
  return {
    id,
    app_slug: 'blog',
    ref: 'main',
    commit_sha: `${id}`.padStart(40, 'a'),
    commit_message: `Change ${id}`,
    status: 'succeeded',
    error: '',
    queued_at: finishedAt,
    started_at: finishedAt,
    finished_at: finishedAt,
    source_deployment_id: null,
    promoted_from: null,
    actor: { id: 1, login: 'octo' },
    trigger: 'manual',
    pusher: '',
    containers_replaced: true,
    ...overrides,
  };
}

module('Unit | Utility | deploy-markers', function () {
  test('it marks the deployments that changed what runs', function (assert) {
    const at = '2026-11-20T06:00:00Z';
    assert.strictEqual(markerKind(deployment(1, at)), 'deploy');
    assert.strictEqual(
      markerKind(deployment(2, at, { source_deployment_id: 1 })),
      'deploy',
      'a rollback',
    );
    assert.strictEqual(
      markerKind(deployment(3, at, { status: 'failed' })),
      'reverted',
      'failed after starting the new release',
    );
    assert.strictEqual(
      markerKind(deployment(4, at, { status: 'canceled' })),
      'reverted',
    );
    assert.strictEqual(
      markerKind(
        deployment(5, at, { status: 'failed', containers_replaced: false }),
      ),
      null,
      'failed before touching the containers',
    );
    assert.strictEqual(
      markerKind(
        deployment(6, at, { status: 'failed', containers_replaced: null }),
      ),
      null,
      'not known',
    );
    assert.strictEqual(
      markerKind(deployment(7, null, { status: 'running' })),
      null,
      'not finished',
    );
  });

  test('it places markers in the range and leaves out the others', function (assert) {
    const markers = deployMarkers(
      [
        deployment(4, '2026-11-21T00:00:00Z'),
        deployment(3, '2026-11-20T12:00:00Z'),
        deployment(2, '2026-11-20T00:00:00Z'),
        deployment(1, '2026-11-19T23:59:59Z'),
        deployment(5, '2026-11-21T00:00:01Z'),
      ],
      RANGE,
    );
    assert.deepEqual(
      markers.map((m) => [m.deployment.id, m.left]),
      [
        [2, 0],
        [3, 50],
        [4, 100],
      ],
      'oldest first, the ends included',
    );
  });

  test('it places by the instant, whatever offset the time is written in', function (assert) {
    const [jst] = deployMarkers(
      [deployment(1, '2026-11-20T15:00:00+09:00')],
      RANGE,
    );
    const [utc] = deployMarkers([deployment(1, '2026-11-20T06:00:00Z')], {
      from: '2026-11-20T09:00:00+09:00',
      to: '2026-11-21T09:00:00+09:00',
    });
    assert.strictEqual(jst?.left, 25);
    assert.strictEqual(utc?.left, 25);
  });

  test('it groups markers too close to press apart', function (assert) {
    const markers = deployMarkers(
      [
        deployment(1, '2026-11-20T00:00:00Z'),
        deployment(2, '2026-11-20T00:20:00Z'),
        deployment(3, '2026-11-20T12:00:00Z'),
        deployment(4, '2026-11-20T12:30:00Z', {
          status: 'failed',
        }),
      ],
      RANGE,
    );
    // 20 minutes of 24 hours on 1200px is 17px: together. 30 minutes is 25px.
    const wide = groupMarkers(markers, 1200);
    assert.deepEqual(
      wide.map((g) => g.markers.map((m) => m.deployment.id)),
      [[2, 1], [3], [4]],
      'newest first within a group',
    );
    assert.strictEqual(wide[0]?.left, 0.7, 'between its first and last');
    assert.strictEqual(wide[2]?.kind, 'reverted');

    const narrow = groupMarkers(markers, 300);
    assert.deepEqual(
      narrow.map((g) => g.markers.map((m) => m.deployment.id)),
      [
        [2, 1],
        [4, 3],
      ],
    );
    assert.strictEqual(
      narrow[1]?.kind,
      'deploy',
      'a group is reverted only when all of it is',
    );

    assert.strictEqual(
      groupMarkers(markers, 0).length,
      4,
      'not laid out yet: one each',
    );
  });

  test('it says what each deployment was', function (assert) {
    const at = '2026-11-20T06:00:00Z';
    assert.strictEqual(triggerText(deployment(1, at)), '手動（octo）');
    assert.strictEqual(triggerText(deployment(1, at, { actor: null })), '手動');
    assert.strictEqual(
      triggerText(
        deployment(1, at, { trigger: 'push', pusher: 'mia', actor: null }),
      ),
      'push で自動デプロイ（mia）',
    );
    assert.strictEqual(
      triggerText(
        deployment(1, at, {
          promoted_from: { deployment_id: 7, app_slug: 'blog-staging' },
        }),
      ),
      'blog-staging から昇格（octo）',
    );
    const rollback = deployment(9, at, {
      ref: 'rollback',
      source_deployment_id: 4,
    });
    assert.strictEqual(triggerText(rollback), 'ロールバック（octo）');
    assert.strictEqual(refText(rollback), '#4 の版へ');
    assert.strictEqual(refText(deployment(1, at)), 'main');

    const [failed] = deployMarkers(
      [deployment(3, at, { status: 'failed' })],
      RANGE,
    );
    const [canceled] = deployMarkers(
      [deployment(3, at, { status: 'canceled' })],
      RANGE,
    );
    assert.strictEqual(resultText(failed!), '失敗して戻した');
    assert.strictEqual(resultText(canceled!), 'キャンセルして戻した');
  });

  test('it names the buttons with the time in JST', function (assert) {
    const markers = deployMarkers(
      [
        deployment(1, '2026-11-20T00:00:00Z'),
        deployment(2, '2026-11-20T00:10:00Z'),
        deployment(3, '2026-11-20T12:00:00Z', {
          status: 'failed',
          commit_sha: '9fceb02d0ae598e95dc970b74767f19372d61af8',
        }),
      ],
      RANGE,
    );
    const groups = groupMarkers(markers, 1200);
    assert.strictEqual(
      groupLabel(groups[0]!),
      'デプロイ 2 件（11/20 09:00〜11/20 09:10）',
    );
    assert.strictEqual(
      groupLabel(groups[1]!),
      '11/20 21:00 の失敗して戻したデプロイ 9fceb02',
    );
  });

  test('it finds the deployment that runs now', function (assert) {
    const first = deployment(1, '2026-11-20T01:00:00Z');
    const second = deployment(2, '2026-11-20T02:00:00Z');
    const back = deployment(3, '2026-11-20T03:00:00Z', {
      source_deployment_id: 1,
    });
    const failed = deployment(4, '2026-11-20T04:00:00Z', { status: 'failed' });
    assert.strictEqual(
      liveDeploymentId([failed, back, second, first], first),
      3,
    );
    assert.strictEqual(liveDeploymentId([second, first], second), 2);
    assert.strictEqual(
      liveDeploymentId([second, first], deployment(9, null)),
      null,
      'the running release was deployed before the range',
    );
    assert.strictEqual(liveDeploymentId([second], null), null);
  });

  test('it asks for a little more than the range', function (assert) {
    const now = Date.parse('2026-11-21T00:00:00Z');
    assert.strictEqual(rangeStart('24h', now), '2026-11-19T23:50:00.000Z');
    assert.strictEqual(rangeStart('7d', now), '2026-11-13T23:50:00.000Z');
  });
});
