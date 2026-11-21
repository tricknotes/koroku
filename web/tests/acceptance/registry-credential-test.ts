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
  RegistryCredential,
  RegistryCredentialInput,
} from 'koroku-console/utils/types';

const PATH = '/api/apps/blog/registry-credential';
const NONE: RegistryCredential = {
  configured: false,
  registry: 'ghcr.io',
  username: '',
  updated_at: null,
};
const SET: RegistryCredential = {
  configured: true,
  registry: 'ghcr.io',
  username: 'octo-bot',
  updated_at: '2026-11-16T03:00:00Z',
};

module('Acceptance | registry credential', function (hooks) {
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
    fakes.api
      .on('GET', '/api/apps', { apps: [] })
      .on('GET', '/api/apps/blog', () => app);
  });

  test('without a credential it says what can still be pulled', async function (assert) {
    fakes.api.on('GET', PATH, NONE);
    await visit('/apps/blog/settings');

    assert.dom('[data-test-registry]').includesText('コンテナレジストリの認証');
    assert.dom('[data-test-registry-empty]').includesText('設定していません');
    assert.dom('[data-test-registry-edit]').hasText('設定する…');
    assert.dom('[data-test-registry-remove]').doesNotExist();
    assert.dom('[data-test-registry-form]').doesNotExist();
  });

  test('a credential is shown without its token', async function (assert) {
    fakes.api.on('GET', PATH, SET);
    await visit('/apps/blog/settings');

    assert.dom('[data-test-registry-host]').hasText('ghcr.io');
    assert.dom('[data-test-registry-username]').hasText('octo-bot');
    assert.dom('[data-test-registry-token]').includesText('設定済み');
    assert.dom('[data-test-registry-updated] time').exists();
    assert.dom('[data-test-registry-edit]').hasText('変更…');
    assert.dom('[data-test-registry-remove]').exists();
  });

  test('a new credential is saved with the default registry', async function (assert) {
    let saved: RegistryCredential = NONE;
    fakes.api
      .on('GET', PATH, () => saved)
      .on('PUT', PATH, (call: FakeCall) => {
        const body = call.body as RegistryCredentialInput;
        saved = {
          configured: true,
          registry: body.registry,
          username: body.username,
          updated_at: '2026-11-18T03:00:00Z',
        };
        return saved;
      });
    await visit('/apps/blog/settings');

    await click('[data-test-registry-edit]');
    assert.dom('[data-test-registry-field="registry"]').hasValue('ghcr.io');
    assert
      .dom('[data-test-registry-field="token"]')
      .hasAttribute('type', 'password');
    assert
      .dom('[data-test-registry-advice]')
      .includesText('read:packages')
      .includesText('読み取り専用のボットアカウント');

    await fillIn('[data-test-registry-field="username"]', ' octo-bot ');
    await fillIn('[data-test-registry-field="token"]', 'ghp_example');
    await click('[data-test-registry-save]');

    assert.deepEqual(
      fakes.api.callsTo('PUT', PATH).map((c) => c.body),
      [{ registry: 'ghcr.io', username: 'octo-bot', token: 'ghp_example' }],
    );
    assert.dom('[data-test-registry-form]').doesNotExist();
    assert.dom('[data-test-registry-saved]').exists();
    assert.dom('[data-test-registry-username]').hasText('octo-bot');
    assert.dom('[data-test-registry]').doesNotIncludeText('ghp_example');
  });

  test('changing keeps the token when the field is left empty', async function (assert) {
    fakes.api.on('GET', PATH, SET).on('PUT', PATH, () => ({
      ...SET,
      registry: 'registry.example.com',
    }));
    await visit('/apps/blog/settings');

    await click('[data-test-registry-edit]');
    assert.dom('[data-test-registry-field="username"]').hasValue('octo-bot');
    assert.dom('[data-test-registry-field="token"]').hasValue('');
    assert.dom('[data-test-field-group="token"]').includesText('空のまま');
    await fillIn(
      '[data-test-registry-field="registry"]',
      'registry.example.com',
    );
    await click('[data-test-registry-save]');

    assert.deepEqual(
      fakes.api.callsTo('PUT', PATH).map((c) => c.body),
      [{ registry: 'registry.example.com', username: 'octo-bot', token: '' }],
    );
    assert.dom('[data-test-registry-host]').hasText('registry.example.com');
  });

  test('refused fields are explained next to them', async function (assert) {
    fakes.api
      .on('GET', PATH, NONE)
      .fail('PUT', PATH, 422, 'validation_failed', {
        registry: 'enter the registry host',
      });
    await visit('/apps/blog/settings');

    await click('[data-test-registry-edit]');
    await fillIn('[data-test-registry-field="registry"]', 'https://ghcr.io');
    await click('[data-test-registry-save]');

    assert
      .dom('[data-test-field-group="registry"] [data-test-field-error]')
      .includesText('https:// やパスは付けません');
    assert.dom('[data-test-registry-form]').exists('the form stays open');
  });

  test('removing asks first', async function (assert) {
    fakes.api.on('GET', PATH, SET).on('DELETE', PATH, NONE);
    await visit('/apps/blog/settings');

    await click('[data-test-registry-remove]');
    assert.dom('[data-test-dialog]').includesText('private');
    assert.deepEqual(fakes.api.callsTo('DELETE', PATH), []);

    await click('[data-test-dialog-confirm]');
    assert.strictEqual(fakes.api.callsTo('DELETE', PATH).length, 1);
    assert.dom('[data-test-registry-empty]').exists();
  });

  test('without a recent login it asks to log in again before the form', async function (assert) {
    fakes.api
      .on('GET', '/api/session', {
        authenticated: true,
        user: { id: 1, login: 'octo', avatar_url: '' },
        reauth_until: '2026-01-01T00:00:00Z',
      })
      .on('GET', PATH, SET);
    await visit('/apps/blog/settings');

    await click('[data-test-registry-edit]');
    assert.dom('[data-test-reauth-dialog]').exists();
    assert.dom('[data-test-registry-form]').doesNotExist();

    await click('[data-test-registry-remove]');
    assert
      .dom('[data-test-dialog]')
      .doesNotIncludeText('コンテナレジストリの認証を削除しますか');
    assert.dom('[data-test-reauth-dialog]').exists();
    assert.deepEqual(fakes.api.callsTo('DELETE', PATH), []);
  });

  test('below manage it can be seen but not changed', async function (assert) {
    app = { ...app, role: 'deploy' };
    fakes.api.on('GET', PATH, SET);
    await visit('/apps/blog/settings');

    assert.dom('[data-test-registry-username]').hasText('octo-bot');
    assert.dom('[data-test-registry-edit]').doesNotExist();
    assert.dom('[data-test-registry-remove]').doesNotExist();
  });
});
