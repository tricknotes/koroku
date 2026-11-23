import { module, test } from 'qunit';
import { makeApp } from 'koroku-console/tests/helpers/fakes';
import {
  parseMemory,
  reservationPlan,
  reservationProblem,
  suggestedReservation,
  withReservation,
} from 'koroku-console/utils/service-reservation';
import type {
  MemoryServiceHistory,
  ServiceStatus,
} from 'koroku-console/utils/types';

const MB = 1024 ** 2;

function service(overrides: Partial<ServiceStatus> = {}): ServiceStatus {
  return {
    name: 'web',
    state: 'running',
    status: 'Up',
    health: '',
    memory_limit_bytes: 512 * MB,
    memory_reservation_bytes: 256 * MB,
    next_memory_reservation_bytes: null,
    ...overrides,
  };
}

function history(p95: number, ready = true): MemoryServiceHistory {
  return {
    name: 'web',
    limit_bytes: 512 * MB,
    peak_bytes: p95,
    restarts: 0,
    points: [],
    suggestion: {
      basis_since: '2026-11-07T03:00:00Z',
      basis_hours: 336,
      peak_bytes: p95,
      p95_bytes: p95,
      ready,
      limit_bytes: null,
      limit: null,
      flag: null,
    },
  };
}

module('Unit | Utility | service-reservation', function () {
  test('parseMemory reads sizes like Compose', function (assert) {
    assert.strictEqual(parseMemory('512m'), 512 * MB);
    assert.strictEqual(parseMemory('1G'), 1024 * MB);
    assert.strictEqual(parseMemory('2048'), 2048);
    assert.strictEqual(parseMemory('64mb'), 64 * MB);
    assert.strictEqual(parseMemory(''), null);
    assert.strictEqual(parseMemory('0'), null);
    assert.strictEqual(parseMemory('lots'), null);
  });

  test('the server tells the next reservation when it can', function (assert) {
    const app = makeApp({ environment: 'production' });
    const plan = reservationPlan(
      app,
      service({ next_memory_reservation_bytes: 128 * MB }),
    );
    assert.strictEqual(plan.nextBytes, 128 * MB);
    assert.true(plan.pending);
    assert.strictEqual(plan.source, 'default');
  });

  test('without it, the console estimates by the same precedence', function (assert) {
    const production = makeApp({
      environment: 'production',
      memory_reservation: '100m',
      service_memory_limits: { web: '256m' },
      service_memory_reservations: {},
    });
    assert.strictEqual(
      reservationPlan(production, service()).nextBytes,
      100 * MB,
      'the app-wide value',
    );
    assert.strictEqual(
      reservationPlan(
        { ...production, service_memory_reservations: { web: '1g' } },
        service(),
      ).nextBytes,
      256 * MB,
      'cut to the limit in the settings',
    );
    assert.strictEqual(
      reservationPlan({ ...production, memory_reservation: '' }, service())
        .nextBytes,
      128 * MB,
      'half the limit',
    );
    assert.strictEqual(
      reservationPlan(
        { ...production, memory_reservation: '', service_memory_limits: {} },
        service({ memory_limit_bytes: null }),
      ).nextBytes,
      null,
      'half of an unknown limit is not told',
    );
    assert.strictEqual(
      reservationPlan(makeApp({ environment: 'staging' }), service()).nextBytes,
      0,
      'staging reserves nothing',
    );
    assert.strictEqual(
      reservationPlan(production, service({ state: 'exited' })).nextBytes,
      null,
      'an exited service may be one-shot',
    );
  });

  test('a finished one-shot service is told apart', function (assert) {
    const app = makeApp({ environment: 'production' });
    const plan = reservationPlan(
      app,
      service({ state: 'exited', next_memory_reservation_bytes: 0 }),
    );
    assert.true(plan.oneShot);
    assert.false(
      reservationPlan(
        { ...app, service_memory_reservations: { web: '0' } },
        service({ state: 'exited', next_memory_reservation_bytes: 0 }),
      ).oneShot,
      'reserving nothing was asked for',
    );
  });

  test('the suggestion is the 95th percentile, rounded up and capped', function (assert) {
    assert.strictEqual(suggestedReservation(history(150 * MB), null), '192m');
    assert.strictEqual(suggestedReservation(history(1 * MB), null), '64m');
    assert.strictEqual(
      suggestedReservation(history(500 * MB), 384 * MB),
      '384m',
      'not above the next limit',
    );
    assert.strictEqual(
      suggestedReservation(history(150 * MB, false), null),
      null,
      'not before a day of data',
    );
    assert.strictEqual(suggestedReservation(undefined, null), null);
  });

  test('values are checked like the settings form', function (assert) {
    assert.strictEqual(reservationProblem('256m'), null);
    assert.strictEqual(reservationProblem('0'), null);
    assert.ok(reservationProblem(''));
    assert.ok(reservationProblem('256 MB'));
  });

  test('one entry changes, the others stay', function (assert) {
    const app = makeApp({
      service_memory_reservations: { worker: '0', web: '128m' },
    });
    assert.deepEqual(withReservation(app, 'web', '192m'), {
      worker: '0',
      web: '192m',
    });
    assert.deepEqual(withReservation(app, 'web', null), { worker: '0' });
    assert.deepEqual(app.service_memory_reservations, {
      worker: '0',
      web: '128m',
    });
  });
});
