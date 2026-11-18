import { module, test } from 'qunit';
import {
  backupHealth,
  latestFinishedRun,
} from 'koroku-console/utils/backup-status';
import { timeUntil } from 'koroku-console/utils/schedule';
import { makeBackups } from 'koroku-console/tests/helpers/fakes';
import type { BackupRun } from 'koroku-console/utils/types';

function run(id: number, status: BackupRun['status']): BackupRun {
  return {
    id,
    trigger: 'scheduled',
    status,
    started_at: '2026-11-17T18:00:00Z',
    finished_at: status === 'running' ? null : '2026-11-17T18:00:40Z',
    error: status === 'failed' ? 'repository is locked' : '',
  };
}

module('Unit | Utility | backup-status', function () {
  test('healthy backups are quiet', function (assert) {
    const health = backupHealth(makeBackups({ runs: [run(2, 'succeeded')] }));

    assert.strictEqual(health.kind, 'running');
    assert.strictEqual(health.label, '正常');
    assert.true(health.quiet);
    assert.strictEqual(health.failedRun, null);
    assert.false(health.stale);
  });

  test('a running backup wins over everything else', function (assert) {
    const health = backupHealth(
      makeBackups({
        running: true,
        warning: true,
        runs: [run(3, 'running'), run(2, 'failed')],
      }),
    );

    assert.strictEqual(health.kind, 'deploying');
    assert.strictEqual(health.label, '実行中');
    assert.false(health.quiet);
    assert.strictEqual(
      health.failedRun?.id,
      2,
      'the failure before it is still reported',
    );
  });

  test('a failed latest run is a failure, an older one is not', function (assert) {
    const failed = backupHealth(
      makeBackups({ runs: [run(3, 'failed'), run(2, 'succeeded')] }),
    );
    assert.strictEqual(failed.kind, 'failed');
    assert.strictEqual(failed.failedRun?.error, 'repository is locked');

    const recovered = backupHealth(
      makeBackups({ runs: [run(3, 'succeeded'), run(2, 'failed')] }),
    );
    assert.strictEqual(recovered.kind, 'running');
    assert.strictEqual(recovered.failedRun, null);
  });

  test('no success for a day is pending, with or without a first success', function (assert) {
    const stale = backupHealth(makeBackups({ warning: true }));
    assert.strictEqual(stale.kind, 'pending');
    assert.strictEqual(stale.label, '24 時間以上成功なし');
    assert.true(stale.stale);

    const never = backupHealth(
      makeBackups({ warning: true, last_success_at: null }),
    );
    assert.strictEqual(never.label, 'まだ成功していません');
  });

  test('backups that are not configured are neither fine nor failing', function (assert) {
    const health = backupHealth(
      makeBackups({ configured: false, warning: true, runs: null }),
    );

    assert.strictEqual(health.kind, 'stopped');
    assert.strictEqual(health.label, '未設定');
    assert.false(health.stale);
  });

  test('latestFinishedRun skips the run in progress', function (assert) {
    assert.strictEqual(
      latestFinishedRun(
        makeBackups({ runs: [run(3, 'running'), run(2, 'succeeded')] }),
      )?.id,
      2,
    );
    assert.strictEqual(latestFinishedRun(makeBackups({ runs: null })), null);
  });
});

module('Unit | Utility | schedule', function () {
  test('timeUntil words the distance to a future time', function (assert) {
    const now = Date.parse('2026-11-18T03:00:00Z');

    assert.strictEqual(timeUntil('2026-11-18T03:00:30Z', now), 'まもなく');
    assert.strictEqual(timeUntil('2026-11-18T03:25:00Z', now), '25 分後');
    assert.strictEqual(timeUntil('2026-11-18T18:00:00Z', now), '15 時間後');
    assert.strictEqual(timeUntil('2026-11-20T04:00:00Z', now), '2 日後');
  });

  test('timeUntil treats a time that has come as due', function (assert) {
    const now = Date.parse('2026-11-18T03:00:00Z');

    assert.strictEqual(timeUntil('2026-11-18T02:00:00Z', now), 'まもなく');
  });

  test('timeUntil is empty without a readable time', function (assert) {
    assert.strictEqual(timeUntil(null), '');
    assert.strictEqual(timeUntil(undefined), '');
    assert.strictEqual(timeUntil('not a time'), '');
  });
});
