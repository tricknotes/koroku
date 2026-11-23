import { module, test } from 'qunit';
import {
  appWideText,
  memoryPatch,
  memoryProblem,
  savedValue,
  serviceValues,
} from 'koroku-console/utils/memory-settings';
import { makeApp } from 'koroku-console/tests/helpers/fakes';

module('Unit | Utility | memory-settings', function () {
  const app = makeApp({
    memory_limit: '',
    service_memory_limits: { worker: '256m', db: '1g' },
    memory_reservation: '0',
    service_memory_reservations: null,
  });

  test('values are read app-wide and per service', function (assert) {
    assert.deepEqual(serviceValues(app, 'limit'), [
      { service: 'db', value: '1g' },
      { service: 'worker', value: '256m' },
    ]);
    assert.deepEqual(serviceValues(app, 'reservation'), []);
    assert.strictEqual(savedValue(app, { kind: 'limit', service: 'db' }), '1g');
    assert.strictEqual(savedValue(app, { kind: 'limit', service: 'web' }), '');
    assert.strictEqual(appWideText(app, 'limit'), '基盤の既定値');
    assert.strictEqual(appWideText(app, 'reservation'), '0（予約しない）');
  });

  test('a patch changes one value and keeps the others', function (assert) {
    assert.deepEqual(memoryPatch(app, { kind: 'limit', service: null }, '1g'), {
      memory_limit: '1g',
    });
    assert.deepEqual(
      memoryPatch(app, { kind: 'reservation', service: null }, null),
      { memory_reservation: '' },
    );
    assert.deepEqual(
      memoryPatch(app, { kind: 'limit', service: 'web' }, '512m'),
      { service_memory_limits: { worker: '256m', db: '1g', web: '512m' } },
    );
    assert.deepEqual(
      memoryPatch(app, { kind: 'limit', service: 'db' }, null),
      { service_memory_limits: { worker: '256m' } },
    );
    assert.deepEqual(
      memoryPatch(app, { kind: 'reservation', service: 'db' }, '0'),
      { service_memory_reservations: { db: '0' } },
    );
  });

  test('values are checked before sending', function (assert) {
    const all = { kind: 'limit' as const, service: null };
    const db = { kind: 'limit' as const, service: 'db' };
    const reserve = { kind: 'reservation' as const, service: 'db' };
    assert.strictEqual(memoryProblem(all, ''), null, 'empty is the default');
    assert.ok(memoryProblem(db, ''), 'a service needs a value');
    assert.ok(memoryProblem(db, '0'), 'a limit of 0 is not a size');
    assert.strictEqual(memoryProblem(reserve, '0'), null, '0 reserves none');
    assert.ok(memoryProblem(reserve, '1.5g'));
    assert.strictEqual(memoryProblem(db, '768m'), null);
  });
});
