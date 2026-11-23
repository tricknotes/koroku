import { module, test } from 'qunit';
import { click, fillIn, visit } from '@ember/test-helpers';
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
  ServiceStatus,
} from 'koroku-console/utils/types';

const MB = 1024 ** 2;

function svc(
  name: string,
  reserved: number | null,
  next: number | null,
  overrides: Partial<ServiceStatus> = {},
): ServiceStatus {
  return {
    name,
    state: 'running',
    status: 'Up 2 hours',
    health: '',
    memory_bytes: 120 * MB,
    memory_limit_bytes: 512 * MB,
    cpu_percent: 1,
    memory_reservation_bytes: reserved,
    next_memory_reservation_bytes: next,
    ...overrides,
  };
}

function history(p95Mib: number, ready = true): MemoryHistory {
  return {
    range: '24h',
    step_seconds: 300,
    from: '2026-11-20T03:00:00Z',
    to: '2026-11-21T03:00:00Z',
    services: [
      {
        name: 'web',
        limit_bytes: 512 * MB,
        peak_bytes: 300 * MB,
        restarts: 0,
        points: [],
        suggestion: {
          basis_since: '2026-11-07T03:00:00Z',
          basis_hours: 336,
          peak_bytes: 300 * MB,
          p95_bytes: p95Mib * MB,
          ready,
          limit_bytes: 512 * MB,
          limit: '512m',
          flag: null,
        },
      },
    ],
  };
}

const ROW = (name: string) => `[data-test-resource-service="${name}"]`;

