import { module, test } from 'qunit';
import { click, fillIn, findAll, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  installFakes,
  makeApp,
  type FakeCall,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';

const SECRET = 'postgres://app:hunter2@db/app';

module('Acceptance | environment variables', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    fakes.api
      .on('GET', '/api/apps/blog', makeApp())
      .on('GET', '/api/apps/blog/env', {
        vars: [
          { key: 'DATABASE_URL', value: SECRET },
          { key: 'RAILS_ENV', value: 'production' },
        ],
      })
      .on('PUT', '/api/apps/blog/env', (call: FakeCall) => call.body);
  });

  test('values are masked until revealed', async function (assert) {
    await visit('/apps/blog/env');

    assert.dom('[data-test-env-row]').exists({ count: 2 });
    assert.dom('[data-test-env-masked]').exists({ count: 2 });
    assert.dom('[data-test-env-value]').doesNotExist();
    assert.false(
      document.querySelector('#ember-testing')?.innerHTML.includes('hunter2'),
      'a masked value is not in the DOM at all',
    );
    assert.dom('[data-test-env-notice]').includesText('次のデプロイから');

    const [first] = findAll('[data-test-env-toggle]');
    await click(first as Element);

    assert.dom('[data-test-env-value]').exists({ count: 1 }).hasValue(SECRET);
    assert.dom('[data-test-env-masked]').exists({ count: 1 });

    await click(first as Element);
    assert.dom('[data-test-env-value]').doesNotExist();

    await click('[data-test-env-toggle-all]');
    assert.dom('[data-test-env-value]').exists({ count: 2 });
  });

  test('edits, additions and removals are saved with one PUT', async function (assert) {
    await visit('/apps/blog/env');

    // Edit the first value.
    await click('[data-test-env-row]:nth-child(1) [data-test-env-toggle]');
    await fillIn(
      '[data-test-env-row]:nth-child(1) [data-test-env-value]',
      'postgres://app:changed@db/app',
    );
    // Remove the second variable.
    await click('[data-test-env-row]:nth-child(2) [data-test-env-remove]');
    // Add a new one; an untouched extra row is ignored.
    await click('[data-test-env-add]');
    await fillIn(
      '[data-test-env-row]:nth-child(2) [data-test-env-key]',
      'SECRET_KEY_BASE',
    );
    await fillIn(
      '[data-test-env-row]:nth-child(2) [data-test-env-value]',
      'abc123',
    );
    await click('[data-test-env-add]');

    assert.dom('[data-test-env-dirty]').exists();
    assert.deepEqual(fakes.api.callsTo('PUT', '/api/apps/blog/env'), []);

    await click('[data-test-env-save]');

    const puts = fakes.api.callsTo('PUT', '/api/apps/blog/env');
    assert.strictEqual(puts.length, 1, 'everything goes in a single request');
    assert.deepEqual(puts[0]?.body, {
      vars: [
        { key: 'DATABASE_URL', value: 'postgres://app:changed@db/app' },
        { key: 'SECRET_KEY_BASE', value: 'abc123' },
      ],
    });
    assert.dom('[data-test-env-saved]').includesText('次のデプロイから反映');
    assert.dom('[data-test-env-dirty]').doesNotExist();
    assert
      .dom('[data-test-env-masked]')
      .exists({ count: 2 }, 'saved values are masked again');
  });

  test('invalid and duplicate keys block saving', async function (assert) {
    await visit('/apps/blog/env');

    await click('[data-test-env-add]');
    await fillIn(
      '[data-test-env-row]:nth-child(3) [data-test-env-key]',
      '1BAD',
    );
    assert.dom('[data-test-env-key-error]').exists({ count: 1 });
    assert.dom('[data-test-env-save]').isDisabled();

    await fillIn(
      '[data-test-env-row]:nth-child(3) [data-test-env-key]',
      'RAILS_ENV',
    );
    assert.dom('[data-test-env-key-error]').hasText('キーが重複しています。');
    assert.dom('[data-test-env-save]').isDisabled();

    await fillIn(
      '[data-test-env-row]:nth-child(3) [data-test-env-key]',
      'OK_1',
    );
    assert.dom('[data-test-env-key-error]').doesNotExist();
    assert.dom('[data-test-env-save]').isNotDisabled();
  });

  test('a rejected save keeps the edits and shows the error', async function (assert) {
    fakes.api.fail('PUT', '/api/apps/blog/env', 422, 'validation_failed', {
      vars: 'duplicate key',
    });

    await visit('/apps/blog/env');
    await click('[data-test-env-add]');
    await fillIn('[data-test-env-row]:nth-child(3) [data-test-env-key]', 'NEW');
    await click('[data-test-env-save]');

    assert.dom('[data-test-error]').hasText('入力内容に誤りがあります。');
    assert.dom('[data-test-env-server-errors]').hasText('vars: duplicate key');
    assert.dom('[data-test-env-row]').exists({ count: 3 });
    assert.dom('[data-test-env-saved]').doesNotExist();
  });
});
