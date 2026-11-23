import { module, test } from 'qunit';
import { click, fillIn, visit, waitFor } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { Domain } from 'koroku-console/utils/types';

const DOMAINS = '/api/apps/blog/domains';
const NO_HOST = 'lookup www.example.org: no such host';

function makeDomain(overrides: Partial<Domain> = {}): Domain {
  const hostname = overrides.hostname ?? 'www.example.org';
  return {
    id: 3,
    hostname,
    status: 'pending',
    is_primary: false,
    force_active: false,
    last_checked_at: null,
    last_error: '',
    records: [
      { type: 'CNAME', name: hostname, value: 'blog.dev.example.com' },
      { type: 'A', name: hostname, value: '203.0.113.10' },
    ],
    ...overrides,
  };
}

module('Acceptance | domains', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let domains: Domain[];

  function update(id: number, changes: Partial<Domain>): Domain {
    domains = domains.map((d) => (d.id === id ? { ...d, ...changes } : d));
    return domains.find((d) => d.id === id) as Domain;
  }

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    domains = [];
    fakes.api
      .on('GET', '/api/apps/blog', makeApp())
      .on('GET', DOMAINS, () => ({
        default_hostname: 'blog.dev.example.com',
        domains,
      }))
      .on('POST', DOMAINS, (call: FakeCall) => {
        const { hostname } = call.body as { hostname: string };
        const created = makeDomain({
          hostname,
          last_checked_at: '2026-11-18T03:00:00Z',
          last_error: NO_HOST,
        });
        domains = [...domains, created];
        return created;
      })
      .on('POST', `${DOMAINS}/3/verify`, () =>
        update(3, {
          status: 'active',
          last_error: '',
          last_checked_at: '2026-11-18T03:05:00Z',
        }),
      )
      .on('PATCH', `${DOMAINS}/3`, (call: FakeCall) =>
        update(3, call.body as Partial<Domain>),
      )
      .on('DELETE', `${DOMAINS}/3`, () => {
        domains = domains.filter((d) => d.id !== 3);
        return undefined;
      });
  });

  test('the assigned domain is always listed, and without custom ones the form is the next step', async function (assert) {
    await visit('/apps/blog/settings?section=domains');

    assert.dom('[data-test-default-hostname]').hasText('blog.dev.example.com');
    assert
      .dom('[data-test-domain-default]')
      .includesText('自動割当')
      .includesText('有効');
    assert
      .dom('[data-test-domain-default] a')
      .hasAttribute('href', 'https://blog.dev.example.com')
      .hasAttribute('target', '_blank')
      .hasAttribute('rel', 'noopener noreferrer');
    assert
      .dom('[data-test-domain-default] button')
      .exists({ count: 1 }, 'only the copy button: it cannot be removed');

    await click('[data-test-domain-default] [data-test-copy]');
    assert.deepEqual(fakes.browser.copied, ['blog.dev.example.com']);

    assert
      .dom('[data-test-no-domains]')
      .includesText('独自ドメインはまだありません');
    assert.dom('[data-test-domain]').doesNotExist();
    assert
      .dom('[data-test-domain-form] button')
      .exists({ count: 1 })
      .hasText('追加');
  });

  test('add, verify and make primary', async function (assert) {
    await visit('/apps/blog/settings?section=domains');

    await fillIn('[data-test-domain-hostname]', ' WWW.example.org ');
    await click('[data-test-domain-add]');

    assert.deepEqual(fakes.api.callsTo('POST', DOMAINS)[0]?.body, {
      hostname: 'www.example.org',
    });
    const row = '[data-test-domain="www.example.org"]';
    assert
      .dom(`${row} [data-test-domain-status]`)
      .hasText('確認待ち')
      .hasAttribute('data-status', 'pending');
    assert
      .dom(`${row} [data-test-domain-status] svg`)
      .hasAttribute('data-icon', 'status-pending', 'not colour alone');
    assert.dom(`${row} [data-test-domain-pending]`).exists();
    assert
      .dom(`${row} [data-test-domain-primary-toggle]`)
      .doesNotExist('a pending domain cannot become primary');
    assert.dom('[data-test-domain-hostname]').hasValue('');
    assert.dom('[data-test-no-domains]').doesNotExist();
    assert
      .dom('[data-test-domain-notice]')
      .includesText('www.example.org を追加しました')
      .includesText('DNS レコードを設定してください');

    await click(`${row} [data-test-domain-verify]`);

    assert.strictEqual(
      fakes.api.callsTo('POST', `${DOMAINS}/3/verify`).length,
      1,
    );
    assert
      .dom(`${row} [data-test-domain-status]`)
      .hasText('有効')
      .hasAttribute('data-status', 'running');
    assert.dom(`${row} [data-test-domain-pending]`).doesNotExist();
    assert.dom(`${row} [data-test-domain-error]`).doesNotExist();
    assert.dom('[data-test-domain-notice]').includesText('有効になりました');

    await click(`${row} [data-test-domain-primary-toggle]`);

    assert.deepEqual(fakes.api.callsTo('PATCH', `${DOMAINS}/3`)[0]?.body, {
      is_primary: true,
    });
    assert.dom(`${row} [data-test-domain-primary]`).hasText('プライマリ');
    assert
      .dom(`${row} [data-test-domain-primary-toggle]`)
      .hasText('プライマリを解除');
    assert
      .dom('[data-test-domain-notice]')
      .includesText('プライマリにしました');

    await click(`${row} [data-test-domain-primary-toggle]`);

    assert.deepEqual(fakes.api.callsTo('PATCH', `${DOMAINS}/3`)[1]?.body, {
      is_primary: false,
    });
    assert.dom(`${row} [data-test-domain-primary]`).doesNotExist();
  });

  test('a pending domain shows the DNS records to set, each value with a copy button', async function (assert) {
    domains = [
      makeDomain({
        last_checked_at: '2026-11-18T03:00:00Z',
        last_error: NO_HOST,
      }),
    ];

    await visit('/apps/blog/settings?section=domains');
    const row = '[data-test-domain="www.example.org"]';

    assert
      .dom(`${row} [data-test-domain-pending]`)
      .includesText('DNS に次のレコードを設定してください')
      .includesText('どちらか一方で構いません')
      .includesText('5 分ごとに自動で再確認します');
    assert.dom(`${row} [data-test-domain-record]`).exists({ count: 2 });
    assert
      .dom(`${row} [data-test-domain-record="CNAME"]`)
      .includesText('種類')
      .includesText('CNAME')
      .includesText('ホスト')
      .includesText('www.example.org')
      .includesText('値')
      .includesText('blog.dev.example.com');
    assert
      .dom(`${row} [data-test-domain-record="A"]`)
      .includesText('203.0.113.10');

    assert
      .dom(
        `${row} [data-test-domain-record="CNAME"] [data-test-copy-record-value]`,
      )
      .hasAria('label', 'CNAME レコードの値をコピー');
    await click(
      `${row} [data-test-domain-record="CNAME"] [data-test-copy-record-value]`,
    );
    await click(
      `${row} [data-test-domain-record="A"] [data-test-copy-record-name]`,
    );
    await click(
      `${row} [data-test-domain-record="A"] [data-test-copy-record-value]`,
    );
    assert.deepEqual(fakes.browser.copied, [
      'blog.dev.example.com',
      'www.example.org',
      '203.0.113.10',
    ]);

    assert
      .dom(`${row} [data-test-domain-error]`)
      .includesText('直近の確認結果')
      .includesText(NO_HOST);
    assert
      .dom(`${row} [data-test-domain-error] time`)
      .hasAttribute('datetime', '2026-11-18T03:00:00Z')
      .hasAttribute('title', '2026/11/18 12:00 JST');
    assert.dom(`${row} [data-test-domain-error] svg`).exists();
  });

  test('an apex domain has one record, and a domain never checked says so', async function (assert) {
    domains = [
      makeDomain({
        hostname: 'example.org',
        records: [{ type: 'A', name: 'example.org', value: '203.0.113.10' }],
      }),
    ];

    await visit('/apps/blog/settings?section=domains');

    assert.dom('[data-test-domain-record]').exists({ count: 1 });
    assert.dom('[data-test-domain-pending]').doesNotIncludeText('どちらか一方');
    assert.dom('[data-test-domain-checked]').hasText('まだ確認していません。');
    assert.dom('[data-test-domain-error]').doesNotExist();
  });

  test('a re-check that still fails updates the reason and says so', async function (assert) {
    domains = [makeDomain({ last_error: NO_HOST })];
    fakes.api.on('POST', `${DOMAINS}/3/verify`, () =>
      update(3, {
        last_checked_at: '2026-11-18T03:05:00Z',
        last_error: 'www.example.org resolves to 198.51.100.7',
      }),
    );

    await visit('/apps/blog/settings?section=domains');
    await click('[data-test-domain-verify]');

    assert.dom('[data-test-domain-status]').hasText('確認待ち');
    assert
      .dom('[data-test-domain-error]')
      .includesText('resolves to 198.51.100.7')
      .doesNotIncludeText('no such host');
    assert
      .dom('[data-test-domain-notice]')
      .includesText('まだ確認できませんでした');
  });

  test('pending domains are re-read by polling until they are active', async function (assert) {
    domains = [makeDomain({ last_error: NO_HOST })];

    await visit('/apps/blog/settings?section=domains');
    const listReads = () => fakes.api.callsTo('GET', DOMAINS).length;

    await fakes.poller.tick();

    assert.strictEqual(listReads(), 2, 'the list is read again');
    assert.dom('[data-test-domain-status]').hasText('確認待ち');

    // The server's own check succeeded in the meantime.
    update(3, { status: 'active', last_error: '' });
    await fakes.poller.tick();

    assert.dom('[data-test-domain-status]').hasText('有効');
    assert.dom('[data-test-domain-pending]').doesNotExist();

    const settledReads = listReads();
    await fakes.poller.tick();

    assert.strictEqual(
      listReads(),
      settledReads,
      'with nothing pending there is nothing to poll',
    );
  });

  test('force-activate and delete', async function (assert) {
    domains = [makeDomain()];

    await visit('/apps/blog/settings?section=domains');
    const row = '[data-test-domain="www.example.org"]';

    assert.dom(`${row} [data-test-domain-force-toggle]`).hasText('強制有効化');

    await click(`${row} [data-test-domain-force-toggle]`);
    assert.deepEqual(fakes.api.callsTo('PATCH', `${DOMAINS}/3`)[0]?.body, {
      force_active: true,
    });
    assert.dom(`${row} [data-test-domain-forced]`).exists();
    assert
      .dom(`${row} [data-test-domain-force-toggle]`)
      .hasText('強制有効化を解除');

    await click(`${row} [data-test-domain-delete]`);
    assert
      .dom('[data-test-dialog]')
      .includesText('ドメインを削除しますか？')
      .includesText('www.example.org');
    assert.dom('[data-test-dialog-confirm]').hasClass('button-danger');
    assert.deepEqual(fakes.api.callsTo('DELETE', `${DOMAINS}/3`), []);

    await click('[data-test-dialog-cancel]');
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(fakes.api.callsTo('DELETE', `${DOMAINS}/3`), []);
    assert.dom(row).exists('canceling keeps the domain');

    await click(`${row} [data-test-domain-delete]`);
    await click('[data-test-dialog-confirm]');
    assert.strictEqual(fakes.api.callsTo('DELETE', `${DOMAINS}/3`).length, 1);
    assert.dom(row).doesNotExist();
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.dom('[data-test-domain-notice]').includesText('削除しました');
    assert.dom('[data-test-no-domains]').exists();
  });

  test('an active domain offers no force switch, and deleting the primary one warns', async function (assert) {
    domains = [makeDomain({ status: 'active', is_primary: true })];

    await visit('/apps/blog/settings?section=domains');

    assert.dom('[data-test-domain-force-toggle]').doesNotExist();
    assert.dom('[data-test-domain-pending]').doesNotExist();

    await click('[data-test-domain-delete]');

    assert
      .dom('[data-test-dialog-primary]')
      .includesText('プライマリの指定も外れ');
  });

  test('changes are passed on to the app, for the header', async function (assert) {
    domains = [makeDomain({ status: 'active' })];

    await visit('/apps/blog/settings?section=domains');
    const before = fakes.api.callsTo('GET', '/api/apps/blog').length;

    await click('[data-test-domain-primary-toggle]');

    assert.strictEqual(
      fakes.api.callsTo('GET', '/api/apps/blog').length,
      before + 1,
      'the primary URL may have changed',
    );
  });

  test('an empty hostname is refused before any request', async function (assert) {
    await visit('/apps/blog/settings?section=domains');
    await click('[data-test-domain-add]');

    assert
      .dom('[data-test-field-error]')
      .hasText('ホスト名を入力してください。');
    assert.dom('[data-test-domain-hostname]').hasAria('invalid', 'true');
    assert.deepEqual(fakes.api.callsTo('POST', DOMAINS), []);
  });

  test('a taken or invalid hostname is reported at the input', async function (assert) {
    fakes.api.fail('POST', DOMAINS, 409, 'hostname_taken');

    await visit('/apps/blog/settings?section=domains');
    await fillIn('[data-test-domain-hostname]', 'www.example.org');
    await click('[data-test-domain-add]');

    assert
      .dom('[data-test-field-error]')
      .hasText('このホスト名はすでに登録されています。');
    assert.dom('[data-test-domain-hostname]').hasValue('www.example.org');
    assert.dom('[data-test-error]').doesNotExist();

    fakes.api.fail('POST', DOMAINS, 422, 'validation_failed', {
      hostname: 'must not be under the base domain',
    });
    await fillIn('[data-test-domain-hostname]', 'x.dev.example.com');
    await click('[data-test-domain-add]');

    assert
      .dom('[data-test-field-error]')
      .hasText('must not be under the base domain');
  });

  test('an add that fails for another reason is reported under the form', async function (assert) {
    fakes.api.fail('POST', DOMAINS, 500, 'internal');

    await visit('/apps/blog/settings?section=domains');
    await fillIn('[data-test-domain-hostname]', 'www.example.org');
    await click('[data-test-domain-add]');

    assert
      .dom('[data-test-domain-add-section] [data-test-error]')
      .includesText('サーバーでエラーが発生しました');
    assert.dom('[data-test-field-error]').doesNotExist();
    assert.dom('[data-test-domain-hostname]').hasValue('www.example.org');
  });

  test('an app with as many domains as it can have says so under the form', async function (assert) {
    fakes.api.fail('POST', DOMAINS, 422, 'too_many_domains', { limit: 20 });

    await visit('/apps/blog/settings?section=domains');
    await fillIn('[data-test-domain-hostname]', 'www.example.org');
    await click('[data-test-domain-add]');

    assert
      .dom('[data-test-domain-add-section] [data-test-error]')
      .hasText(
        '独自ドメインはアプリごとに 20 個までです。使っていないものを削除してから追加してください。',
      );
    assert.dom('[data-test-field-error]').doesNotExist();
    assert.dom('[data-test-domain-hostname]').hasValue('www.example.org');
  });

  test('a failed action shows its error and keeps the list', async function (assert) {
    domains = [makeDomain()];
    fakes.api.fail('PATCH', `${DOMAINS}/3`, 500, 'internal');

    await visit('/apps/blog/settings?section=domains');
    await click('[data-test-domain-force-toggle]');

    assert
      .dom('[data-test-error]')
      .includesText('サーバーでエラーが発生しました');
    assert.dom('[data-test-domain="www.example.org"]').exists();
    assert.dom('[data-test-domain-notice]').doesNotExist();
  });

  test('the reason of a refused change is shown', async function (assert) {
    domains = [makeDomain({ status: 'active' })];
    fakes.api.fail('PATCH', `${DOMAINS}/3`, 422, 'validation_failed', {
      is_primary: 'only an active domain can be the primary domain',
    });

    await visit('/apps/blog/settings?section=domains');
    await click('[data-test-domain-primary-toggle]');

    assert
      .dom('[data-test-error-detail]')
      .hasText('is_primary: only an active domain can be the primary domain');
  });

  test('skeleton rows while loading, and a failed list can be retried', async function (assert) {
    let fail: (reason: unknown) => void = () => {};
    fakes.api.on(
      'GET',
      DOMAINS,
      () => new Promise((_resolve, reject) => (fail = reject)),
    );

    const visiting = visit('/apps/blog/settings?section=domains');
    await waitFor('[data-test-loading]');

    assert.dom('[data-test-loading] .skeleton').exists();
    assert.dom('[data-test-domain-default]').doesNotExist();
    assert
      .dom('[data-test-domain-form]')
      .doesNotExist('nothing to add to until the list is there');

    fail(new Error('gone'));
    await visiting;

    assert
      .dom('[data-test-error]')
      .hasAttribute('role', 'alert')
      .includesText('ドメインの一覧を取得できませんでした');
    assert.dom('[data-test-domain-form]').doesNotExist();

    fakes.api.on('GET', DOMAINS, {
      default_hostname: 'blog.dev.example.com',
      domains: [],
    });
    await click('[data-test-error] button');

    assert.dom('[data-test-error]').doesNotExist();
    assert.dom('[data-test-default-hostname]').hasText('blog.dev.example.com');
    assert.dom('[data-test-domain-form]').exists();
  });

  test('a failed refresh keeps the list and says it is stale', async function (assert) {
    domains = [makeDomain()];

    await visit('/apps/blog/settings?section=domains');

    fakes.api.fail('GET', DOMAINS, 500, 'internal');
    await fakes.poller.tick();

    assert.dom('[data-test-domain="www.example.org"]').exists();
    assert
      .dom('[data-test-stale]')
      .includesText('最新の状態を取得できていません');
  });
});