// Changing one service's reservation from the resources tab (ARCHITECTURE 9.5).
module('Acceptance | service reservation', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let services: ServiceStatus[];

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      environment: 'production',
      current_deployment: deployment,
      latest_deployment: deployment,
      service_memory_limits: { db: '1g' },
      service_memory_reservations: { worker: '0' },
    });
    services = [
      svc('db', 512 * MB, 512 * MB, { memory_limit_bytes: 1024 * MB }),
      svc('web', 256 * MB, 256 * MB),
      svc('worker', null, 0),
    ];
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on('GET', '/api/apps/blog', () => app)
      .on('GET', '/api/apps/blog/services', () => ({ services }))
      .on('GET', '/api/apps/blog/memory', history(150))
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
        app = { ...app, ...(call.body as Partial<App>) };
        return app;
      });
  });

  test('it shows what each service reserves and where it comes from', async function (assert) {
    await visit('/apps/blog/resources');

    assert
      .dom(`${ROW('web')} [data-test-resource-reservation]`)
      .hasText('256.0 MB');
    assert
      .dom(`${ROW('web')} [data-test-reservation-source]`)
      .hasText('既定（上限の半分）');
    assert
      .dom(`${ROW('worker')} [data-test-resource-reservation]`)
      .hasText('なし');
    assert
      .dom(`${ROW('worker')} [data-test-reservation-source]`)
      .hasText('サービス別の値 0（予約しない）');
    assert.dom('[data-test-reservation-next]').doesNotExist();
    assert.dom('[data-test-reservation-edit="web"]').exists();
  });

  test('it saves one service and keeps the others', async function (assert) {
    await visit('/apps/blog/resources');
    await click('[data-test-reservation-edit="web"]');

    assert.dom('[data-test-dialog]').includesText('web のメモリの予約');
    assert
      .dom('[data-test-reservation-notice]')
      .includesText('次のデプロイから反映されます')
      .includesText('システム画面の予約の合計');
    assert
      .dom('[data-test-dialog-alt]')
      .doesNotExist('nothing of its own to reset');

    // The server reports the next reservation once the settings are saved.
    services = [services[0]!, svc('web', 256 * MB, 192 * MB), services[2]!];
    await fillIn('[data-test-reservation-input]', '192M');
    await click('[data-test-dialog-confirm]');

    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      service_memory_reservations: { worker: '0', web: '192m' },
    });
    assert.dom('[data-test-dialog]').doesNotExist();
    assert
      .dom(`${ROW('web')} [data-test-reservation-next]`)
      .hasText('次のデプロイから 192.0 MB');
    assert
      .dom(`${ROW('web')} [data-test-resource-reservation]`)
      .hasText('256.0 MB', 'the running release keeps its reservation');
    assert
      .dom(`${ROW('web')} [data-test-reservation-source]`)
      .hasText('サービス別の値 192m');
  });

  test('resetting removes only this service', async function (assert) {
    app = {
      ...app,
      service_memory_reservations: { web: '128m', worker: '0' },
    };
    services = [services[0]!, svc('web', 256 * MB, 128 * MB), services[2]!];
    await visit('/apps/blog/resources');

    assert
      .dom(`${ROW('web')} [data-test-reservation-next]`)
      .hasText('次のデプロイから 128.0 MB');
    await click('[data-test-reservation-edit="web"]');
    assert.dom('[data-test-reservation-input]').hasValue('128m');
    assert.dom('[data-test-dialog]').includesText('上限の半分を使います');

    services = [services[0]!, svc('web', 256 * MB, 256 * MB), services[2]!];
    await click('[data-test-dialog-alt]');

    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      service_memory_reservations: { worker: '0' },
    });
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.dom(`${ROW('web')} [data-test-reservation-next]`).doesNotExist();
  });

  test('a bad value is caught before sending', async function (assert) {
    await visit('/apps/blog/resources');
    await click('[data-test-reservation-edit="web"]');

    assert
      .dom('[data-test-dialog-confirm]')
      .isDisabled('an empty value is not a reservation');
    await fillIn('[data-test-reservation-input]', '256 MB');
    await click('[data-test-dialog-confirm]');

    assert
      .dom('[data-test-field-group="reservation"] [data-test-field-error]')
      .includesText('0（予約しない）');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
  });

  test("the API's refusal stays in the dialog", async function (assert) {
    fakes.api.fail('PATCH', '/api/apps/blog', 422, 'validation_failed', {
      service_memory_reservations:
        'the reservation of web must not exceed the memory limit (512m)',
    });
    await visit('/apps/blog/resources');
    await click('[data-test-reservation-edit="web"]');
    await fillIn('[data-test-reservation-input]', '1g');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-dialog]').exists();
    assert
      .dom('[data-test-field-group="reservation"] [data-test-field-error]')
      .includesText('must not exceed the memory limit (512m)');
  });

  test('it offers a suggestion from the memory history', async function (assert) {
    await visit('/apps/blog/resources');
    await click('[data-test-reservation-edit="web"]');

    // The 95th percentile of 150 MiB, rounded up to 64 MiB.
    assert.dom('[data-test-reservation-suggestion]').includesText('目安 192m');
    await click('[data-test-reservation-use-suggestion]');
    assert.dom('[data-test-reservation-input]').hasValue('192m');
    assert.dom('[data-test-reservation-suggestion]').doesNotExist();
  });

  test('no suggestion before a day of data', async function (assert) {
    fakes.api.on('GET', '/api/apps/blog/memory', history(150, false));
    await visit('/apps/blog/resources');
    await click('[data-test-reservation-edit="web"]');

    assert.dom('[data-test-reservation-suggestion]').doesNotExist();
  });

  test('a finished one-shot service reserves nothing and has no action', async function (assert) {
    services = [
      ...services,
      svc('migrate', 256 * MB, 0, {
        state: 'exited',
        status: 'Exited (0) 2 hours ago',
        memory_bytes: null,
        memory_limit_bytes: null,
        cpu_percent: null,
      }),
    ];
    await visit('/apps/blog/resources');

    assert
      .dom(`${ROW('migrate')} [data-test-reservation-next]`)
      .hasText('次のデプロイから なし');
    assert
      .dom(`${ROW('migrate')} [data-test-reservation-source]`)
      .hasText('終わって止まるサービスには付きません');
    assert.dom('[data-test-reservation-edit="migrate"]').doesNotExist();
  });

  test('without a recorded shape, the console estimates the next value', async function (assert) {
    app = { ...app, memory_reservation: '100m' };
    services = [
      svc('web', 256 * MB, null),
      svc('migrate', 256 * MB, null, { state: 'exited', status: 'Exited (0)' }),
    ];
    await visit('/apps/blog/resources');

    assert
      .dom(`${ROW('web')} [data-test-reservation-next]`)
      .hasText('次のデプロイから 100.0 MB');
    assert
      .dom(`${ROW('migrate')} [data-test-reservation-next]`)
      .doesNotExist('an exited service may be one-shot: no estimate');
  });

  test('a staging app has no reservation and no action', async function (assert) {
    app = { ...app, environment: 'staging' };
    services = [svc('web', null, 0), svc('db', null, 0)];
    await visit('/apps/blog/resources');

    assert
      .dom('[data-test-resources-now]')
      .includesText('ステージングのアプリにはメモリの予約は付きません');
    assert.dom('[data-test-resource-reservation-cell]').doesNotExist();
    assert.dom('[data-test-reservation-edit]').doesNotExist();
  });

  test('a staging app still running a reservation says it goes away', async function (assert) {
    app = { ...app, environment: 'staging' };
    services = [svc('web', 256 * MB, 0)];
    await visit('/apps/blog/resources');

    assert
      .dom(`${ROW('web')} [data-test-reservation-next]`)
      .hasText('次のデプロイで外れます');
    assert.dom('[data-test-reservation-edit]').doesNotExist();
  });

  test('a member without manage only reads', async function (assert) {
    app = { ...app, role: 'deploy' };
    await visit('/apps/blog/resources');

    assert
      .dom(`${ROW('web')} [data-test-resource-reservation]`)
      .hasText('256.0 MB');
    assert.dom('[data-test-reservation-edit]').doesNotExist();
  });
});
