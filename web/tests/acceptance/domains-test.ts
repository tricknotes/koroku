import { module, test } from 'qunit';
import { click, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  installFakes,
  makeApp,
  type FakeCall,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';
import type { Domain } from 'paas-console/utils/types';

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
      .on('GET', '/api/apps/blog/domains', () => ({
        default_hostname: 'blog.dev.example.com',
        domains,
      }))
      .on('POST', '/api/apps/blog/domains', (call: FakeCall) => {
        const { hostname } = call.body as { hostname: string };
        const created = makeDomain({
          hostname,
          last_checked_at: '2026-11-18T03:00:00Z',
          last_error: 'no A or CNAME record points to this server',
        });
        domains = [...domains, created];
        return created;
      })
      .on('POST', '/api/apps/blog/domains/3/verify', () =>
        update(3, { status: 'active', last_error: '' }),
      )
      .on('PATCH', '/api/apps/blog/domains/3', (call: FakeCall) =>
        update(3, call.body as Partial<Domain>),
      )
      .on('DELETE', '/api/apps/blog/domains/3', () => {
        domains = domains.filter((d) => d.id !== 3);
        return undefined;
      });
  });

  test('add, verify and make primary', async function (assert) {
    await visit('/apps/blog/domains');

    assert.dom('[data-test-default-hostname]').hasText('blog.dev.example.com');
    assert.dom('[data-test-no-domains]').exists();

    await fillIn('[data-test-domain-hostname]', ' WWW.example.org ');
    await click('[data-test-domain-add]');

    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/domains')[0]?.body,
      { hostname: 'www.example.org' },
    );
    const row = '[data-test-domain="www.example.org"]';
    assert.dom(`${row} [data-test-domain-status]`).hasText('確認待ち');
    assert
      .dom(`${row} [data-test-domain-records]`)
      .includesText('CNAME')
      .includesText('blog.dev.example.com')
      .includesText('203.0.113.10');
    assert
      .dom(`${row} [data-test-domain-error]`)
      .includesText('no A or CNAME record points to this server');
    assert
      .dom(`${row} [data-test-domain-primary-toggle]`)
      .isDisabled('a pending domain cannot become primary');
    assert.dom('[data-test-domain-hostname]').hasValue('');

    await click(`${row} [data-test-domain-verify]`);

    assert.strictEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/domains/3/verify').length,
      1,
    );
    assert.dom(`${row} [data-test-domain-status]`).hasText('有効');
    assert.dom(`${row} [data-test-domain-error]`).doesNotExist();

    await click(`${row} [data-test-domain-primary-toggle]`);

    assert.deepEqual(
      fakes.api.callsTo('PATCH', '/api/apps/blog/domains/3')[0]?.body,
      { is_primary: true },
    );
    assert.dom(`${row} [data-test-domain-primary]`).exists();
    assert
      .dom(`${row} [data-test-domain-primary-toggle]`)
      .hasText('プライマリを解除');

    await click(`${row} [data-test-domain-primary-toggle]`);

    assert.deepEqual(
      fakes.api.callsTo('PATCH', '/api/apps/blog/domains/3')[1]?.body,
      { is_primary: false },
    );
    assert.dom(`${row} [data-test-domain-primary]`).doesNotExist();
  });

  test('force-activate and delete', async function (assert) {
    domains = [makeDomain()];

    await visit('/apps/blog/domains');
    const row = '[data-test-domain="www.example.org"]';

    await click(`${row} [data-test-domain-force-toggle]`);
    assert.deepEqual(
      fakes.api.callsTo('PATCH', '/api/apps/blog/domains/3')[0]?.body,
      { force_active: true },
    );
    assert.dom(`${row} [data-test-domain-forced]`).exists();
    assert
      .dom(`${row} [data-test-domain-force-toggle]`)
      .hasText('強制有効化を解除');

    await click(`${row} [data-test-domain-delete]`);
    assert.dom('[data-test-dialog]').includesText('www.example.org');
    assert.deepEqual(
      fakes.api.callsTo('DELETE', '/api/apps/blog/domains/3'),
      [],
    );

    await click('[data-test-dialog-confirm]');
    assert.strictEqual(
      fakes.api.callsTo('DELETE', '/api/apps/blog/domains/3').length,
      1,
    );
    assert.dom(row).doesNotExist();
    assert.dom('[data-test-dialog]').doesNotExist();
  });

  test('a taken or invalid hostname is reported at the input', async function (assert) {
    fakes.api.fail('POST', '/api/apps/blog/domains', 409, 'hostname_taken');

    await visit('/apps/blog/domains');
    await fillIn('[data-test-domain-hostname]', 'www.example.org');
    await click('[data-test-domain-add]');

    assert
      .dom('[data-test-domain-hostname-error]')
      .hasText('このホスト名はすでに登録されています。');
    assert.dom('[data-test-domain-hostname]').hasValue('www.example.org');

    fakes.api.fail('POST', '/api/apps/blog/domains', 422, 'validation_failed', {
      hostname: 'must not be under the base domain',
    });
    await fillIn('[data-test-domain-hostname]', 'x.dev.example.com');
    await click('[data-test-domain-add]');

    assert
      .dom('[data-test-domain-hostname-error]')
      .hasText('must not be under the base domain');
  });

  test('a failed action shows its error and keeps the list', async function (assert) {
    domains = [makeDomain()];
    fakes.api.fail('PATCH', '/api/apps/blog/domains/3', 500, 'internal');

    await visit('/apps/blog/domains');
    await click('[data-test-domain-force-toggle]');

    assert
      .dom('[data-test-error]')
      .includesText('サーバーでエラーが発生しました');
    assert.dom('[data-test-domain="www.example.org"]').exists();
  });
});
