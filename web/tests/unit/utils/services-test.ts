import { module, test } from 'qunit';
import {
  healthLabel,
  memoryUsage,
  serviceState,
} from 'paas-console/utils/services';

const MB = 1024 ** 2;

function state(name: string, status = '', health = '') {
  return serviceState({ state: name, status, health });
}

module('Unit | Utility | services', function () {
  test('serviceState maps docker states onto the shared vocabulary', function (assert) {
    assert.deepEqual(state('running', 'Up 2 hours', 'healthy'), {
      kind: 'running',
      label: '稼働中',
      problem: false,
    });
    assert.deepEqual(state('running', 'Up 2 hours'), {
      kind: 'running',
      label: '稼働中',
      problem: false,
    });
    assert.deepEqual(state('running', 'Up 3 seconds', 'starting'), {
      kind: 'deploying',
      label: '起動中',
      problem: false,
    });
    assert.deepEqual(state('running', 'Up 2 hours', 'unhealthy'), {
      kind: 'failed',
      label: '異常',
      problem: true,
    });
    assert.deepEqual(state('restarting', 'Restarting (1) 3 seconds ago'), {
      kind: 'pending',
      label: '再起動中',
      problem: true,
    });
    assert.deepEqual(state('dead'), {
      kind: 'failed',
      label: '異常終了',
      problem: true,
    });
    assert.strictEqual(state('created').label, '未起動');
    assert.strictEqual(state('paused').label, '一時停止');
    assert.deepEqual(state('removing'), {
      kind: 'stopped',
      label: 'removing',
      problem: false,
    });
    assert.strictEqual(state('').label, '不明');
  });

  test('an exit with code 0 is a finished job, any other code a failure', function (assert) {
    assert.deepEqual(state('exited', 'Exited (0) 2 days ago'), {
      kind: 'stopped',
      label: '終了',
      problem: false,
    });
    assert.deepEqual(state('exited', 'Exited (137) 4 minutes ago'), {
      kind: 'failed',
      label: '異常終了',
      problem: true,
    });
    assert.false(state('exited', '').problem, 'an unknown code is not flagged');
  });

  test('healthLabel', function (assert) {
    assert.strictEqual(healthLabel('healthy'), '正常');
    assert.strictEqual(healthLabel('unhealthy'), '異常');
    assert.strictEqual(healthLabel('starting'), '確認中');
    assert.strictEqual(healthLabel('other'), 'other');
  });

  test('memoryUsage', function (assert) {
    assert.deepEqual(
      memoryUsage({ memory_bytes: 256 * MB, memory_limit_bytes: 512 * MB }),
      { text: '256.0 MB / 512.0 MB', percent: 50, high: false },
    );
    assert.deepEqual(
      memoryUsage({ memory_bytes: 461 * MB, memory_limit_bytes: 512 * MB }),
      { text: '461.0 MB / 512.0 MB', percent: 90, high: true },
    );
    assert.deepEqual(
      memoryUsage({ memory_bytes: 256 * MB, memory_limit_bytes: 0 }),
      { text: '256.0 MB / 上限なし', percent: null, high: false },
    );
    assert.strictEqual(
      memoryUsage({ memory_bytes: 256 * MB, memory_limit_bytes: null })?.text,
      '256.0 MB / 上限なし',
    );
    assert.strictEqual(
      memoryUsage({ memory_bytes: null, memory_limit_bytes: 512 * MB }),
      null,
    );
    assert.strictEqual(memoryUsage({}), null, 'an older backend');
  });
});
