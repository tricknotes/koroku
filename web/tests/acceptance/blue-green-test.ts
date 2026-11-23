import { module, test } from 'qunit';
import { click, fillIn, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  makeSystem,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type {
  App,
  AppRole,
  Deployment,
  ServiceStatus,
} from 'koroku-console/utils/types';

const MB = 1024 ** 2;
const GROUP = '[data-test-blue-green-group]';

/** A step headline `seconds` after 03:00:00, as koroku writes it. */
function head(seconds: number, key: string, title: string): string {
  const time = new Date(Date.UTC(2026, 10, 18, 3, 0, seconds))
    .toISOString()
    .replace('.000', '');
  return `==> ${time} ${key} ${title}`;
}

function stepNames(): string[] {
  return [...document.querySelectorAll('[data-test-step] .step-name')].map(
    (name) => name.textContent?.trim() ?? '',
  );
}

// Blue/green deploys of the public service (ARCHITECTURE 7.2).
module('Acceptance | blue/green deploys', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let deployments: Deployment[];

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const running = makeDeployment({
      id: 42,
      web_color: 'green',
      switched_at: '2026-11-18T03:03:41Z',
    });
    deployments = [running];
    app = makeApp({
      current_deployment: running,
      latest_deployment: running,
      blue_green: false,
      blue_green_drain_seconds: 10,
      alt_host_port: null,
      web_color: null,
      blue_green_problems: [],
    });
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on('GET', '/api/system', makeSystem())
      .on('GET', '/api/apps/blog', () => app)
      .on('GET', '/api/apps/blog/deployments', () => ({ deployments }))
      .on('GET', '/api/repos/octo/blog/branches', {
        branches: [{ name: 'main', sha: running.commit_sha }],
      })
      .on('GET', '/api/repos/octo/blog/commits', { commits: [] })
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
        app = { ...app, ...(call.body as Partial<App>) };
        return app;
      });
  });

  module('the setting', function () {
    test('a staging app turns it on without a dialog and sets the wait', async function (assert) {
      await visit('/apps/blog/deploy');

      assert.dom(`${GROUP} [data-test-field="blue_green"]`).isNotChecked();
      assert
        .dom('[data-test-field="blue_green_drain_seconds"]')
        .doesNotExist('the wait is asked once it is on');
      assert.dom('[data-test-blue-green-ok]').exists();
      assert
        .dom('[data-test-blue-green-guide]')
        .hasAttribute('href', /prepare-app\/references\/requirements\.md/);
      assert.dom('[data-test-blue-green-note="memory"]').includesText('1');
      assert.dom('[data-test-blue-green-note="env"]').includesText('DB');
      assert
        .dom('[data-test-blue-green-note="restart"]')
        .includesText('再起動');
      assert.dom(GROUP).includesText('web-blue').includesText('web-green');

      await click('[data-test-field="blue_green"]');
      assert.dom('[data-test-field="blue_green_drain_seconds"]').hasValue('10');
      await fillIn('[data-test-field="blue_green_drain_seconds"]', ' 30 ');
      await click('[data-test-deploy-settings-save]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        blue_green: true,
        blue_green_drain_seconds: 30,
      });
      assert
        .dom('[data-test-deploy-settings-saved]')
        .includesText('次のデプロイから');
    });

    test('the wait must be a whole number of seconds from 0 to 300', async function (assert) {
      app = { ...app, blue_green: true };
      await visit('/apps/blog/deploy');

      for (const value of ['301', '-1', '1.5', 'ten', '']) {
        await fillIn('[data-test-field="blue_green_drain_seconds"]', value);
        await click('[data-test-deploy-settings-save]');
        assert
          .dom(
            '[data-test-field-group="blue_green_drain_seconds"] [data-test-field-error]',
          )
          .includesText('0〜300', `${JSON.stringify(value)} is refused`);
      }
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

      await fillIn('[data-test-field="blue_green_drain_seconds"]', '0');
      await click('[data-test-deploy-settings-save]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        blue_green_drain_seconds: 0,
      });
    });

    test('the server refusing the wait shows under the field', async function (assert) {
      app = { ...app, blue_green: true };
      fakes.api.fail('PATCH', '/api/apps/blog', 422, 'validation_failed', {
        blue_green_drain_seconds: 'must be between 0 and 300',
      });
      await visit('/apps/blog/deploy');
      await fillIn('[data-test-field="blue_green_drain_seconds"]', '20');
      await click('[data-test-deploy-settings-save]');
      assert
        .dom(
          '[data-test-field-group="blue_green_drain_seconds"] [data-test-field-error]',
        )
        .includesText('must be between 0 and 300');
    });

    test('a production app asks before turning it on, not before turning it off', async function (assert) {
      app = { ...app, environment: 'production' };
      await visit('/apps/blog/deploy');

      await click('[data-test-field="blue_green"]');
      await click('[data-test-deploy-settings-save]');
      assert
        .dom('[data-test-dialog]')
        .includesText('本番アプリで無停止デプロイを有効にしますか？');
      assert
        .dom('[data-test-blue-green-confirm-migration]')
        .includesText('マイグレーション')
        .includesText('古い版のコード');
      assert
        .dom('[data-test-blue-green-confirm-memory]')
        .includesText('メモリ');
      assert.dom('[data-test-reauth-dialog]').doesNotExist();

      await click('[data-test-dialog-cancel]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

      await click('[data-test-deploy-settings-save]');
      await click('[data-test-dialog-confirm]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        blue_green: true,
      });
      assert.dom('[data-test-dialog]').doesNotExist();

      await click('[data-test-field="blue_green"]');
      await click('[data-test-deploy-settings-save]');
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[1]?.body, {
        blue_green: false,
      });
    });

    test('it shows the colour receiving the requests and the ports', async function (assert) {
      app = {
        ...app,
        blue_green: true,
        alt_host_port: 20010,
        web_color: 'green',
      };
      await visit('/apps/blog/deploy');
      assert
        .dom('[data-test-blue-green-live]')
        .includesText('green')
        .includesText('20000')
        .includesText('20010');
    });

    test('it lists what keeps the running release from blue/green deploys', async function (assert) {
      app = {
        ...app,
        blue_green: true,
        blue_green_problems: [
          {
            service: 'web',
            rule: 'blue_green',
            message: 'container_name blog-web would be used by both colours',
            relaxable: false,
          },
          {
            service: 'worker',
            rule: 'blue_green',
            message: 'refers to the public service web (depends_on)',
            relaxable: false,
          },
        ],
      };
      await visit('/apps/blog/deploy');

      assert.dom('[data-test-blue-green-problem]').exists({ count: 2 });
      assert
        .dom('[data-test-blue-green-problems]')
        .includesText('container_name blog-web')
        .includesText('worker')
        .includesText('検証の手順で止まります');
      assert.dom('[data-test-blue-green-ok]').doesNotExist();
    });

    test('it says when the conditions are not known yet', async function (assert) {
      app = { ...app, blue_green_problems: null };
      await visit('/apps/blog/deploy');
      assert
        .dom('[data-test-blue-green-unknown]')
        .includesText('次のデプロイで確かめます');
      assert.dom('[data-test-blue-green-problems]').doesNotExist();
    });

    test('deploying with the change unsaved asks first', async function (assert) {
      await visit('/apps/blog/deploy');
      await click('[data-test-field="blue_green"]');
      await click('[data-test-deploy-submit]');

      assert.dom('[data-test-unsaved-dialog]').exists();
      await click('[data-test-dialog-cancel]');
      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments'),
        [],
      );
      assert
        .dom('[data-test-field="blue_green"]')
        .isChecked('the change stays');
    });

    for (const [role, editable] of [
      ['view', false],
      ['deploy', false],
      ['manage', true],
    ] as [AppRole, boolean][]) {
      test(`the ${role} role ${editable ? 'changes' : 'only reads'} it`, async function (assert) {
        app = { ...app, role, blue_green: true };
        await visit('/apps/blog/deploy');

        if (editable) {
          assert.dom('[data-test-field="blue_green"]').isEnabled();
          assert
            .dom('[data-test-field="blue_green_drain_seconds"]')
            .isEnabled();
          assert.dom('[data-test-deploy-settings-save]').exists();
        } else {
          // The form's fieldset is disabled.
          assert
            .dom('[data-test-field="blue_green"]')
            .matchesSelector(':disabled')
            .isChecked();
          assert
            .dom('[data-test-field="blue_green_drain_seconds"]')
            .matchesSelector(':disabled')
            .hasValue('10');
          assert.dom('[data-test-role-note]').exists();
          assert.dom('[data-test-deploy-settings-save]').doesNotExist();
        }
      });
    }
  });

  module('the history', function () {
    test('rows show the colour and when it took over, and a memory failure in words', async function (assert) {
      deployments = [
        makeDeployment({
          id: 44,
          status: 'failed',
          error:
            'Not enough memory to run two web containers (312 MiB available, 512 MiB needed).',
          containers_replaced: false,
        }),
        makeDeployment({
          id: 43,
          status: 'failed',
          error: 'health check did not pass within 1m0s',
          web_color: 'blue',
        }),
        ...deployments,
        makeDeployment({ id: 41, web_color: 'blue', switched_at: null }),
      ];
      await visit('/apps/blog/deploy');

      assert
        .dom('[data-test-deployment="42"] [data-test-deployment-color]')
        .hasText('12:03:41 に green へ切り替え');
      assert
        .dom('[data-test-deployment="43"] [data-test-deployment-color]')
        .hasText('blue を起動、切り替えなし');
      assert
        .dom('[data-test-deployment="41"] [data-test-deployment-color]')
        .hasText('blue で起動');
      assert
        .dom('[data-test-deployment="44"] [data-test-deployment-color]')
        .doesNotExist();
      assert
        .dom('[data-test-deployment="44"] [data-test-row-error]')
        .hasText(
          'メモリが足りないため無停止で入れ替えられません（空き 312 MiB、必要 512 MiB）',
        )
        .hasAttribute('title', /Not enough memory/);
      assert
        .dom('[data-test-deployment="43"] [data-test-row-error]')
        .hasText('health check did not pass within 1m0s');
    });

    test('the detail of a memory failure says what to do and that nothing changed', async function (assert) {
      deployments = [
        makeDeployment({
          id: 44,
          status: 'failed',
          error:
            'Not enough memory to run two web containers (312 MiB available, 512 MiB needed).',
          containers_replaced: false,
        }),
        ...deployments,
      ];
      await visit('/apps/blog/deploy');
      // The latest failed deployment opens by itself.
      assert.dom('[data-test-deployment="44"] [data-test-log]').exists();
      const stream = fakes.eventSource.last;
      stream.emit(head(1, 'start', 'Starting blue on port 20000'));
      stream.emit(
        'Not enough memory to run two web containers (312 MiB available, 512 MiB needed).',
      );
      stream.emit('');
      stream.emit(
        head(
          2,
          'failed',
          'Deployment failed: Not enough memory to run two web containers (312 MiB available, 512 MiB needed).',
        ),
      );
      stream.end('failed');
      await settled();

      assert
        .dom('[data-test-deployment="44"] [data-test-memory-shortage]')
        .includesText('空き 312 MiB、必要 512 MiB')
        .includesText('無停止デプロイを外して');
      assert
        .dom('[data-test-deployment="44"]')
        .includesText('メモリが足りないため無停止で入れ替えられません');
      assert
        .dom('[data-test-rollback-notice]')
        .hasAttribute('data-rollback', 'untouched');
    });

    test('the detail of a switched deployment shows the new steps in order', async function (assert) {
      await visit('/apps/blog/deploy');
      await click('[data-test-deployment="42"] [data-test-show-log]');

      assert
        .dom(
          '[data-test-deployment="42"] [data-test-log] [data-test-deployment-color]',
        )
        .hasText('12:03:41 に green へ切り替え');

      const stream = fakes.eventSource.last;
      for (const line of [
        head(1, 'fetch', 'Fetching x'),
        head(2, 'prepare', 'Preparing'),
        head(3, 'validate', 'Validating'),
        head(4, 'start', 'Starting green on port 20010'),
        head(9, 'check', 'Checking http://127.0.0.1:20010/'),
        head(10, 'switch', 'Switching traffic from blue to green'),
        'Requests now reach green on port 20010.',
        head(11, 'services', 'Recreating the other services'),
        head(14, 'drain', 'Stopping blue'),
        head(25, 'done', 'Deployment succeeded'),
      ]) {
        stream.emit(line);
      }
      stream.end('succeeded');
      await settled();

      assert.deepEqual(stepNames(), [
        '取得',
        '生成',
        '検証',
        '起動',
        '確認',
        '切り替え',
        'ほかのサービスの作り直し',
        '旧色の停止',
        '完了',
      ]);
    });

    test('a running blue/green deployment waits for the switch, the other services and the drain', async function (assert) {
      app = { ...app, blue_green: true };
      const running = makeDeployment({
        id: 45,
        status: 'running',
        finished_at: null,
      });
      deployments = [running, ...deployments];
      app = { ...app, latest_deployment: running };
      await visit('/apps/blog/deploy');
      // A running deployment opens by itself.
      assert.dom('[data-test-deployment="45"] [data-test-log]').exists();

      const stream = fakes.eventSource.last;
      stream.emit(head(1, 'fetch', 'Fetching x'));
      stream.emit(head(4, 'start', 'Starting green on port 20010'));
      await settled();

      assert.deepEqual(stepNames(), [
        '取得',
        '起動',
        '確認',
        '切り替え',
        'ほかのサービスの作り直し',
        '旧色の停止',
        '完了',
      ]);
    });

    test('a production deploy says the public service is not interrupted', async function (assert) {
      app = { ...app, environment: 'production', blue_green: true };
      await visit('/apps/blog/deploy');
      await click('[data-test-deploy-submit]');
      assert.dom('[data-test-deploy-dialog-blue-green]').includesText('無停止');
    });
  });

  module('the colours of the public service', function (hooks) {
    let services: ServiceStatus[];

    hooks.beforeEach(function () {
      app = {
        ...app,
        blue_green: true,
        alt_host_port: 20010,
        web_color: 'green',
      };
      services = [
        {
          name: 'web',
          state: 'running',
          status: 'Up 2 hours',
          health: 'healthy',
          memory_bytes: 200 * MB,
          memory_limit_bytes: 512 * MB,
          color: 'blue',
        },
        {
          name: 'web',
          state: 'running',
          status: 'Up 30 seconds',
          health: 'healthy',
          memory_bytes: 150 * MB,
          memory_limit_bytes: 512 * MB,
          color: 'green',
        },
        {
          name: 'db',
          state: 'running',
          status: 'Up 2 hours',
          health: 'healthy',
          memory_bytes: 300 * MB,
          memory_limit_bytes: 1024 * MB,
        },
      ];
      fakes.api
        .on('GET', '/api/apps/blog/services', () => ({ services }))
        .on('GET', '/api/apps/blog/domains', {
          default_hostname: 'blog.dev.example.com',
          domains: [],
        });
    });

    test('the overview shows both colours during a deployment, and which one receives the requests', async function (assert) {
      await visit('/apps/blog');

      assert.dom('[data-test-service="web"]').exists({ count: 2 });
      assert.dom('[data-test-service-color="blue"]').exists();
      assert.dom('[data-test-service-color="green"]').exists();
      assert
        .dom('[data-test-service-live]')
        .exists({ count: 1 })
        .hasText('リクエストを受信中');
      assert
        .dom('[data-test-service="web"]:has([data-test-service-live])')
        .includesText('green');
      assert.dom('[data-test-service-standby]').exists({ count: 1 });
      assert
        .dom('[data-test-service="db"] [data-test-service-color]')
        .doesNotExist();
    });

    test('with one colour running, only its name is shown', async function (assert) {
      services = services.filter((s) => s.color !== 'blue');
      await visit('/apps/blog');

      assert.dom('[data-test-service-color="green"]').exists();
      assert.dom('[data-test-service-live]').doesNotExist();
      assert.dom('[data-test-service-standby]').doesNotExist();
    });
  });
});
