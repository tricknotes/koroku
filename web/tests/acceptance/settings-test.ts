import { module, test } from 'qunit';
import {
  click,
  currentURL,
  fillIn,
  find,
  findAll,
  triggerEvent,
  visit,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  apiError,
  installFakes,
  makeApp,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App } from 'koroku-console/utils/types';

function fieldError(name: string): string {
  return `[data-test-field-group="${name}"] [data-test-field-error]`;
}

module('Acceptance | settings', function (hooks) {
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
      backup_exclude_volumes: ['pgdata'],
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -U postgres app',
      backup_before_deploy: false,
    });
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on('GET', '/api/apps/blog', () => app)
      .on('PATCH', '/api/apps/blog', (call: FakeCall) => {
        app = { ...app, ...(call.body as Partial<App>) };
        return app;
      })
      .on('DELETE', '/api/apps/blog', undefined);
  });

  test('it shows what the app is deployed from and how', async function (assert) {
    await visit('/apps/blog/settings');

    assert
      .dom('[data-test-repository] a')
      .hasText('octo/blog')
      .hasAttribute('href', 'https://github.com/octo/blog');
    assert.dom('[data-test-field="default_branch"]').hasValue('main');
    assert.dom('[data-test-field="compose_path"]').hasValue('');
    assert.dom('[data-test-field="web_service"]').hasValue('web');
    assert.dom('[data-test-field="web_port"]').hasValue('3000');
    assert.dom('[data-test-host-port]').hasText('127.0.0.1:20000');
    assert.dom('[data-test-field="healthcheck_path"]').hasValue('/');
    assert.dom('[data-test-environment-option="staging"]').isChecked();
    assert
      .dom('[data-test-memory-summary-limit]')
      .hasText('基盤の既定値（全サービス共通）、db 1g');
    assert
      .dom('[data-test-field="dump_hook_service"]')
      .doesNotExist('the backup settings live on the backups tab');
    for (const field of [
      'auto_deploy',
      'relax_policy',
      'promote_from',
      'idle_sleep',
    ]) {
      assert
        .dom(`[data-test-field="${field}"]`)
        .doesNotExist(`${field} lives on the tab it acts on`);
    }
    assert.dom('[data-test-env-source-option]').doesNotExist();
    assert.dom('[data-test-control]').doesNotExist('nothing to start here');
  });

  test('only the changed fields are sent', async function (assert) {
    await visit('/apps/blog/settings');

    await click('[data-test-settings-save]');
    assert.dom('[data-test-settings-unchanged]').exists();
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

    await fillIn('[data-test-field="healthcheck_path"]', '/up');
    await fillIn('[data-test-field="default_branch"]', 'trunk');
    await click('[data-test-environment-option="production"]');
    await click('[data-test-settings-save]');

    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      default_branch: 'trunk',
      healthcheck_path: '/up',
      environment: 'production',
    });
    assert.dom('[data-test-settings-saved]').exists();
    assert
      .dom('[data-test-app-environment]')
      .hasText('本番', 'the header follows the saved app');

    await fillIn('[data-test-field="web_service"]', 'app');
    assert
      .dom('[data-test-settings-saved]')
      .doesNotExist('a new change clears the note');
    // Enter in a field of another part of the page saves everything too.
    await triggerEvent(
      find('[data-test-field="default_branch"]')!.closest('form')!,
      'submit',
    );

    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[1]?.body, {
      web_service: 'app',
    });
  });

  module('unsaved changes and deploying', function (hooks) {
    hooks.beforeEach(function () {
      fakes.api
        .on('GET', '/api/apps/blog/deployments', { deployments: [] })
        .on('GET', '/api/repos/octo/blog/branches', { branches: [] })
        .on('GET', '/api/repos/octo/blog/commits', { commits: [] });
    });

    test('without changes the deploy button goes straight on', async function (assert) {
      await visit('/apps/blog/settings');
      await click('[data-test-header-deploy]');
      assert.strictEqual(currentURL(), '/apps/blog/deploy');
      assert.dom('[data-test-unsaved-dialog]').doesNotExist();
    });

    test('unsaved changes are saved first when asked', async function (assert) {
      await visit('/apps/blog/settings');
      await fillIn('[data-test-field="healthcheck_path"]', '/up');
      await click('[data-test-header-deploy]');

      assert.dom('[data-test-unsaved-dialog]').exists();
      assert.strictEqual(currentURL(), '/apps/blog/settings', 'held back');

      await click('[data-test-dialog-cancel]');
      assert.dom('[data-test-unsaved-dialog]').doesNotExist();
      assert.strictEqual(currentURL(), '/apps/blog/settings');
      assert
        .dom('[data-test-field="healthcheck_path"]')
        .hasValue('/up', 'cancelling keeps the edit');

      await click('[data-test-tab="deploy"]');
      assert.dom('[data-test-unsaved-dialog]').exists('the tab asks too');
      await click('[data-test-dialog-confirm]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        healthcheck_path: '/up',
      });
      assert.strictEqual(currentURL(), '/apps/blog/deploy');
    });

    test('unsaved changes can be dropped', async function (assert) {
      await visit('/apps/blog/settings');
      await fillIn('[data-test-field="healthcheck_path"]', '/up');
      await click('[data-test-header-deploy]');
      await click('[data-test-dialog-alt]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
      assert.strictEqual(currentURL(), '/apps/blog/deploy');
    });

    test('a save that fails keeps the user on the settings', async function (assert) {
      fakes.api.fail('PATCH', '/api/apps/blog', 422, 'validation_failed', {
        healthcheck_path: 'must start with /',
      });
      await visit('/apps/blog/settings');
      await fillIn('[data-test-field="healthcheck_path"]', '/up');
      await click('[data-test-header-deploy]');
      await click('[data-test-dialog-confirm]');

      assert.strictEqual(currentURL(), '/apps/blog/settings');
      assert.dom(fieldError('healthcheck_path')).exists();
    });

    test('the resources and environment tabs guard their settings too', async function (assert) {
      fakes.api.on('GET', '/api/system', { idle_sleep: { available: true } });
      await visit('/apps/blog/resources');
      await click('[data-test-field="idle_sleep"]');
      await click('[data-test-header-deploy]');
      assert.dom('[data-test-unsaved-dialog]').includesText('休止の設定');
      await click('[data-test-dialog-confirm]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        idle_sleep: true,
      });
      assert.strictEqual(currentURL(), '/apps/blog/deploy');

      await visit('/apps/blog/env');
      await click('[data-test-env-source-option="off"]');
      await click('[data-test-tab="deploy"]');
      assert.dom('[data-test-unsaved-dialog]').includesText('照合元');
      await click('[data-test-dialog-alt]');
      assert.strictEqual(currentURL(), '/apps/blog/deploy');
      assert.strictEqual(
        fakes.api.callsTo('PATCH', '/api/apps/blog').length,
        1,
        'dropped without saving',
      );
    });

    test('deploying with unsaved deploy settings asks first', async function (assert) {
      fakes.api.on(
        'POST',
        '/api/apps/blog/deployments',
        makeDeployment({ id: 50, status: 'queued' }),
      );
      app = { ...app, role: 'owner' };
      await visit('/apps/blog/deploy');
      await click('[data-test-field="relax_policy"]');
      await click('[data-test-deploy-submit]');

      assert.dom('[data-test-unsaved-dialog]').exists();
      assert.dom('[data-test-dialog-confirm]').hasText('保存してデプロイ');
      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments'),
        [],
        'held back',
      );

      await click('[data-test-dialog-alt]');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments').length,
        1,
        'deployed with the saved settings',
      );
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
      assert
        .dom('[data-test-field="relax_policy"]')
        .isChecked('the edit stays in the form');

      await click('[data-test-deploy-submit]');
      await click('[data-test-dialog-confirm]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        relax_policy: true,
      });
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments').length,
        2,
        'saved, then deployed',
      );
    });

    test('relaxing the policy checks comes with a warning', async function (assert) {
      await visit('/apps/blog/deploy');

      assert.dom('[data-test-relax-warning]').doesNotExist();
      assert.dom('[data-test-auto-rollback]').includesText('有効');

      await click('[data-test-field="relax_policy"]');

      assert
        .dom('[data-test-relax-warning]')
        .includesText('保護の多くが効きません')
        .includesText('root');

      await click('[data-test-field="relax_policy"]');
      assert.dom('[data-test-relax-warning]').doesNotExist();
    });

    test('an app that already runs relaxed shows the warning at once', async function (assert) {
      app = { ...app, relax_policy: true };

      await visit('/apps/blog/deploy');

      assert.dom('[data-test-field="relax_policy"]').isChecked();
      assert.dom('[data-test-relax-warning]').exists();
    });
  });

  test('a production app that deploys pushes asks before its branch changes', async function (assert) {
    app = { ...app, environment: 'production', auto_deploy: true };
    await visit('/apps/blog/settings');

    await fillIn('[data-test-field="default_branch"]', 'release');
    await click('[data-test-settings-save]');
    assert
      .dom('[data-test-auto-deploy-confirm]')
      .includesText('release', 'auto-deploy follows the default branch');
    await click('[data-test-dialog-confirm]');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      default_branch: 'release',
    });
  });

  test('turning a staging app that sleeps into production asks first', async function (assert) {
    app = { ...app, idle_sleep: true, idle_sleep_minutes: 60 };
    await visit('/apps/blog/settings');

    await click('[data-test-environment-option="production"]');
    await click('[data-test-settings-save]');
    assert.dom('[data-test-idle-sleep-confirm]').includesText('1 時間');
    await click('[data-test-dialog-cancel]');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
  });

  test('validation errors are shown next to the fields', async function (assert) {
    await visit('/apps/blog/settings');

    await fillIn('[data-test-field="web_port"]', '0');
    await click('[data-test-settings-save]');
    assert.dom(fieldError('web_port')).exists();
    assert
      .dom('[data-test-field="web_port"]')
      .hasAttribute('aria-invalid', 'true');
    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

    fakes.api.on('PATCH', '/api/apps/blog', () => {
      throw apiError(422, 'validation_failed', {
        compose_path: 'must be a relative path',
        mystery: 'is not understood',
      });
    });
    await fillIn('[data-test-field="web_port"]', '3000');
    await fillIn('[data-test-field="compose_path"]', '/etc/compose.yaml');
    await click('[data-test-settings-save]');

    assert.dom(fieldError('compose_path')).hasText('must be a relative path');
    assert
      .dom('[data-test-other-errors]')
      .hasText('mystery: is not understood');
    assert.dom('[data-test-settings-saved]').doesNotExist();
  });

  module('access protection', function () {
    test('a protected app edits the paths that skip the protection', async function (assert) {
      app = {
        ...app,
        access_protection: true,
        protection_exempt_paths: ['/webhooks/'],
      };
      await visit('/apps/blog/settings');

      assert.dom('[data-test-field="access_protection"]').isChecked();
      assert.dom('[data-test-app-protected]').hasText('保護');
      assert.dom('[data-test-exempt-path]').hasValue('/webhooks/');
      assert.dom('[data-test-protection-public]').doesNotExist();

      await click('[data-test-exempt-add]');
      await fillIn(
        '[data-test-exempt-row]:nth-child(2) [data-test-exempt-path]',
        ' /api/hook ',
      );
      await click('[data-test-settings-save]');

      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        protection_exempt_paths: ['/webhooks/', '/api/hook'],
      });
      assert.dom('[data-test-settings-saved]').exists();

      await click(
        '[data-test-exempt-row]:nth-child(1) [data-test-exempt-remove]',
      );
      await click('[data-test-settings-save]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[1]?.body, {
        protection_exempt_paths: ['/api/hook'],
      });
    });

    test('paths that would open too much are refused before saving', async function (assert) {
      app = { ...app, access_protection: true, protection_exempt_paths: [] };
      await visit('/apps/blog/settings');

      for (const [path, message] of [
        ['/', '保護そのものを無効に'],
        ['webhooks', '/ で始まる'],
        ['/hooks/*', '* や %'],
        ['/.koroku/auth/', '/.koroku'],
        ['/a/../b', '..'],
      ]) {
        if (!document.querySelector('[data-test-exempt-row]')) {
          await click('[data-test-exempt-add]');
        }
        await fillIn('[data-test-exempt-path]', path!);
        await click('[data-test-settings-save]');
        assert
          .dom(fieldError('protection_exempt_paths'))
          .includesText(message!, path);
        assert
          .dom('[data-test-exempt-path]')
          .hasAttribute('aria-invalid', 'true');
      }
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);
    });

    test('turning it off on a staging app saves at once and explains noindex', async function (assert) {
      app = { ...app, access_protection: true };
      await visit('/apps/blog/settings');

      await click('[data-test-field="access_protection"]');
      assert
        .dom('[data-test-field-group="protection_exempt_paths"]')
        .doesNotExist();
      assert.dom('[data-test-protection-public]').includesText('noindex');
      await click('[data-test-settings-save]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        access_protection: false,
      });
      assert.dom('[data-test-app-protected]').doesNotExist();
    });

    test('a production app asks before the protection changes', async function (assert) {
      app = { ...app, environment: 'production', access_protection: false };
      await visit('/apps/blog/settings');

      await click('[data-test-field="access_protection"]');
      await click('[data-test-settings-save]');
      assert.dom('[data-test-protection-confirm="on"]').includesText('blog');
      await click('[data-test-dialog-cancel]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog'), []);

      await click('[data-test-settings-save]');
      await click('[data-test-dialog-confirm]');
      assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
        access_protection: true,
      });
      assert.dom('[data-test-dialog]').doesNotExist();

      await click('[data-test-field="access_protection"]');
      await click('[data-test-settings-save]');
      assert
        .dom('[data-test-protection-confirm="off"]')
        .includesText('誰でも開ける');
      assert.dom('[data-test-dialog-confirm]').hasClass('button-danger');
    });
  });

  module('delete', function () {
    test('deleting requires typing the app name', async function (assert) {
      await visit('/apps/blog/settings');
      await click('[data-test-delete]');

      assert.dom('[data-test-dialog]').includesText('blog');
      assert.dom('[data-test-dialog-confirm]').isDisabled();
      assert.dom('[data-test-delete-volumes]').isNotChecked();

      await fillIn('[data-test-dialog-input]', 'blo');
      assert.dom('[data-test-dialog-confirm]').isDisabled();

      await fillIn('[data-test-dialog-input]', 'blog');
      assert.dom('[data-test-dialog-confirm]').isNotDisabled();
      assert.deepEqual(fakes.api.callsTo('DELETE', '/api/apps/blog'), []);

      await click('[data-test-dialog-confirm]');

      const calls = fakes.api.callsTo('DELETE', '/api/apps/blog');
      assert.strictEqual(calls.length, 1);
      assert.deepEqual(calls[0]?.query, {}, 'volumes are kept by default');
      assert.strictEqual(currentURL(), '/');
    });

    test('deleting with volumes is chosen in the confirmation', async function (assert) {
      app = { ...app, environment: 'production' };

      await visit('/apps/blog/settings');
      await click('[data-test-delete]');

      assert.dom('[data-test-dialog]').includesText('本番アプリ');
      assert.dom('[data-test-delete-volumes-warning]').doesNotExist();

      await click('[data-test-delete-volumes]');
      assert.dom('[data-test-delete-volumes-warning]').exists();

      await fillIn('[data-test-dialog-input]', 'blog');
      await click('[data-test-dialog-confirm]');

      assert.deepEqual(
        fakes.api.callsTo('DELETE', '/api/apps/blog')[0]?.query,
        { volumes: 'true' },
      );
      assert.strictEqual(currentURL(), '/');
    });

    test('a canceled or failed delete leaves the app alone', async function (assert) {
      await visit('/apps/blog/settings');
      await click('[data-test-delete]');
      await click('[data-test-delete-volumes]');
      await click('[data-test-dialog-cancel]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(fakes.api.callsTo('DELETE', '/api/apps/blog'), []);

      fakes.api.fail('DELETE', '/api/apps/blog', 409, 'busy');
      await click('[data-test-delete]');
      assert
        .dom('[data-test-delete-volumes]')
        .isNotChecked('the choice starts over each time');
      await fillIn('[data-test-dialog-input]', 'blog');
      await click('[data-test-dialog-confirm]');

      assert.strictEqual(currentURL(), '/apps/blog/settings');
      assert
        .dom('[data-test-danger-zone] [data-test-error]')
        .includesText('ほかの処理が実行中です');
    });
  });

  module('sections', function (hooks) {
    hooks.beforeEach(function () {
      fakes.api.on('GET', '/api/apps/blog/domains', {
        default_hostname: 'blog.dev.example.com',
        domains: [],
      });
    });

    test('the index leads to each section and marks it', async function (assert) {
      await visit('/apps/blog/settings');

      const links = findAll('[data-test-settings-index-link]').map((link) =>
        link.getAttribute('data-test-settings-index-link'),
      );
      assert.deepEqual(links, [
        'source',
        'publish',
        'environment',
        'protection',
        'memory',
        'registry',
        'danger',
      ]);
      const sections = findAll('[data-settings-section]').map((section) =>
        section.getAttribute('data-settings-section'),
      );
      assert.deepEqual(sections, links, 'the index follows the page');
      assert
        .dom('[data-test-settings-index-link="source"]')
        .hasAttribute('aria-current', 'location')
        .hasAttribute('href', '/apps/blog/settings?section=source');

      await click('[data-test-settings-index-link="environment"]');
      assert.strictEqual(
        currentURL(),
        '/apps/blog/settings?section=environment',
      );
      assert.dom('#settings-environment').isFocused();
      assert
        .dom('[data-test-settings-index-link="environment"]')
        .hasAttribute('aria-current', 'location');
      assert.dom('[data-test-settings-jump-select]').hasValue('environment');
    });

    test('the jump control does the same on a narrow screen', async function (assert) {
      await visit('/apps/blog/settings');

      await fillIn('[data-test-settings-jump-select]', 'registry');
      assert.strictEqual(currentURL(), '/apps/blog/settings?section=registry');
      assert.dom('#settings-registry').isFocused();

      // Which of the two shows depends on the width of the window.
      const narrow = window.matchMedia('(width < 64rem)').matches;
      assert
        .dom('[data-test-settings-index]')
        .hasStyle({ display: narrow ? 'none' : 'block' });
      assert
        .dom('[data-test-settings-jump]')
        .hasStyle({ display: narrow ? 'flex' : 'none' });
    });

    test('a link to a section opens the page there', async function (assert) {
      await visit('/apps/blog/settings?section=memory');

      assert.dom('#settings-memory').isFocused();
      assert
        .dom('[data-test-settings-index-link="memory"]')
        .hasAttribute('aria-current', 'location');

      await visit('/apps/blog/settings?section=domains');
      assert.dom('#domains').isFocused();
      assert
        .dom('[data-test-settings-index-link="publish"]')
        .hasAttribute(
          'aria-current',
          'location',
          'the domains are part of 公開',
        );
    });

    test('the domains sit in 公開, outside the settings forms', async function (assert) {
      fakes.api.on('POST', '/api/apps/blog/domains', {
        id: 3,
        hostname: 'www.example.org',
        status: 'pending',
        is_primary: false,
        force_active: false,
        records: [],
        last_checked_at: null,
        last_error: '',
      });
      await visit('/apps/blog/settings');

      assert.dom('#settings-publish [data-test-domains]').exists();
      assert.dom('#settings-publish [data-test-field="web_service"]').exists();
      assert.strictEqual(
        find('[data-test-domain-form]')!.parentElement!.closest('form'),
        null,
        'not nested in a settings form',
      );

      await fillIn('[data-test-field="web_service"]', 'app');
      await fillIn('[data-test-domain-hostname]', 'www.example.org');
      await click('[data-test-domain-add]');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/domains').length,
        1,
        'the domain is added at once',
      );
      assert.deepEqual(
        fakes.api.callsTo('PATCH', '/api/apps/blog'),
        [],
        'the settings wait for their save',
      );
      assert.dom('[data-test-field="web_service"]').hasValue('app');
    });

    test('only those who may delete see the danger zone in the index', async function (assert) {
      app = { ...app, role: 'manage' };
      await visit('/apps/blog/settings');
      assert.dom('[data-test-settings-index-link="danger"]').doesNotExist();
      assert.dom('[data-test-danger-zone]').doesNotExist();
      assert.dom('[data-test-settings-index-link="memory"]').exists();
    });
  });
});
