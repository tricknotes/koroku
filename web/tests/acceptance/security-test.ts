import { module, test } from 'qunit';
import { click, currentURL, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  brokenReferences,
  unnamedControls,
} from 'koroku-console/tests/helpers/a11y';
import {
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { LoginEvent, LoginSession } from 'koroku-console/utils/types';

const CHROME_MAC =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36';
const SAFARI_IPHONE =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1';

function makeSession(overrides: Partial<LoginSession> = {}): LoginSession {
  return {
    id: '9f2c41d07a1b3e55',
    created_at: '2026-11-18T03:00:00Z',
    last_seen_at: '2026-11-18T03:05:00Z',
    expires_at: '2026-11-25T03:00:00Z',
    ip: '203.0.113.5',
    user_agent: CHROME_MAC,
    current: true,
    ...overrides,
  };
}

function makeEvent(overrides: Partial<LoginEvent> = {}): LoginEvent {
  return {
    at: '2026-11-18T03:00:00Z',
    outcome: 'succeeded',
    github_user_id: 1,
    login: 'octo',
    ip: '203.0.113.5',
    user_agent: CHROME_MAC,
    ...overrides,
  };
}

module('Acceptance | security', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    fakes.api
      .on('GET', '/api/apps', { apps: [makeApp()] })
      .on('GET', '/api/sessions', {
        sessions: [
          makeSession(),
          makeSession({
            id: 'aa00bb11cc22dd33',
            current: false,
            ip: '198.51.100.7',
            user_agent: SAFARI_IPHONE,
          }),
        ],
      })
      .on('GET', '/api/login-events', {
        events: [
          makeEvent(),
          makeEvent({
            outcome: 'not_allowed',
            login: 'stranger',
            github_user_id: 7,
            ip: '192.0.2.44',
          }),
        ],
      });
  });

  test('the user menu leads to the session list and login history', async function (assert) {
    await visit('/');
    await click('[data-test-user-menu]');
    await click('[data-test-menu-security]');

    assert.strictEqual(currentURL(), '/security');
    assert.dom('h1').hasText('セキュリティ');
    assert.dom('[data-test-session]').exists({ count: 2 });
    assert
      .dom('[data-test-session="9f2c41d07a1b3e55"] [data-test-session-agent]')
      .hasText('Chrome / macOS')
      .hasAttribute('title', CHROME_MAC);
    assert
      .dom('[data-test-session="9f2c41d07a1b3e55"] [data-test-session-current]')
      .exists('the own session is marked, not offered to end');
    assert
      .dom('[data-test-session="aa00bb11cc22dd33"] [data-test-session-agent]')
      .hasText('Safari / iOS');
    assert
      .dom('[data-test-session="aa00bb11cc22dd33"] [data-test-session-ip]')
      .hasText('198.51.100.7');

    assert.dom('[data-test-login-event]').exists({ count: 2 });
    assert
      .dom('[data-test-login-event="not_allowed"]')
      .includesText('許可されていないアカウント')
      .includesText('stranger');

    assert.deepEqual(unnamedControls(), [], 'every control has a name');
    assert.deepEqual(brokenReferences(), [], 'no broken ARIA references');
  });

  test('a single session can be ended', async function (assert) {
    fakes.api.on('DELETE', '/api/sessions/aa00bb11cc22dd33', undefined);
    await visit('/security');
    fakes.api.on('GET', '/api/sessions', { sessions: [makeSession()] });

    await click(
      '[data-test-session="aa00bb11cc22dd33"] [data-test-session-end]',
    );

    assert.strictEqual(
      fakes.api.callsTo('DELETE', '/api/sessions/aa00bb11cc22dd33').length,
      1,
    );
    assert.dom('[data-test-session]').exists({ count: 1 });
    assert.dom('[data-test-revoke-others]').doesNotExist();
  });

  test('all other sessions can be ended after a confirmation', async function (assert) {
    fakes.api.on('POST', '/api/sessions/revoke-others', { revoked: 1 });
    await visit('/security');

    await click('[data-test-revoke-others]');
    assert.dom('[data-test-dialog]').includesText('1 件');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/sessions/revoke-others'),
      [],
      'nothing happens before confirming',
    );

    fakes.api.on('GET', '/api/sessions', { sessions: [makeSession()] });
    await click('[data-test-dialog-confirm]');

    assert.strictEqual(
      fakes.api.callsTo('POST', '/api/sessions/revoke-others').length,
      1,
    );
    assert
      .dom('[data-test-revoked]')
      .hasText('1 件のセッションを終了しました。');
    assert.dom('[data-test-session]').exists({ count: 1 });
  });

  test('the time left for dangerous operations is shown', async function (assert) {
    const until = new Date(Date.now() + 7 * 60_000 - 1000).toISOString();
    fakes.api.on('GET', '/api/session', {
      authenticated: true,
      user: { id: 1, login: 'octo', avatar_url: '' },
      reauth_until: until,
    });
    await visit('/security');

    assert
      .dom('[data-test-reauth-state]')
      .hasText('あと 7 分は再ログインなしで操作できます');
    assert.dom('[data-test-reauth-now]').doesNotExist();
  });

  test('without a recent login the page offers to log in again', async function (assert) {
    fakes.api.on('GET', '/api/session', {
      authenticated: true,
      user: { id: 1, login: 'octo', avatar_url: '' },
      reauth_until: '2026-01-01T00:00:00Z',
    });
    await visit('/security');

    assert
      .dom('[data-test-reauth-state]')
      .hasText('次に危険な操作をするときは再ログインが必要です');
    await click('[data-test-reauth-now]');
    assert.deepEqual(fakes.browser.assigned, [
      '/auth/github/login?return_to=%2Fsecurity',
    ]);
  });

  test('a refused operation asks to log in again and comes back', async function (assert) {
    const running = makeDeployment();
    fakes.api
      .on('GET', '/api/apps/blog', makeApp({ current_deployment: running }))
      .fail('DELETE', '/api/apps/blog', 403, 'reauth_required');
    await visit('/apps/blog/settings');

    await click('[data-test-delete]');
    await fillIn('[data-test-dialog-input]', 'blog');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-reauth-dialog]').exists();
    await click(
      '[data-test-dialog]:has([data-test-reauth-dialog]) [data-test-dialog-confirm]',
    );
    assert.deepEqual(fakes.browser.assigned, [
      '/auth/github/login?return_to=%2Fapps%2Fblog%2Fsettings',
    ]);
  });

  test('the reasons for refused logins are explained', async function (assert) {
    fakes.api.on('GET', '/api/session', { authenticated: false });
    fakes.browser.query = '?login_error=two_factor_required';
    await visit('/');

    assert
      .dom('[data-test-login-error]')
      .includesText('2 段階認証が有効になっていない');
  });
});
