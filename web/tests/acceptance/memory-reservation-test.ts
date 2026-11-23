import { module, test } from 'qunit';
import { click, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeBackups,
  makeDeployment,
  makeSystem,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, SystemInfo } from 'koroku-console/utils/types';

const GB = 1024 ** 3;
const MB = 1024 ** 2;

// Production memory protection (ARCHITECTURE 6.2).
module('Acceptance | memory reservation', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
  });

  module('system screen', function (hooks) {
    let system: SystemInfo;

    hooks.beforeEach(function () {
      system = makeSystem({
        memory: { total_bytes: 4 * GB, available_bytes: 2 * GB },
        memory_reservation: {
          total_bytes: 1.5 * GB,
          margin_bytes: GB,
          warning: false,
          apps: [
            {
              slug: 'shop',
              environment: 'production',
              total_bytes: GB,
              services: { web: 256 * MB, db: 768 * MB },
            },
            {
              slug: 'docs',
              environment: 'staging',
              total_bytes: 512 * MB,
              services: { web: 512 * MB },
            },
          ],
        },
      });
      fakes.api
        .on('GET', '/api/system', () => system)
        .on('GET', '/api/system/docker', { items: [] })
        .on('GET', '/api/system/containers', {
          cpu_count: 2,
          memory_total_bytes: 4 * GB,
          groups: [],
        })
        .on('GET', '/api/backups', makeBackups());
    });

    test('it totals the reservations against host memory', async function (assert) {
      await visit('/system');

      assert
        .dom('[data-test-reservation-total]')
        .includesText('予約の合計')
        .includesText('1.5 GB / 4.0 GB')
        .includesText('あと 1.5 GB まで');
      assert
        .dom('[data-test-reservation-total] [role="meter"]')
        .hasAttribute('aria-valuenow', '37.5');
      assert
        .dom('[data-test-reservation-app="shop"]')
        .includesText('db 768.0 MB、web 256.0 MB')
        .includesText('1.0 GB');
      assert
        .dom(
          '[data-test-reservation-app="docs"] [data-test-reservation-switched]',
        )
        .includesText('次のデプロイで外れます');
      assert.dom('[data-test-reservation-warning]').doesNotExist();
      assert.dom('[data-test-reservation-flag]').doesNotExist();
    });

    test('an app link covers its own row, not the page', async function (assert) {
      await visit('/system');

      // The link is stretched over its row with a positioned ::after. Without
      // a positioned row it covered the whole page, header included, so the
      // header's アプリ opened this app's settings.
      const nav = document.querySelector('[data-test-nav="apps"]')!;
      const box = nav.getBoundingClientRect();
      const hit = document.elementFromPoint(
        box.left + box.width / 2,
        box.top + box.height / 2,
      );
      assert.ok(nav.contains(hit), 'the header link is on top at its place');

      const row = document.querySelector('[data-test-reservation-app="shop"]')!;
      assert.strictEqual(
        getComputedStyle(row).position,
        'relative',
        'the row contains the stretched link',
      );
    });

    test('reservations that leave no margin warn', async function (assert) {
      system = makeSystem({
        memory: { total_bytes: 4 * GB, available_bytes: GB },
        memory_reservation: {
          total_bytes: 3.5 * GB,
          margin_bytes: GB,
          warning: true,
          apps: [],
        },
      });

      await visit('/system');

      assert
        .dom('[data-test-reservation-warning]')
        .includesText('予約がホストのメモリに対して多すぎます')
        .includesText('3.5 GB')
        .includesText('1.0 GB');
      assert.dom('[data-test-reservation-flag]').hasText('余白不足');
    });

    test('a member gets no summary, and the section stays away', async function (assert) {
      system = makeSystem({ memory_reservation: null });

      await visit('/system');

      assert.dom('[data-test-resource="memory"]').exists();
      assert.dom('[data-test-reservation]').doesNotExist();
    });
  });

  module('settings', function (hooks) {
    let app: App;

    hooks.beforeEach(function () {
      const deployment = makeDeployment();
      app = makeApp({
        environment: 'production',
        current_deployment: deployment,
        latest_deployment: deployment,
        service_memory_reservations: { worker: '0' },
      });
      fakes.api
        .on('GET', '/api/apps', { apps: [] })
        .on('GET', '/api/apps/blog', () => app)
        .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
          app = { ...app, ...(call.body as Partial<App>) };
          return app;
        });
    });

    test('a production app saves its reservations', async function (assert) {
      await visit('/apps/blog/settings');

      assert.dom('[data-test-reservation-staging-note]').doesNotExist();
      assert.dom('[data-test-reservation-service]').hasValue('worker');
      assert.dom('[data-test-reservation-value]').hasValue('0');

      await fillIn('[data-test-field="memory_reservation"]', '192m');
      await click('[data-test-reservation-add]');
      const rows = document.querySelectorAll('[data-test-reservation-row]');
      const last = rows[rows.length - 1]!;
      await fillIn(
        last.querySelector('[data-test-reservation-service]')!,
        'db',
      );
      await fillIn(
        last.querySelector('[data-test-reservation-value]')!,
        '512m',
      );
      await click('[data-test-settings-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        memory_reservation: '192m',
        service_memory_reservations: { worker: '0', db: '512m' },
      });
      assert.dom('[data-test-settings-saved]').includesText('次のデプロイから');
    });

    test('a bad value is caught before sending', async function (assert) {
      await visit('/apps/blog/settings');

      await fillIn('[data-test-field="memory_reservation"]', 'たくさん');
      await click('[data-test-settings-save]');

      assert
        .dom(
          '[data-test-field-group="memory_reservation"] [data-test-field-error]',
        )
        .includesText('0（予約しない）');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
    });

    test('a staging app is told it reserves nothing', async function (assert) {
      app = { ...app, environment: 'staging' };

      await visit('/apps/blog/settings');

      assert
        .dom('[data-test-reservation-staging-note]')
        .includesText('ステージングのアプリには付きません');
    });
  });

  test('the resources tab shows what each service reserves', async function (assert) {
    const deployment = makeDeployment();
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on(
        'GET',
        '/api/apps/blog',
        makeApp({
          environment: 'production',
          current_deployment: deployment,
          latest_deployment: deployment,
        }),
      )
      .on('GET', '/api/apps/blog/services', {
        services: [
          {
            name: 'web',
            state: 'running',
            status: 'Up',
            health: '',
            memory_bytes: 200 * MB,
            memory_limit_bytes: 512 * MB,
            cpu_percent: 1,
            memory_reservation_bytes: 256 * MB,
          },
          {
            name: 'worker',
            state: 'running',
            status: 'Up',
            health: '',
            memory_bytes: 100 * MB,
            memory_limit_bytes: 512 * MB,
            cpu_percent: 1,
            memory_reservation_bytes: null,
          },
        ],
      })
      .on('GET', '/api/apps/blog/memory', { range: '24h', services: [] });

    await visit('/apps/blog/resources');

    assert
      .dom(
        '[data-test-resource-service="web"] [data-test-resource-reservation]',
      )
      .hasText('予約 256.0 MB');
    assert
      .dom(
        '[data-test-resource-service="worker"] [data-test-resource-reservation]',
      )
      .doesNotExist();
  });
});
