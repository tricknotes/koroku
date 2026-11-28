import { module, test } from 'qunit';
import {
  click,
  currentURL,
  fillIn,
  findAll,
  select,
  visit,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  serveConsole,
  type Console,
} from 'koroku-console/tests/helpers/console';
import {
  installFakes,
  makeApp,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { AppRole, Member } from 'koroku-console/utils/types';

const MEMBER_SESSION = {
  authenticated: true,
  user: { id: 2, login: 'mia', avatar_url: '', owner: false },
};

// What a member sees (ARCHITECTURE 10.7): the server refuses what a role
// does not allow; the console hides or disables it so nobody runs into the
// refusal.
module('Acceptance | roles', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let state: Console;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
  });

  function asMember(role: AppRole): void {
    fakes.api.on('GET', '/api/session', MEMBER_SESSION);
    state = serveConsole(fakes, { role });
    // The values never come for a role below manage.
    if (role === 'view' || role === 'deploy') {
      fakes.api.on('GET', '/api/apps/blog/env', () => ({
        vars: state.env.map(({ key }) => ({ key, value: null })),
        removed: [],
        compared: false,
        values_hidden: true,
        values_forbidden: true,
      }));
    }
  }

  test('a member has no way to the owner screens', async function (assert) {
    asMember('deploy');
    await visit('/');

    assert.dom('[data-test-nav="system"]').doesNotExist('no system screen');
    assert.dom('[data-test-new-app]').doesNotExist('no app creation');
    assert.dom('[data-test-role-badge="deploy"]').hasText('ロール: デプロイ');
    await click('[data-test-user-menu]');
    assert.dom('[data-test-menu-role]').hasText('メンバー');
    assert.dom('[data-test-menu-security]').exists();

    for (const url of [
      '/system',
      '/system/jobs',
      '/system/admin',
      '/apps/new',
    ]) {
      await visit(url);
      assert.strictEqual(currentURL(), '/', `${url} leads back to the list`);
    }
  });

  test('the view role only looks', async function (assert) {
    asMember('view');

    await visit('/apps/blog/deploy');
    assert.dom('[data-test-header-deploy]').doesNotExist();
    assert.dom('[data-test-deploy-form]').doesNotExist();
    assert.dom('[data-test-deploy-readonly]').includesText('「デプロイ」');
    assert.dom('[data-test-redeploy]').doesNotExist();
    assert.dom('[data-test-rollback]').doesNotExist();
    assert.dom('[data-test-deployment]').exists('the history is there');

    await visit('/apps/blog/env');
    assert.dom('[data-test-env-forbidden]').includesText('「管理」');
    assert.dom('[data-test-env-hidden]').doesNotExist('no re-login offered');
    assert.dom('[data-test-env-add]').doesNotExist();
    assert.dom('[data-test-env-edit]').doesNotExist();
    assert.dom('[data-test-env-key]').exists({ count: 2 }, 'the keys show');
    assert.dom('[data-test-env-source-form] fieldset').isDisabled();
    assert
      .dom('[data-test-env-source-group] [data-test-role-note="manage"]')
      .exists();
    assert.dom('[data-test-env-source-save]').doesNotExist();

    await visit('/apps/blog/settings?section=domains');
    assert.dom('[data-test-domains]').exists('the domains can be read');
    assert.dom('[data-test-domain-form]').doesNotExist();
    assert.dom('[data-test-domain-delete]').doesNotExist();
    assert
      .dom('[data-test-domains] [data-test-copy]')
      .isNotDisabled('copying a record is reading');

    await visit('/apps/blog/settings');
    assert.dom('[data-test-settings-form] fieldset').isDisabled();
    assert.dom('[data-test-role-note="manage"]').exists();
    assert.dom('[data-test-action-menu]').doesNotExist('no start or stop');
    assert.dom('[data-test-control]').doesNotExist();
    assert.dom('[data-test-delete]').doesNotExist();
    assert.dom('[data-test-danger-zone]').doesNotExist();
    assert.dom('[data-test-memory-summary-limit]').exists();
    assert.dom('[data-test-settings-resources-link]').exists();

    await visit('/apps/blog/deploy');
    assert
      .dom('[data-test-deploy-settings-form] fieldset')
      .isDisabled('the deploy settings can be read');
    assert.dom('[data-test-field="auto_deploy"]').exists();
    assert.dom('[data-test-deploy-settings-save]').doesNotExist();

    await visit('/apps/blog/resources');
    assert.dom('[data-test-idle-sleep-form] fieldset').isDisabled();
    assert.dom('[data-test-idle-sleep-save]').doesNotExist();
    assert.dom('[data-test-memory-settings]').exists();
    assert.dom('[data-test-memory-setting-edit]').doesNotExist();
    assert.dom('[data-test-memory-setting-add]').doesNotExist();
    assert
      .dom('[data-test-memory-settings] [data-test-role-note="manage"]')
      .exists();
  });

  test('the manage role cannot touch the owner-only settings', async function (assert) {
    asMember('manage');
    await visit('/apps/blog/settings');

    assert.dom('[data-test-settings-form] fieldset').isNotDisabled();
    assert.dom('[data-test-domain-form]').exists('manage changes the domains');
    const pending = '[data-test-domain="example.net"]';
    assert.dom(`${pending} [data-test-domain-verify]`).exists();
    assert.dom(`${pending} [data-test-domain-delete]`).exists();
    assert
      .dom('[data-test-domain-force-toggle]')
      .doesNotExist('skipping the DNS check is the owner’s');
    assert
      .dom(`${pending} [data-test-domain-force-owner-note]`)
      .hasText('強制有効化はオーナーだけが変えられます。');
    assert
      .dom('[data-test-environment-option="production"]')
      .isDisabled('the environment is the owner’s');
    assert.dom('[data-test-environment-owner-note]').exists();
    await click('[data-test-action-menu]');
    assert.dom('[data-test-control="restart"]').exists('may restart');
    assert.dom('[data-test-control="stop"]').exists('may stop');
    assert.dom('[data-test-delete]').doesNotExist('deleting is the owner’s');

    await visit('/apps/blog/deploy');
    assert.dom('[data-test-deploy-settings-form] fieldset').isNotDisabled();
    assert.dom('[data-test-field="relax_policy"]').isDisabled();
    assert.dom('[data-test-field="auto_deploy"]').isNotDisabled();
    assert.dom('[data-test-owner-only-note]').exists();
  });

  test('the Compose file written in koroku is the owner’s to change', async function (assert) {
    asMember('manage');
    Object.assign(state.app, {
      compose_source: 'inline',
      compose_content: 'services:\n  web:\n    build: .\n',
      compose_content_hidden: false,
    });
    await visit('/apps/blog/settings');

    assert.dom('[data-test-compose-owner-note]').exists();
    assert.dom('[data-test-compose-source-option="repository"]').isDisabled();
    assert.dom('[data-test-compose-source-option="inline"]').isChecked();
    assert
      .dom('[data-test-field="compose_content"]')
      .hasValue('services:\n  web:\n    build: .\n')
      .hasAttribute('readonly');
    assert
      .dom('[data-test-compose-assist]')
      .doesNotExist('nothing to write for a role that cannot save it');
  });

  test('the Compose file written in koroku is not shown below manage', async function (assert) {
    asMember('view');
    Object.assign(state.app, {
      compose_source: 'inline',
      compose_content: null,
      compose_content_hidden: true,
    });
    await visit('/apps/blog/settings');

    assert.dom('[data-test-compose-source-option="inline"]').isChecked();
    assert.dom('[data-test-field="compose_content"]').doesNotExist();
    assert.dom('[data-test-compose-hidden]').exists();
  });

  test('the deploy history says who deployed', async function (assert) {
    state = serveConsole(fakes);
    state.deployments[0]!.actor = { id: 2, login: 'mia' };
    await visit('/apps/blog/deploy');

    assert.dom('[data-test-deploy-actor]').exists({ count: 1 });
    assert.dom('[data-test-deploy-actor]').hasText('実行: mia');
  });

  test('the owner adds a member and gives it a role', async function (assert) {
    let members: Member[] = [];
    fakes.api
      .on('GET', '/api/apps', {
        apps: [makeApp(), makeApp({ slug: 'shop', environment: 'production' })],
      })
      .on('GET', '/api/members', () => ({ members }))
      .on('POST', '/api/members', () => {
        const member: Member = {
          github_user_id: 7,
          login: 'mia',
          avatar_url: '',
          added_at: '2026-11-18T03:00:00Z',
          roles: [],
        };
        members = [member];
        return member;
      })
      .on('PUT', '/api/members/7/roles/shop', () => ({
        ...members[0],
        roles: [{ app: 'shop', role: 'deploy' }],
      }))
      .on('DELETE', '/api/members/7', () => undefined);

    await visit('/');
    await click('[data-test-nav="system"]');
    await click('[data-test-system-tab="admin"]');
    assert.strictEqual(currentURL(), '/system/admin');
    assert.dom('[data-test-no-members]').exists();

    await fillIn('[data-test-member-login]', '@mia');
    await click('[data-test-member-add]');
    const added = fakes.api.calls.find((c) => c.method === 'POST');
    assert.deepEqual(added?.body, { login: 'mia' }, 'the @ is dropped');
    assert.dom('[data-test-member="mia"]').exists();
    assert.dom('[data-test-member-no-role]').exists('cannot log in yet');

    await select(
      '[data-test-member-role="shop"] [data-test-member-role-select]',
      'deploy',
    );
    const put = fakes.api.calls.find((c) => c.method === 'PUT');
    assert.deepEqual(put?.body, { role: 'deploy' });
    assert.dom('[data-test-member-no-role]').doesNotExist();

    await click('[data-test-member-remove]');
    await click('[data-test-dialog-confirm]');
    assert.ok(
      fakes.api.calls.some(
        (c) => c.method === 'DELETE' && c.path === '/api/members/7',
      ),
    );
    assert.strictEqual(findAll('[data-test-member]').length, 0);
  });

  test('a refused login name is shown at the field', async function (assert) {
    fakes.api
      .on('GET', '/api/apps', { apps: [makeApp()] })
      .on('GET', '/api/members', { members: [] })
      .fail('POST', '/api/members', 404, 'user_not_found');
    await visit('/system/admin');
    await fillIn('[data-test-member-login]', 'nobody');
    await click('[data-test-member-add]');

    assert
      .dom('[data-test-member-form] .field-error')
      .hasText('この名前の GitHub ユーザーは見つかりません。');
  });
});
