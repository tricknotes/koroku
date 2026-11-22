import { module, test } from 'qunit';
import {
  containerStatus,
  createdBy,
  imageIdentity,
  memoryLimit,
  memoryReservation,
  mismatchMarkers,
  oomScoreAdj,
  portsText,
  releaseLabel,
} from 'koroku-console/utils/compose-view';
import { makeContainer } from 'koroku-console/tests/helpers/compose';

const MB = 1024 ** 2;

module('Unit | Utility | compose-view', function () {
  test('markers put troubles before differences', function (assert) {
    const c = makeContainer({
      restart_count: 2,
      exit_code: 137,
      mismatches: ['memory_limit', 'outdated', 'restarted', 'oom_killed'],
    });
    assert.deepEqual(
      mismatchMarkers(c).map((m) => [m.code, m.label, m.tone]),
      [
        ['oom_killed', 'メモリ不足で停止', 'danger'],
        ['restarted', '自動の再起動 2 回', 'warn'],
        ['outdated', '古い設定', 'warn'],
        ['memory_limit', 'メモリ上限が違う', 'warn'],
      ],
    );
    assert.strictEqual(
      mismatchMarkers(
        makeContainer({ exit_code: 1, mismatches: ['exited'] }),
      )[0]?.label,
      '異常終了（コード 1）',
    );
  });

  test('the state reads as one of the five kinds', function (assert) {
    const kind = (overrides: Parameters<typeof makeContainer>[0]) =>
      containerStatus(makeContainer(overrides));
    assert.deepEqual(kind({}), { kind: 'running', label: '稼働中' });
    assert.deepEqual(kind({ health: 'unhealthy' }), {
      kind: 'failed',
      label: '稼働中（不調）',
    });
    assert.deepEqual(kind({ health: 'starting' }), {
      kind: 'pending',
      label: '起動中',
    });
    assert.deepEqual(kind({ state: 'restarting' }), {
      kind: 'failed',
      label: '再起動中',
    });
    assert.deepEqual(kind({ state: 'exited' }), {
      kind: 'stopped',
      label: '停止',
    });
    assert.deepEqual(kind({ state: 'exited', exit_code: 137 }), {
      kind: 'failed',
      label: '停止（コード 137）',
    });
    assert.deepEqual(kind({ state: 'created' }), {
      kind: 'stopped',
      label: '停止',
    });
  });

  test('values are compared with the release', function (assert) {
    const c = makeContainer({
      memory_limit_bytes: 384 * MB,
      memory_reservation_bytes: 0,
      oom_score_adj: 0,
    });
    assert.deepEqual(memoryLimit(c), {
      value: '384.0 MB',
      expected: '512.0 MB',
      differs: true,
    });
    assert.deepEqual(memoryReservation(c), {
      value: 'なし',
      expected: '256.0 MB',
      differs: true,
    });
    assert.deepEqual(oomScoreAdj(c), {
      value: '0',
      expected: '-400',
      differs: true,
    });
    const uncompared = makeContainer({ expected: null });
    assert.false(memoryLimit(uncompared).differs);
    assert.strictEqual(memoryLimit(uncompared).expected, '');
  });

  test('the deployment and the image are described', function (assert) {
    assert.strictEqual(
      createdBy(makeContainer()),
      '#42 のデプロイ（イメージのタグから）',
    );
    assert.strictEqual(
      createdBy(makeContainer({ matched_by: 'created_at', deployment_id: 7 })),
      '#7 のデプロイ（作成時刻からの推定）',
    );
    assert.strictEqual(createdBy(makeContainer({ deployment_id: null })), '');
    assert.strictEqual(imageIdentity(makeContainer()), 'c'.repeat(12));
    assert.strictEqual(
      imageIdentity(
        makeContainer({ image_digest: `postgres@sha256:${'d'.repeat(64)}` }),
      ),
      `sha256:${'d'.repeat(12)}`,
    );
    assert.strictEqual(
      portsText(makeContainer()),
      '127.0.0.1:20000 → 3000/tcp',
    );
    assert.strictEqual(portsText(makeContainer({ ports: [] })), '');
  });

  test('releases are labelled by deployment, commit and message', function (assert) {
    const release = {
      deployment_id: 41,
      commit_sha: 'b'.repeat(40),
      commit_message: 'Add search\n\nLonger text',
      finished_at: null,
      current: false,
    };
    assert.strictEqual(
      releaseLabel(release),
      `#41 ${'b'.repeat(7)} Add search`,
    );
    assert.strictEqual(
      releaseLabel({ ...release, current: true }),
      `#41 ${'b'.repeat(7)} Add search（稼働中）`,
    );
  });
});
