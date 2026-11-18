import { module, test } from 'qunit';
import { click, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';
import type { App } from 'paas-console/utils/types';

module('Acceptance | overview, logs, backups, system', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
    });
    fakes.api.on('GET', '/api/apps/blog', () => app);
  });

  module('overview', function () {
    test('it shows the state, the deployment and the services', async function (assert) {
      fakes.api.on('GET', '/api/apps/blog/services', {
        services: [
          {
            name: 'web',
            state: 'running',
            status: 'Up 2 hours',
            health: 'healthy',
          },
          { name: 'db', state: 'exited', status: 'Exited (1)', health: '' },
        ],
      });

      await visit('/apps/blog');

      assert.dom('[data-test-app-title]').hasText('blog');
      assert.dom('[data-test-overview-state]').hasText('稼働中');
      assert.dom('[data-test-current-deployment]').includesText('Fix login');
      assert.dom('[data-test-latest-deployment]').doesNotExist();
      assert
        .dom('[data-test-service="web"]')
        .includesText('running')
        .includesText('Up 2 hours')
        .includesText('healthy');
      assert.dom('[data-test-service="db"]').includesText('exited');
    });

    test('stop, start and restart call their endpoints', async function (assert) {
      fakes.api
        .on('GET', '/api/apps/blog/services', { services: [] })
        .on('POST', '/api/apps/blog/stop', () => {
          app = { ...app, desired_state: 'stopped' };
          return app;
        })
        .on('POST', '/api/apps/blog/start', () => {
          app = { ...app, desired_state: 'running' };
          return app;
        })
        .on('POST', '/api/apps/blog/restart', () => app);

      await visit('/apps/blog');
      await click('[data-test-control="stop"]');

      assert.dom('[data-test-dialog]').doesNotExist('staging stops directly');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/stop').length,
        1,
      );
      assert.dom('[data-test-overview-state]').hasText('停止');
      assert.dom('[data-test-app-state]').hasText('停止');
      assert.dom('[data-test-notice]').hasText('停止しました。');

      await click('[data-test-control="start"]');
      assert.dom('[data-test-overview-state]').hasText('稼働中');

      await click('[data-test-control="restart"]');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/restart').length,
        1,
      );
    });

    test('stopping a production app asks first', async function (assert) {
      app = { ...app, environment: 'production' };
      fakes.api
        .on('GET', '/api/apps/blog/services', { services: [] })
        .on('POST', '/api/apps/blog/stop', () => app);

      await visit('/apps/blog');
      await click('[data-test-control="stop"]');

      assert.dom('[data-test-dialog]').exists();
      assert.deepEqual(fakes.api.callsTo('POST', '/api/apps/blog/stop'), []);

      await click('[data-test-dialog-confirm]');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/stop').length,
        1,
      );
      assert.dom('[data-test-dialog]').doesNotExist();
    });

    test('controls that are not available yet do not break the page', async function (assert) {
      app = makeApp();
      fakes.api.fail('POST', '/api/apps/blog/start', 409, 'not_deployed');

      await visit('/apps/blog');

      assert.dom('[data-test-overview-state]').hasText('未デプロイ');
      assert
        .dom('[data-test-unavailable="unavailable"]')
        .includesText('この機能はまだ利用できません');

      await click('[data-test-control="start"]');
      assert
        .dom('[data-test-error]')
        .includesText('まだ一度もデプロイされていません');
    });
  });
});
