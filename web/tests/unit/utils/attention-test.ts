import { module, test } from 'qunit';
import {
  attentionItems,
  diskLimitPercent,
  diskOverLimit,
} from 'koroku-console/utils/attention';
import {
  makeApp,
  makeBackups,
  makeDeployment,
  makeSystem,
} from 'koroku-console/tests/helpers/fakes';

function disk(usedPercent: number) {
  return {
    total_bytes: 100,
    used_bytes: usedPercent,
    used_percent: usedPercent,
  };
}

module('Unit | Utility | attention', function () {
  test('nothing needs attention on a healthy host', function (assert) {
    const ok = makeDeployment();
    const apps = [makeApp({ current_deployment: ok, latest_deployment: ok })];

    assert.deepEqual(attentionItems(apps, makeSystem(), makeBackups()), []);
    assert.deepEqual(
      attentionItems(null, null, null),
      [],
      'data that is not loaded contributes nothing',
    );
  });

  test('the disk is flagged above the limit the backend reports', function (assert) {
    assert.false(diskOverLimit(makeSystem({ disk: disk(85) })));
    assert.true(diskOverLimit(makeSystem({ disk: disk(85.1) })));
    assert.false(diskOverLimit(makeSystem({ disk: null })));
    assert.true(
      diskOverLimit(makeSystem({ disk: disk(72), disk_limit_percent: 70 })),
    );
    assert.strictEqual(
      diskLimitPercent(makeSystem({ disk_limit_percent: undefined })),
      85,
      'an older backend without the field uses the documented default',
    );

    assert.deepEqual(
      attentionItems([], makeSystem({ disk: disk(91.2) }), null),
      [{ kind: 'disk', usedPercent: 91.2, limitPercent: 85 }],
    );
  });

  test('a stale backup is flagged only when backups are configured', function (assert) {
    assert.deepEqual(
      attentionItems(
        [],
        null,
        makeBackups({ warning: true, last_success_at: '2026-11-16T18:00:40Z' }),
      ),
      [{ kind: 'backup', lastSuccessAt: '2026-11-16T18:00:40Z' }],
    );
    assert.deepEqual(
      attentionItems(
        [],
        null,
        makeBackups({ configured: false, warning: true }),
      ),
      [],
    );
  });

  test('apps are flagged for a failed latest deployment and pending domains', function (assert) {
    const ok = makeDeployment({ id: 1 });
    const failed = makeDeployment({ id: 2, status: 'failed' });
    const apps = [
      makeApp({
        slug: 'shop',
        pending_domains: 1,
        current_deployment: ok,
        latest_deployment: ok,
      }),
      makeApp({
        slug: 'shop-staging',
        current_deployment: ok,
        latest_deployment: failed,
      }),
      makeApp({
        slug: 'canceled',
        current_deployment: ok,
        latest_deployment: makeDeployment({ id: 3, status: 'canceled' }),
      }),
      makeApp({
        slug: 'deploying',
        latest_deployment: makeDeployment({ id: 4, status: 'running' }),
      }),
      makeApp({ slug: 'old-backend', pending_domains: undefined }),
    ];

    assert.deepEqual(attentionItems(apps, null, null), [
      { kind: 'deployment', slug: 'shop-staging', deployment: failed },
      { kind: 'domains', slug: 'shop', count: 1 },
    ]);
  });

  test('host items come before app items', function (assert) {
    const failed = makeDeployment({ status: 'failed' });
    const items = attentionItems(
      [makeApp({ pending_domains: 2, latest_deployment: failed })],
      makeSystem({ disk: disk(90) }),
      makeBackups({ warning: true }),
    );

    assert.deepEqual(
      items.map((item) => item.kind),
      ['disk', 'backup', 'deployment', 'domains'],
    );
  });
});
