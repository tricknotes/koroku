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
import {
  FINISH_DURATION,
  SHOW_DELAY,
} from 'koroku-console/components/route-progress';

/** The app list's link to an app. */
function appLink(slug: string): Element {
  const link = findAll('[data-test-app-link]').find(
    (element) => element.textContent?.trim() === slug,
  );
  if (!link) throw new Error(`no link to ${slug}`);
  return link;
}

const pause = (ms: number) =>
  new Promise<void>((resolve) => setTimeout(resolve, ms));

// A screen change that waits for data shows a thin bar at the top, so a
// click never looks ignored; a quick one shows nothing.
module('Acceptance | route progress', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    serveConsole(fakes);
  });

  test('opening an app that loads slowly shows the bar until it is there', async function (assert) {
    await visit('/');
    const appResponse = await fakes.api.transport('GET', '/api/apps/blog');
    const held = fakes.api.hold('GET', '/api/apps/blog');

    const opening = click(appLink('blog'));
    await waitUntil(() => find('[data-test-route-progress="loading"]'), {
      timeout: SHOW_DELAY + 1000,
    });
    assert
      .dom('[data-test-route-progress]')
      .hasAttribute('role', 'progressbar')
      .hasAttribute('aria-label', '画面を読み込み中');
    assert.strictEqual(currentURL(), '/', 'still on the list while it loads');

    held.release(appResponse);
    await opening;

    assert.strictEqual(currentURL(), '/apps/blog');
    assert.dom('[data-test-route-progress="finishing"]').exists('it fills up');
    await waitUntil(() => !find('[data-test-route-progress]'), {
      timeout: FINISH_DURATION + 1000,
    });
    assert.dom('[data-test-route-progress]').doesNotExist('and goes away');
  });

  test('a quick screen change shows no bar', async function (assert) {
    await visit('/');
    await click(appLink('blog'));

    assert.strictEqual(currentURL(), '/apps/blog');
    assert.dom('[data-test-route-progress]').doesNotExist();
    await pause(SHOW_DELAY + 50);
    assert
      .dom('[data-test-route-progress]')
      .doesNotExist('nor does it show up afterwards');
  });

  test('switching tabs of a loaded app shows no bar', async function (assert) {
    await visit('/apps/blog');
    await click('[data-test-tab="deploy"]');
    await pause(SHOW_DELAY + 50);

    assert.strictEqual(currentURL(), '/apps/blog/deploy');
    assert.dom('[data-test-route-progress]').doesNotExist();
  });
});
