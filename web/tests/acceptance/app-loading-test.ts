import { module, test } from 'qunit';
import {
  click,
  currentURL,
  find,
  findAll,
  visit,
  waitUntil,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import { serveConsole } from 'koroku-console/tests/helpers/console';
import { installFakes, type Fakes } from 'koroku-console/tests/helpers/fakes';

/** The app list's link to an app. */
function appLink(slug: string): Element {
  const link = findAll('[data-test-app-link]').find(
    (element) => element.textContent?.trim() === slug,
  );
  if (!link) throw new Error(`no link to ${slug}`);
  return link;
}

// Opening an app waits for it (routes/app.ts). Meanwhile the router shows
// the loading substate, shaped like the app detail, so a click never looks
// ignored.
module('Acceptance | app loading', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    serveConsole(fakes);
  });

  test('opening an app shows its shape until it has loaded', async function (assert) {
    await visit('/');
    const app = await fakes.api.transport('GET', '/api/apps/blog');
    const held = fakes.api.hold('GET', '/api/apps/blog');

    const opening = click(appLink('blog'));
    await waitUntil(() => find('[data-test-app-loading]'));

    assert
      .dom('[data-test-app-loading]')
      .hasAttribute('role', 'status')
      .hasAttribute('aria-label', 'アプリを読み込み中');
    assert.dom('.route-progress').exists('with the bar along the top');
    assert.dom('[data-test-app-link]').doesNotExist('the list has made way');

    held.release(app);
    await opening;

    assert.strictEqual(currentURL(), '/apps/blog');
    assert.dom('[data-test-app-title]').hasText('blog');
    assert.dom('[data-test-app-loading]').doesNotExist();
    assert.dom('.route-progress').doesNotExist();
  });

  test('switching tabs of a loaded app does not show it', async function (assert) {
    await visit('/apps/blog');
    await click('[data-test-tab="deploy"]');

    assert.strictEqual(currentURL(), '/apps/blog/deploy');
    assert.dom('[data-test-app-loading]').doesNotExist();
  });
});
