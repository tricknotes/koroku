import { module, test } from 'qunit';
import { reservationView } from 'koroku-console/utils/memory-reservation';
import { makeSystem } from 'koroku-console/tests/helpers/fakes';

const GB = 1024 ** 3;
const MB = 1024 ** 2;

module('Unit | Utility | memory-reservation', function () {
  test('nothing to show without a summary', function (assert) {
    assert.strictEqual(reservationView(makeSystem()), null, 'older backend');
    assert.strictEqual(
      reservationView(makeSystem({ memory_reservation: null })),
      null,
      'a member',
    );
  });

  test('the total against the host, with the margin marked', function (assert) {
    const view = reservationView(
      makeSystem({
        memory: { total_bytes: 4 * GB, available_bytes: GB },
        memory_reservation: {
          total_bytes: 2 * GB,
          margin_bytes: GB,
          warning: false,
          apps: [
            {
              slug: 'shop',
              environment: 'production',
              total_bytes: 1.5 * GB,
              services: { web: GB, db: 512 * MB },
            },
            {
              slug: 'docs',
              environment: 'staging',
              total_bytes: 512 * MB,
              services: { web: 512 * MB },
            },
          ],
        },
      }),
    );

    assert.strictEqual(view?.percent, 50);
    assert.strictEqual(view?.mark, 75, 'host minus the margin');
    assert.strictEqual(view?.total, '2.0 GB');
    assert.strictEqual(view?.room, '1.0 GB');
    assert.deepEqual(
      view?.apps.map((a) => [a.slug, a.switched, a.breakdown]),
      [
        ['shop', false, 'db 512.0 MB、web 1.0 GB'],
        ['docs', true, 'web 512.0 MB'],
      ],
    );
  });

  test('over the margin: a warning and no room left', function (assert) {
    const view = reservationView(
      makeSystem({
        memory: { total_bytes: 4 * GB, available_bytes: GB },
        memory_reservation: {
          total_bytes: 3.5 * GB,
          margin_bytes: GB,
          warning: true,
          apps: [],
        },
      }),
    );
    assert.true(view?.warning);
    assert.strictEqual(view?.room, '');
  });
});
