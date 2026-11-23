import { module, test } from 'qunit';
import { click, currentURL, findAll, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import { SCREENS, serveConsole } from 'koroku-console/tests/helpers/console';
import { installFakes, type Fakes } from 'koroku-console/tests/helpers/fakes';

/** `data-test-…` values of the elements marked as the current page. */
function current(attribute: string): (string | null)[] {
  return findAll(`[${attribute}][aria-current="page"]`).map((element) =>
    element.getAttribute(attribute),
  );
}

// Every screen is reached by its URL alone (a reload, a bookmark, a link
// from a notification), and the navigation says where the reader is.
module('Acceptance | navigation', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    serveConsole(fakes);
  });

  for (const screen of SCREENS.filter((entry) => entry.tab)) {
    test(`${screen.url} restores the tab and marks it as current`, async function (assert) {
      await visit(screen.url);

      assert.strictEqual(currentURL(), screen.url);
      assert.dom('[data-test-app-title]').hasText('blog');
      assert.deepEqual(
        current('data-test-tab'),
        [screen.tab ?? null],
        'exactly one tab is the current page',
      );
      assert.dom('[data-test-tab].active').exists({ count: 1 });
      assert.dom(`[data-test-tab="${screen.tab}"]`).hasClass('active');
      assert.deepEqual(
        current('data-test-nav'),
        ['apps'],
        'an app belongs to the "apps" section',
      );
      assert.true(document.title.includes('blog'), document.title);
    });
  }

  test('the top bar marks the section of every screen', async function (assert) {
    const sections: Record<string, string> = {
      '/': 'apps',
      '/apps/new': 'apps',
      '/apps/blog/settings': 'apps',
      '/system': 'system',
    };

    for (const [url, section] of Object.entries(sections)) {
      await visit(url);

      assert.deepEqual(current('data-test-nav'), [section], url);
      assert.dom('[data-test-nav].active').exists({ count: 1 });
      assert.dom(`[data-test-nav="${section}"]`).hasClass('active');
    }

    await visit('/nope');
    assert.deepEqual(
      current('data-test-nav'),
      [],
      'an unknown page belongs to neither',
    );
  });

  test('each screen names itself in the window title', async function (assert) {
    const titles: Record<string, string> = {
      '/': 'アプリ',
      '/apps/new': 'アプリを作成',
      '/apps/blog': 'blog',
      '/apps/blog/logs': 'ログ',
      '/system': 'システム',
      '/nope': 'ページが見つかりません',
    };
    const seen = new Set<string>();

    for (const [url, title] of Object.entries(titles)) {
      await visit(url);

      assert.true(document.title.includes(title), `${url}: ${document.title}`);
      assert.true(document.title.includes('koroku'), 'and the console');
      seen.add(document.title);
    }

    assert.strictEqual(seen.size, Object.keys(titles).length);
  });

  test('unknown paths below a known one are not found', async function (assert) {
    for (const url of ['/apps/blog/nope', '/system/nope', '/apps']) {
      await visit(url);

      assert.dom('[data-test-not-found]').exists(url);
      assert.dom('[data-test-app-detail]').doesNotExist();
      assert
        .dom('main a[href], main button')
        .exists({ count: 1 }, 'exactly one way on');
    }

    await click('[data-test-back-to-list]');
    assert.strictEqual(currentURL(), '/');
  });

  test('the tabs keep the app when moving between them', async function (assert) {
    await visit('/apps/blog/settings');

    const order = ['index', 'deploy', 'logs', 'resources', 'env', 'backups'];
    const urls: string[] = [];
    for (const tab of order) {
      await click(`[data-test-tab="${tab}"]`);
      urls.push(currentURL());
      assert.deepEqual(current('data-test-tab'), [tab]);
    }

    assert.deepEqual(urls, [
      '/apps/blog',
      '/apps/blog/deploy',
      '/apps/blog/logs',
      '/apps/blog/resources',
      '/apps/blog/env',
      '/apps/blog/backups',
    ]);
    assert.strictEqual(
      fakes.api.callsTo('GET', '/api/apps/blog').length,
      1,
      'the app is read once, not once per tab',
    );
  });

  test('the URLs of sections without a tab lead to their section', async function (assert) {
    const sections = [
      {
        url: '/apps/blog/review-apps',
        to: '/apps/blog/deploy?section=review-apps',
        tab: 'deploy',
        focused: '#review-apps',
      },
      {
        url: '/apps/blog/compose',
        to: '/apps/blog/resources?section=compose',
        tab: 'resources',
        focused: '#compose',
      },
      {
        url: '/apps/blog/domains',
        to: '/apps/blog/settings?section=domains',
        tab: 'settings',
        focused: '#domains',
      },
    ];
    for (const { url, to, tab, focused } of sections) {
      await visit(url);

      assert.strictEqual(currentURL(), to, url);
      assert.deepEqual(current('data-test-tab'), [tab], `${url}: the tab`);
      assert.dom(focused).isFocused(`${url}: the section has the focus`);
    }
  });

  test('a section URL on an app without that section opens the tab', async function (assert) {
    serveConsole(fakes, { environment: 'production' });
    await visit('/apps/blog/review-apps');

    assert.strictEqual(currentURL(), '/apps/blog/deploy?section=review-apps');
    assert.dom('[data-test-review-tab]').doesNotExist();
    assert.dom('[data-test-deploy-panel]').exists();
  });

  test('the log filters in the URL are read leniently', async function (assert) {
    await visit('/apps/blog/logs?period=bogus&service=web&q=GET');

    assert
      .dom('[data-test-log-period]')
      .hasValue('tail', 'an unknown period is the live tail');
    assert.dom('[data-test-log-service]').hasValue('web');
    assert.dom('[data-test-log-keyword]').hasValue('GET');
    assert.strictEqual(fakes.eventSource.streams.length, 1);
    assert.true(
      fakes.eventSource.last.url.includes('service=web'),
      'the stream is asked for that service',
    );
    assert.deepEqual(
      fakes.api.callsTo('GET', '/api/apps/blog/logs/history'),
      [],
      'and nothing is searched',
    );
  });

  test('a service in the URL that is not running can still be selected', async function (assert) {
    await visit('/apps/blog/logs?service=worker');

    assert.dom('[data-test-log-service]').hasValue('worker');
    assert.true(fakes.eventSource.last.url.includes('service=worker'));
  });

  test('the log filters do not follow the reader to another tab and back', async function (assert) {
    await visit('/apps/blog/logs?period=1h&q=GET');

    await click('[data-test-tab="env"]');
    assert.strictEqual(currentURL(), '/apps/blog/env', 'no stray parameters');

    await click('[data-test-tab="logs"]');
    assert.dom('[data-test-log-keyword]').hasValue('GET');
    assert.strictEqual(
      currentURL(),
      '/apps/blog/logs?period=1h&q=GET',
      'the tab comes back as it was left, and the URL says so',
    );
  });
});
