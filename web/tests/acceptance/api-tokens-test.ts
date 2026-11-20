import { module, test } from 'qunit';
import { click, fillIn, select, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  brokenReferences,
  unnamedControls,
} from 'koroku-console/tests/helpers/a11y';
import {
  installFakes,
  makeApp,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { ApiToken } from 'koroku-console/utils/types';

const VALUE = 'koroku_ro_' + 'A'.repeat(43);

function makeToken(overrides: Partial<ApiToken> = {}): ApiToken {
  return {
    id: '3fa9c2d17b40e815',
    name: 'ノート PC の Claude Code',
    created_at: '2026-11-18T03:00:00Z',
    expires_at: '2026-12-18T03:00:00Z',
    last_used_at: '2026-11-19T03:00:00Z',
    last_used_ip: '203.0.113.5',
    ...overrides,
  };
}

module('Acceptance | API tokens', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    fakes.api
      .on('GET', '/api/apps', { apps: [makeApp()] })
      .on('GET', '/api/sessions', { sessions: [] })
      .on('GET', '/api/login-events', { events: [] })
      .on('GET', '/api/tokens', {
        tokens: [
          makeToken(),
          makeToken({
            id: '0b7e14c9d2a35f68',
            name: 'デスクトップ',
            last_used_at: null,
            last_used_ip: '',
          }),
        ],
      });
  });

  test('the security page lists the tokens without their values', async function (assert) {
    await visit('/security');

    assert.dom('[data-test-token]').exists({ count: 2 });
    assert
      .dom('[data-test-token="3fa9c2d17b40e815"] [data-test-token-name]')
      .hasText('ノート PC の Claude Code');
    assert
      .dom('[data-test-token="3fa9c2d17b40e815"] [data-test-token-last-used]')
      .includesText('203.0.113.5');
    assert
      .dom('[data-test-token="0b7e14c9d2a35f68"] [data-test-token-last-used]')
      .includesText('未使用');
    assert.dom('[data-test-token-value]').doesNotExist();

    assert.deepEqual(unnamedControls(), [], 'every control has a name');
    assert.deepEqual(brokenReferences(), [], 'no broken ARIA references');
  });

  test('without tokens there is one way to create one', async function (assert) {
    fakes.api.on('GET', '/api/tokens', { tokens: [] });
    await visit('/security');

    assert.dom('[data-test-no-tokens]').exists();
    assert.dom('[data-test-token-create]').exists({ count: 1 });
  });

  test('a created token is shown once with a copy button', async function (assert) {
    fakes.api.on('POST', '/api/tokens', {
      token: makeToken({ id: 'new0000000000001', name: 'laptop' }),
      value: VALUE,
    });
    await visit('/security');

    await click('[data-test-token-create]');
    assert.dom('[data-test-token-dialog]').exists();
    await fillIn('[data-test-token-name-input]', '  laptop  ');
    await select('[data-test-token-lifetime]', '90');
    fakes.api.on('GET', '/api/tokens', {
      tokens: [makeToken({ id: 'new0000000000001', name: 'laptop' })],
    });
    await click('[data-test-dialog-confirm]');

    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/tokens').map((c) => c.body),
      [{ name: 'laptop', expires_in_days: 90 }],
    );
    assert.dom('[data-test-token-value]').hasText(VALUE);
    assert.dom('[data-test-token-once]').includesText('今しか表示されません');
    assert
      .dom('[data-test-token-snippet]')
      .includesText(`export KOROKU_TOKEN=${VALUE}`);

    await click('[data-test-token-copy]');
    assert.deepEqual(fakes.browser.copied, [VALUE]);

    await click('[data-test-dialog-close]');
    assert.dom('[data-test-token-dialog]').doesNotExist();
    assert.dom('[data-test-token-value]').doesNotExist('never shown again');
    assert.dom('[data-test-token]').exists({ count: 1 });
  });

  test('invalid input is explained in the form', async function (assert) {
    fakes.api.fail('POST', '/api/tokens', 422, 'validation_failed', {
      name: 'is required',
    });
    await visit('/security');

    await click('[data-test-token-create]');
    // The browser itself refuses an empty name; the server has the last say.
    await fillIn('[data-test-token-name-input]', '   ');
    await click('[data-test-dialog-confirm]');

    assert
      .dom('[data-test-token-dialog]')
      .includesText('名前を 1〜100 文字で入力してください。');
    assert.dom('[data-test-token-value]').doesNotExist();
  });

  test('creating without a recent login asks to log in again first', async function (assert) {
    fakes.api.on('GET', '/api/session', {
      authenticated: true,
      user: { id: 1, login: 'octo', avatar_url: '' },
      reauth_until: '2026-01-01T00:00:00Z',
    });
    await visit('/security');

    await click('[data-test-token-create]');

    assert.dom('[data-test-token-dialog]').doesNotExist();
    assert.dom('[data-test-reauth-dialog]').exists();
    assert.deepEqual(fakes.api.callsTo('POST', '/api/tokens'), []);
  });

  test('a token is revoked after a confirmation', async function (assert) {
    fakes.api.on('DELETE', '/api/tokens/3fa9c2d17b40e815', undefined);
    await visit('/security');

    await click(
      '[data-test-token="3fa9c2d17b40e815"] [data-test-token-revoke]',
    );
    assert.dom('[data-test-dialog]').includesText('ノート PC の Claude Code');
    assert.deepEqual(
      fakes.api.callsTo('DELETE', '/api/tokens/3fa9c2d17b40e815'),
      [],
      'nothing happens before confirming',
    );

    fakes.api.on('GET', '/api/tokens', {
      tokens: [makeToken({ id: '0b7e14c9d2a35f68', name: 'デスクトップ' })],
    });
    await click('[data-test-dialog-confirm]');

    assert.strictEqual(
      fakes.api.callsTo('DELETE', '/api/tokens/3fa9c2d17b40e815').length,
      1,
    );
    assert.dom('[data-test-token]').exists({ count: 1 });
  });
});
