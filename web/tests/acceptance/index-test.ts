import { module, test } from 'qunit';
import { visit, currentURL } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';

module('Acceptance | index', function (hooks) {
  setupApplicationTest(hooks);

  test('visiting / shows the console shell', async function (assert) {
    await visit('/');

    assert.strictEqual(currentURL(), '/');
    assert.dom('h1').hasText('PaaS コンソール');
    assert.dom('[data-test-placeholder]').exists();
  });
});
