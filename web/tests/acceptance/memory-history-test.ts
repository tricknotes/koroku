import { module, test } from 'qunit';
import { click, triggerKeyEvent, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type {
  App,
  MemoryHistory,
  MemoryServiceHistory,
  MemorySuggestion,
} from 'koroku-console/utils/types';

const MIB = 1024 ** 2;
const NOW = Date.parse('2026-11-21T03:00:00Z');

function history(
  services: MemoryServiceHistory[],
  range = '24h',
): MemoryHistory {
  return {
    range: range as MemoryHistory['range'],
    step_seconds: 300,
    from: new Date(NOW - 24 * 3600_000).toISOString(),
    to: new Date(NOW).toISOString(),
    services,
  };
}

function memoryService(
  name: string,
  peakMib: number,
  limitMib: number,
  suggestion: Partial<MemorySuggestion> = {},
): MemoryServiceHistory {
  const points = [0, 1, 2].map((i) => ({
    at: new Date(NOW - (3 - i) * 300_000).toISOString(),
    max_bytes: (peakMib - 2 + i) * MIB,
    avg_bytes: (peakMib - 10) * MIB,
    limit_bytes: limitMib * MIB,
  }));
  return {
    name,
    limit_bytes: limitMib * MIB,
    peak_bytes: peakMib * MIB,
    restarts: 0,
    points,
    suggestion: {
      basis_since: new Date(NOW - 14 * 24 * 3600_000).toISOString(),
      basis_hours: 336,
      peak_bytes: peakMib * MIB,
      p95_bytes: peakMib * MIB,
      ready: true,
      limit_bytes: null,
      limit: null,
      flag: null,
      ...suggestion,
    },
  };
}

const SERVICE = (name: string) => `[data-test-memory-service="${name}"]`;

module('Acceptance | memory history', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
      service_memory_limits: { db: '1g' },
    });
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on('GET', '/api/apps/blog', () => app)
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
        app = { ...app, ...(call.body as Partial<App>) };
        return app;
      });
  });

  test('it flags services and saves a suggestion as the limit', async function (assert) {
    fakes.api.on(
      'GET',
      '/api/apps/blog/memory',
      history([
        memoryService('db', 400, 1024, {
          limit_bytes: 1024 * MIB,
          limit: '1024m',
        }),
        {
          ...memoryService('web', 480, 512, {
            limit_bytes: 768 * MIB,
            limit: '768m',
            flag: 'near_limit',
          }),
          restarts: 2,
        },
        memoryService('worker', 90, 512, {
          limit_bytes: 192 * MIB,
          limit: '192m',
          flag: 'generous',
        }),
      ]),
    );
    await visit('/apps/blog/resources');

    assert.deepEqual(
      fakes.api.callsTo('GET', '/api/apps/blog/memory')[0]?.query,
      {
        range: '24h',
      },
    );
    assert
      .dom(`${SERVICE('web')} [data-test-memory-flag="near_limit"]`)
      .hasText('上限に近い');
    assert
      .dom(`${SERVICE('web')} [data-test-memory-peak]`)
      .includesText('480.0 MB')
      .includesText('上限の 94%');
    assert.dom(`${SERVICE('web')} [data-test-memory-restarts]`).hasText('2 回');
    assert
      .dom(`${SERVICE('worker')} [data-test-memory-flag="generous"]`)
      .hasText('余裕が大きい');
    assert.dom(`${SERVICE('db')} [data-test-memory-flag]`).doesNotExist();
    assert
      .dom(`${SERVICE('db')} [data-test-memory-current]`)
      .exists('the suggestion equals the running limit: nothing to apply');
    assert
      .dom(`${SERVICE('web')} [data-test-memory-suggested]`)
      .hasText('768m');

    // Saving asks first, then keeps the other services' limits.
    await click('[data-test-memory-apply="web"]');
    assert
      .dom('[data-test-dialog]')
      .includesText('web の上限を 768m にしますか？');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
    await click('[data-test-dialog-confirm]');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      service_memory_limits: { db: '1g', web: '768m' },
    });
    assert.dom('[data-test-dialog]').doesNotExist();
    assert
      .dom(`${SERVICE('web')} [data-test-memory-saved]`)
      .includesText('次のデプロイから反映されます');
    assert.dom('[data-test-memory-apply="web"]').doesNotExist();

    // Cancelling sends nothing.
    await click('[data-test-memory-apply="worker"]');
    await click('[data-test-dialog-cancel]');
    assert.strictEqual(fakes.api.callsTo('PATCH', '/api/apps/blog').length, 1);
    assert.dom('[data-test-memory-apply="worker"]').exists();
  });

  test('a failed save stays in the dialog', async function (assert) {
    fakes.api
      .on(
        'GET',
        '/api/apps/blog/memory',
        history([
          memoryService('web', 480, 512, {
            limit_bytes: 768 * MIB,
            limit: '768m',
          }),
        ]),
      )
      .fail('PATCH', '/api/apps/blog', 500, 'internal');
    await visit('/apps/blog/resources');
    await click('[data-test-memory-apply="web"]');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-dialog] [data-test-error]').exists();
    assert.dom('[data-test-memory-saved]').doesNotExist();
  });

  test('a viewer sees the suggestion but cannot save it', async function (assert) {
    app = { ...app, role: 'view' };
    fakes.api.on(
      'GET',
      '/api/apps/blog/memory',
      history([
        memoryService('web', 480, 512, {
          limit_bytes: 768 * MIB,
          limit: '768m',
        }),
      ]),
    );
    await visit('/apps/blog/resources');

    assert
      .dom(`${SERVICE('web')} [data-test-memory-suggested]`)
      .hasText('768m');
    assert.dom('[data-test-memory-apply]').doesNotExist();
  });

  test('the chart reads out a point from the keyboard', async function (assert) {
    fakes.api.on(
      'GET',
      '/api/apps/blog/memory',
      history([memoryService('web', 300, 512)]),
    );
    await visit('/apps/blog/resources');

    const plot = `${SERVICE('web')} [data-test-memory-chart-plot]`;
    assert
      .dom(plot)
      .hasAttribute(
        'aria-label',
        /web のメモリ使用量。期間内のピーク 300.0 MB、上限 512.0 MB/,
      );
    await triggerKeyEvent(plot, 'keydown', 'End');
    assert
      .dom(`${SERVICE('web')} [data-test-memory-readout]`)
      .includesText('300.0 MB')
      .includesText('59%');
    await triggerKeyEvent(plot, 'keydown', 'ArrowLeft');
    assert
      .dom(`${SERVICE('web')} [data-test-memory-readout]`)
      .includesText('299.0 MB');
    await triggerKeyEvent(plot, 'keydown', 'Escape');
    assert.dom(`${SERVICE('web')} [data-test-memory-readout]`).hasNoText();
  });

  test('switching the range asks again', async function (assert) {
    fakes.api.on('GET', '/api/apps/blog/memory', (call: FakeCall) =>
      history([memoryService('web', 300, 512)], call.query['range']),
    );
    await visit('/apps/blog/resources');
    await click('[data-test-memory-range-option="7d"]');

    assert.deepEqual(
      fakes.api
        .callsTo('GET', '/api/apps/blog/memory')
        .map((c) => c.query['range']),
      ['24h', '7d'],
    );
    assert.dom('[data-test-memory-range-option="7d"]').isChecked();
  });

  test('while collecting, it says how long until a suggestion', async function (assert) {
    fakes.api.on(
      'GET',
      '/api/apps/blog/memory',
      history([
        memoryService('web', 140, 512, { ready: false, basis_hours: 3 }),
      ]),
    );
    await visit('/apps/blog/resources');

    assert
      .dom(`${SERVICE('web')} [data-test-memory-collecting]`)
      .includesText('記録中')
      .includesText('3 時間分を記録しました。目安はあと 21 時間で出ます。');
    assert.dom('[data-test-memory-apply]').doesNotExist();
  });

  test('without data, one next step', async function (assert) {
    fakes.api.on('GET', '/api/apps/blog/memory', history([]));
    await visit('/apps/blog/resources');

    assert.dom('[data-test-memory-empty]').includesText('まだ記録がありません');
    assert.dom('[data-test-memory-empty] a').hasText('デプロイを開く');
  });

  test('a failure keeps the current usage on screen', async function (assert) {
    fakes.api
      .fail('GET', '/api/apps/blog/memory', 500, 'internal')
      .on('GET', '/api/apps/blog/services', {
        services: [
          {
            name: 'web',
            state: 'running',
            status: 'Up 2 hours',
            health: '',
            memory_bytes: 300 * MIB,
            memory_limit_bytes: 512 * MIB,
            cpu_percent: 6.04,
          },
        ],
      });
    await visit('/apps/blog/resources');

    assert
      .dom('[data-test-memory-history] [data-test-error]')
      .includesText('メモリの履歴を読み込めませんでした');
    assert.dom('[data-test-resource-service="web"]').exists();
  });

  test('the current usage shows CPU and memory per service', async function (assert) {
    fakes.api
      .on('GET', '/api/apps/blog/memory', history([]))
      .on('GET', '/api/apps/blog/services', {
        services: [
          {
            name: 'web',
            state: 'running',
            status: 'Up 2 hours',
            health: '',
            memory_bytes: 480 * MIB,
            memory_limit_bytes: 512 * MIB,
            cpu_percent: 6.04,
          },
          {
            name: 'job',
            state: 'exited',
            status: 'Exited (0)',
            health: '',
            memory_bytes: null,
            memory_limit_bytes: null,
            cpu_percent: null,
          },
        ],
      });
    await visit('/apps/blog/resources');

    const web = '[data-test-resource-service="web"]';
    assert.dom(`${web} [data-test-resource-cpu]`).includesText('6%');
    assert
      .dom(`${web} [data-test-resource-memory]`)
      .includesText('480.0 MB / 512.0 MB');
    assert.dom(`${web} [data-test-resource-memory]`).hasClass('is-high');
    assert
      .dom('[data-test-resource-service="job"] [data-test-resource-cpu]')
      .includesText('—');
  });

  test('the settings point to the resources tab', async function (assert) {
    fakes.api.on('GET', '/api/apps/blog/memory', history([]));
    await visit('/apps/blog/settings');

    assert.dom('[data-test-memory-history]').doesNotExist();
    await click('[data-test-settings-resources-link]');
    assert.dom('[data-test-memory-history]').exists();
  });
});
