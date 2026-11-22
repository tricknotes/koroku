import { module, test } from 'qunit';
import { click, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  apiError,
  installFakes,
  makeApp,
  makeDeployment,
  makeSystem,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, SystemInfo } from 'koroku-console/utils/types';

const WEBHOOK_URL = 'https://console.dev.example.com/webhooks/github';

module('Acceptance | automatic deployment on push', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let system: SystemInfo;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
      auto_deploy: false,
      auto_deploy_branch: '',
    });
    system = makeSystem({
      webhook: { url: WEBHOOK_URL, secret_configured: true },
    });
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on('GET', '/api/system', () => system)
      .on('GET', '/api/apps/blog', () => app)
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
        app = { ...app, ...(call.body as Partial<App>) };
        return app;
      });
  });

  test('a staging app turns it on without a dialog and shows the GitHub App setup', async function (assert) {
    await visit('/apps/blog/settings');

    assert.dom('[data-test-field="auto_deploy"]').isNotChecked().isEnabled();
    assert.dom('[data-test-webhook-setup]').doesNotExist();

    await click('[data-test-field="auto_deploy"]');
    assert
      .dom('[data-test-field="auto_deploy_branch"]')
      .hasValue('')
      .hasAttribute('placeholder', 'main');
    assert.dom('[data-test-webhook-url]').hasText(WEBHOOK_URL);
    assert.dom('[data-test-webhook-setup]').includesText('Push');
    assert.dom('[data-test-webhook-secret-missing]').doesNotExist();

    await fillIn('[data-test-field="auto_deploy_branch"]', ' develop ');
    await click('[data-test-settings-save]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      auto_deploy: true,
      auto_deploy_branch: 'develop',
    });
    assert.dom('[data-test-settings-saved]').exists();
  });

  test('a production app asks before deploying pushes', async function (assert) {
    app = { ...app, environment: 'production' };
    await visit('/apps/blog/settings');

    await click('[data-test-field="auto_deploy"]');
    await click('[data-test-settings-save]');
    assert
      .dom('[data-test-auto-deploy-confirm]')
      .includesText('main')
      .includesText('blog');
    await click('[data-test-dialog-cancel]');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

    await click('[data-test-settings-save]');
    await click('[data-test-dialog-confirm]');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      auto_deploy: true,
    });
    assert.dom('[data-test-dialog]').doesNotExist();

    // Following another branch asks again; turning it off does not.
    await fillIn('[data-test-field="auto_deploy_branch"]', 'release');
    await click('[data-test-settings-save]');
    assert.dom('[data-test-auto-deploy-confirm]').includesText('release');
    await click('[data-test-dialog-confirm]');

    await click('[data-test-field="auto_deploy"]');
    await click('[data-test-settings-save]');
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[2]?.body, {
      auto_deploy: false,
    });
  });

  test('it can wait for a workflow, with the App setup it needs', async function (assert) {
    await visit('/apps/blog/settings');

    await click('[data-test-field="auto_deploy"]');
    assert
      .dom('[data-test-field="auto_deploy_workflow"]')
      .hasValue('')
      .hasAttribute('placeholder', '.github/workflows/image.yml');
    assert.dom('[data-test-workflow-setup]').doesNotExist();

    await fillIn(
      '[data-test-field="auto_deploy_workflow"]',
      ' .github/workflows/image.yml ',
    );
    assert
      .dom('[data-test-workflow-setup]')
      .includesText('Actions')
      .includesText('Workflow run');
    await click('[data-test-settings-save]');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      auto_deploy: true,
      auto_deploy_workflow: '.github/workflows/image.yml',
    });
  });

  test('a refused workflow path is shown at the field', async function (assert) {
    app = { ...app, auto_deploy: true };
    fakes.api.on('PATCH', '/api/apps/blog', () => {
      throw apiError(422, 'validation_failed', {
        auto_deploy_workflow:
          'must be a .yml or .yaml file directly in .github/workflows/',
      });
    });
    await visit('/apps/blog/settings');

    await fillIn('[data-test-field="auto_deploy_workflow"]', 'image.yml');
    await click('[data-test-settings-save]');
    assert
      .dom(
        '[data-test-field-group="auto_deploy_workflow"] [data-test-field-error]',
      )
      .includesText('.github/workflows/');
  });

  test('a production app asks again when the workflow changes', async function (assert) {
    app = { ...app, environment: 'production', auto_deploy: true };
    await visit('/apps/blog/settings');

    await fillIn(
      '[data-test-field="auto_deploy_workflow"]',
      '.github/workflows/image.yml',
    );
    await click('[data-test-settings-save]');
    assert
      .dom('[data-test-auto-deploy-confirm]')
      .includesText('.github/workflows/image.yml');
    await click('[data-test-dialog-confirm]');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      auto_deploy_workflow: '.github/workflows/image.yml',
    });
  });

  test('a stale login is asked to log in again', async function (assert) {
    app = { ...app, environment: 'production' };
    fakes.api.on('PATCH', '/api/apps/blog', () => {
      throw apiError(403, 'reauth_required');
    });
    await visit('/apps/blog/settings');

    await click('[data-test-field="auto_deploy"]');
    await click('[data-test-settings-save]');
    await click('[data-test-dialog-confirm]');
    assert.dom('[data-test-reauth-dialog]').exists();
  });

  test('it warns while the webhook secret is not set', async function (assert) {
    app = { ...app, auto_deploy: true };
    system = makeSystem({
      webhook: { url: WEBHOOK_URL, secret_configured: false },
    });
    await visit('/apps/blog/settings');

    assert.dom('[data-test-field="auto_deploy"]').isChecked();
    assert
      .dom('[data-test-webhook-secret-missing]')
      .includesText('webhook_secret');
  });

  test('an app fetched by URL cannot turn it on', async function (assert) {
    app = {
      ...app,
      repo_full_name: '',
      repo_url: 'https://github.com/octo/oss.git',
    };
    await visit('/apps/blog/settings');

    assert.dom('[data-test-field="auto_deploy"]').isDisabled();
    assert.dom('#settings-auto-deploy-hint').includesText('GitHub App');
  });

  test('the deploy history marks deployments started by a push', async function (assert) {
    const pushed = makeDeployment({ id: 43, trigger: 'push', pusher: 'octo' });
    const manual = makeDeployment({ id: 42, trigger: 'manual', pusher: '' });
    app = { ...app, latest_deployment: pushed, current_deployment: pushed };
    fakes.api
      .on('GET', '/api/apps/blog/deployments', {
        deployments: [pushed, manual],
      })
      .on('GET', '/api/repos/octo/blog/branches', { branches: [] })
      .on('GET', '/api/repos/octo/blog/commits', { commits: [] });
    await visit('/apps/blog/deploy');

    assert.dom('[data-test-push-badge]').exists({ count: 1 });
    assert
      .dom('[data-test-push-badge]')
      .hasText('push で自動デプロイ')
      .hasAttribute('title', 'octo の push で自動デプロイ');

    await click('[data-test-show-log]');
    assert
      .dom('[data-test-push-trigger]')
      .hasText('octo の push で自動デプロイ');
  });

  test('the deploy tab shows a push kept until the running command ends', async function (assert) {
    fakes.api
      .on('GET', '/api/apps/blog/deployments', {
        deployments: [app.current_deployment],
        pending_auto_deploy: {
          ref: 'main',
          commit_sha: '5eed5eed1234567890abcdef1234567890abcdef',
          commit_message: 'Show the order history\n\nAlso paginate it.',
          pusher: 'octo',
          workflow: '.github/workflows/image.yml',
          received_at: new Date().toISOString(),
        },
      })
      .on('GET', '/api/repos/octo/blog/branches', { branches: [] })
      .on('GET', '/api/repos/octo/blog/commits', { commits: [] });
    await visit('/apps/blog/deploy');

    assert
      .dom('[data-test-pending-auto-deploy]')
      .includesText('コマンドの実行が終わったら自動デプロイします')
      .includesText('5eed5ee')
      .includesText('Show the order history')
      .includesText('push: octo')
      .includesText('.github/workflows/image.yml');
    assert
      .dom('[data-test-pending-auto-deploy] a')
      .hasAttribute('href', '/apps/blog/tasks');
  });

  test('the deploy tab shows nothing without a kept push', async function (assert) {
    fakes.api
      .on('GET', '/api/apps/blog/deployments', {
        deployments: [app.current_deployment],
        pending_auto_deploy: null,
      })
      .on('GET', '/api/repos/octo/blog/branches', { branches: [] })
      .on('GET', '/api/repos/octo/blog/commits', { commits: [] });
    await visit('/apps/blog/deploy');
    assert.dom('[data-test-pending-auto-deploy]').doesNotExist();
  });
});
