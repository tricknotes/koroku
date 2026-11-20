import { module, test } from 'qunit';
import {
  canRollBack,
  isReleaseGone,
  isRollback,
  releaseId,
  rollbackSummary,
} from 'koroku-console/utils/rollback';
import type { Deployment } from 'koroku-console/utils/types';

function deployment(overrides: Partial<Deployment> = {}): Deployment {
  return {
    id: 10,
    app_slug: 'blog',
    ref: 'main',
    commit_sha: '9fceb02d0ae598e95dc970b74767f19372d61af8',
    commit_message: 'Fix login',
    status: 'succeeded',
    error: '',
    queued_at: '2026-11-18T03:00:00Z',
    started_at: '2026-11-18T03:00:01Z',
    finished_at: '2026-11-18T03:01:10Z',
    source_deployment_id: null,
    rollback_available: true,
    ...overrides,
  };
}

module('Unit | Utility | rollback', function () {
  test('a rollback runs the release of its source', function (assert) {
    const plain = deployment();
    const rollback = deployment({ id: 13, source_deployment_id: 10 });

    assert.false(isRollback(plain));
    assert.false(
      isRollback(deployment({ source_deployment_id: undefined })),
      'an older backend says nothing',
    );
    assert.true(isRollback(rollback));
    assert.strictEqual(releaseId(plain), 10);
    assert.strictEqual(releaseId(rollback), 10);
    assert.strictEqual(rollbackSummary(rollback), '#10 の版を再起動');
  });

  test('only a kept release that does not run yet can be rolled back to', function (assert) {
    const current = { id: 12 };

    assert.true(canRollBack(deployment(), current));
    assert.false(
      canRollBack(deployment({ id: 12 }), current),
      'not the running release',
    );
    assert.false(
      canRollBack(deployment({ id: 13, source_deployment_id: 12 }), current),
      'not a rollback to the running release either',
    );
    assert.true(
      canRollBack(deployment({ id: 13, source_deployment_id: 10 }), current),
      'a rollback to another kept release',
    );
    assert.false(
      canRollBack(deployment({ rollback_available: false }), current),
    );
    assert.false(
      canRollBack(deployment({ rollback_available: undefined }), current),
      'an older backend offers no rollback',
    );
    assert.false(
      canRollBack(deployment({ status: 'running' }), current),
      'not while it runs',
    );
    assert.true(canRollBack(deployment(), null), 'nothing runs at all');
  });

  test('a release that is not kept is gone only when the backend says so', function (assert) {
    const current = { id: 12 };

    assert.true(
      isReleaseGone(deployment({ rollback_available: false }), current),
    );
    assert.false(isReleaseGone(deployment(), current), 'kept');
    assert.false(
      isReleaseGone(deployment({ rollback_available: undefined }), current),
      'an older backend does not tell',
    );
    assert.false(
      isReleaseGone(
        deployment({ status: 'failed', rollback_available: false }),
        current,
      ),
      'a failed deployment never had a release',
    );
    assert.false(
      isReleaseGone(deployment({ id: 12, rollback_available: false }), current),
      'the running release is not gone',
    );
  });
});
