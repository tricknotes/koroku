import { module, test } from 'qunit';
import { click, currentURL, visit, waitFor } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeBackups,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type {
  App,
  AppRole,
  PullRequest,
  ReviewInfo,
} from 'koroku-console/utils/types';

const MEMBER_SESSION = {
  authenticated: true,
  user: { id: 2, login: 'mia', avatar_url: '', owner: false },
};

function review(overrides: Partial<ReviewInfo> = {}): ReviewInfo {
  return {
    parent: 'blog-staging',
    pr_number: 42,
    pr_title: 'Add search',
    pr_url: 'https://github.com/octo/blog/pull/42',
    pr_state: 'open',
    head_branch: 'search',
    data_copy: 'waiting',
    data_copy_detail: '',
    data_copy_status: '',
    ...overrides,
  };
}

function pull(overrides: Partial<PullRequest> = {}): PullRequest {
  return {
    number: 46,
    title: 'Paginate the archive',
    head_branch: 'archive',
    head_sha: 'a'.repeat(40),
    author: 'mona',
    url: 'https://github.com/octo/blog/pull/46',
    updated_at: '2026-11-18T02:00:00Z',
    from_fork: false,
    review_app: null,
    ...overrides,
  };
}

module('Acceptance | review apps', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let parent: App;
  let child: App;

  function serve(app: App) {
    fakes.api
      .on('GET', `/api/apps/${app.slug}`, () => app)
      .on('GET', `/api/apps/${app.slug}/services`, { services: [] })
      .on('GET', `/api/apps/${app.slug}/deployments`, { deployments: [] })
      .on('GET', `/api/apps/${app.slug}/domains`, {
        default_hostname: `${app.slug}.dev.example.com`,
        domains: [],
      })
      .on('GET', `/api/apps/${app.slug}/backups`, { snapshots: [] })
      .on('GET', `/api/apps/${app.slug}/db-copies`, { copies: [] })
      .on('GET', `/api/apps/${app.slug}/activity`, {
        entries: [],
        next_before: null,
      });
  }

  function asMember(role: AppRole) {
    fakes.api.on('GET', '/api/session', MEMBER_SESSION);
    parent.role = role;
    child.role = role;
  }

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const running = makeDeployment();
    parent = makeApp({
      slug: 'blog-staging',
      current_deployment: running,
      latest_deployment: running,
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump',
      load_hook_service: 'db',
      load_hook_command: 'pg_restore',
      review_apps_allowed: true,
      review_apps: { count: 1, failed: 0, deploying: 0 },
    });
    child = makeApp({
      slug: 'blog-staging-pr-42',
      review: review(),
      review_apps: null,
    });
    serve(parent);
    serve(child);
    fakes.api
      .on('GET', '/api/backups', makeBackups())
      .on('GET', '/api/apps/blog-staging/review-apps', () => ({
        review_apps: [child],
      }))
      .on('GET', '/api/apps/blog-staging/pull-requests', {
        pull_requests: [
          pull(),
          pull({ number: 45, from_fork: true, title: 'Typo' }),
          pull({ number: 42, review_app: 'blog-staging-pr-42' }),
        ],
      });
  });

  test('the app list counts review apps on their parent', async function (assert) {
    const other = makeApp({
      slug: 'alpha',
      review_apps: { count: 0, failed: 0, deploying: 0 },
    });
    parent.review_apps = { count: 3, failed: 1, deploying: 1 };
    fakes.api.on('GET', '/api/apps', {
      apps: [other, parent, child],
    });
    await visit('/');
    const rows = [...document.querySelectorAll('[data-test-app]')].map((row) =>
      row.getAttribute('data-test-app'),
    );
    assert.deepEqual(rows, ['alpha', 'blog-staging'], 'no row of its own');
    assert
      .dom('[data-test-app="blog-staging"] [data-test-review-count-link]')
      .hasText('レビューアプリ 3')
      .hasAttribute('href', '/apps/blog-staging/deploy?section=review-apps');
    assert
      .dom('[data-test-app="blog-staging"] [data-test-review-count-failed]')
      .hasText('1 件失敗');
    assert
      .dom('[data-test-app="blog-staging"] [data-test-review-count-deploying]')
      .hasText('1 件デプロイ中');
    assert
      .dom('[data-test-app="alpha"] [data-test-review-count]')
      .doesNotExist();

    // Nothing abnormal: the count alone.
    parent.review_apps = { count: 2, failed: 0, deploying: 0 };
    await visit('/apps/blog-staging');
    await visit('/');
    assert
      .dom('[data-test-app="blog-staging"] [data-test-review-count]')
      .hasText('レビューアプリ 2');

    await click('[data-test-app="blog-staging"] [data-test-review-count-link]');
    assert.strictEqual(
      currentURL(),
      '/apps/blog-staging/deploy?section=review-apps',
    );
  });

  test('the tab lists review apps and creates one', async function (assert) {
    child.review = review({ data_copy: 'started', data_copy_status: 'failed' });
    const created = makeApp({
      slug: 'blog-staging-pr-46',
      review: review({ pr_number: 46, pr_title: 'Paginate the archive' }),
    });
    fakes.api.on('POST', '/api/apps/blog-staging/review-apps', () => ({
      ...created,
      dns_warning: true,
    }));
    await visit('/apps/blog-staging/review-apps');
    assert.strictEqual(
      currentURL(),
      '/apps/blog-staging/deploy?section=review-apps',
    );
    assert.dom('[data-test-tab="deploy"]').hasClass('active');
    assert
      .dom('[data-test-review-tab]')
      .exists('the review apps follow the deploy history');
    assert.dom('#review-apps').isFocused();
    await waitFor('[data-test-review-app]');
    assert
      .dom('[data-test-review-app="blog-staging-pr-42"]')
      .includesText('#42')
      .includesText('オープン')
      .includesText('blog-staging-pr-42.dev.example.com');
    assert
      .dom(
        '[data-test-review-app="blog-staging-pr-42"] [data-test-review-row-copy]',
      )
      .includesText('コピーに失敗');
    assert.dom('[data-test-review-allow]').hasAttribute('aria-checked', 'true');

    await click('[data-test-review-create]');
    await waitFor('[data-test-review-pulls]');
    assert.dom('[data-test-review-pull="45"] input').isDisabled();
    assert.dom('[data-test-review-pull="42"] input').isDisabled();
    assert.dom('[data-test-review-pull="45"]').includesText('fork');
    assert.dom('[data-test-dialog-confirm]').isDisabled();
    assert.dom('[data-test-review-copy]').isChecked().isEnabled();

    await click('[data-test-review-pull="46"] input');
    await click('[data-test-dialog-confirm]');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog-staging/review-apps')[0]?.body,
      { pr_number: 46, copy_data: true },
    );
    assert.dom('[data-test-review-created]').includesText('blog-staging-pr-46');
    assert
      .dom('[data-test-review-dns-warning]')
      .includesText('blog-staging-pr-46.dev.example.com');
  });

  test('the owner allows review apps after a confirmation', async function (assert) {
    parent.review_apps_allowed = false;
    parent.review_apps = { count: 0, failed: 0, deploying: 0 };
    fakes.api
      .on('GET', '/api/apps/blog-staging/review-apps', { review_apps: [] })
      .on('PATCH', '/api/apps/blog-staging', ({ body }: FakeCall) => {
        Object.assign(parent, body as Partial<App>);
        return { ...parent };
      });
    await visit('/apps/blog-staging/review-apps');
    assert
      .dom('[data-test-review-allow]')
      .hasAttribute('aria-checked', 'false');
    assert.dom('[data-test-review-create]').isDisabled();
    assert.dom('[data-test-review-empty]').exists();

    await click('[data-test-review-allow]');
    assert
      .dom('[data-test-review-allow-confirm]')
      .includesText('環境変数（値を含む）とデータ');
    assert.strictEqual(
      fakes.api.callsTo('PATCH', '/api/apps/blog-staging').length,
      0,
      'nothing is saved before the confirmation',
    );
    await click('[data-test-dialog-confirm]');
    assert.deepEqual(
      fakes.api.callsTo('PATCH', '/api/apps/blog-staging')[0]?.body,
      { review_apps_allowed: true },
    );
    assert.dom('[data-test-review-allow]').hasAttribute('aria-checked', 'true');
    assert.dom('[data-test-review-create]').isEnabled();

    // Turning it off asks nothing and keeps the review apps.
    await click('[data-test-review-allow]');
    assert.deepEqual(
      fakes.api.callsTo('PATCH', '/api/apps/blog-staging')[1]?.body,
      { review_apps_allowed: false },
    );
    assert
      .dom('[data-test-review-allow]')
      .hasAttribute('aria-checked', 'false');
  });

  test('existing review apps stay when they are no longer allowed', async function (assert) {
    parent.review_apps_allowed = false;
    await visit('/apps/blog-staging/review-apps');
    await waitFor('[data-test-review-app]');
    assert.dom('[data-test-review-app="blog-staging-pr-42"]').exists();
    assert.dom('[data-test-review-create]').isDisabled();
  });

  test('a member sees the switch and is told the owner allows', async function (assert) {
    asMember('manage');
    parent.review_apps_allowed = false;
    parent.review_apps = { count: 0, failed: 0, deploying: 0 };
    fakes.api.on('GET', '/api/apps/blog-staging/review-apps', {
      review_apps: [],
    });
    await visit('/apps/blog-staging/review-apps');
    assert.dom('[data-test-review-allow]').doesNotExist();
    assert
      .dom('[data-test-review-allowed-state]')
      .includesText('許可されていません');
    assert.dom('[data-test-review-owner-needed]').includesText('オーナー');
    assert.dom('[data-test-review-create]').isDisabled();
  });

  test('a member with a role on the parent reaches its review apps', async function (assert) {
    asMember('view');
    fakes.api.on('GET', '/api/apps', { apps: [parent, child] });
    await visit('/');
    await click('[data-test-app="blog-staging"] [data-test-review-count-link]');
    await waitFor('[data-test-review-app]');
    assert
      .dom('[data-test-review-allowed-state]')
      .includesText('許可されています');
    assert
      .dom('[data-test-review-create]')
      .doesNotExist('viewers do not create');
    await click('[data-test-review-app="blog-staging-pr-42"] a.row-link');
    assert.strictEqual(currentURL(), '/apps/blog-staging-pr-42');
  });

  test('the overview has one line about the review apps', async function (assert) {
    parent.review_apps = { count: 3, failed: 1, deploying: 0 };
    await visit('/apps/blog-staging');
    assert.dom('[data-test-review-line-count]').hasText('3 件');
    assert.dom('[data-test-review-line-failed]').hasText('1 件失敗');
    assert
      .dom('[data-test-review-app]')
      .doesNotExist('no list on the overview');
    await click('[data-test-review-line-link]');
    assert.strictEqual(
      currentURL(),
      '/apps/blog-staging/deploy?section=review-apps',
    );
  });

  test('the overview line is hidden when not allowed and there are none', async function (assert) {
    parent.review_apps_allowed = false;
    parent.review_apps = { count: 0, failed: 0, deploying: 0 };
    await visit('/apps/blog-staging');
    assert.dom('[data-test-review-line]').doesNotExist();
    await visit('/apps/blog-staging/deploy');
    assert.dom('[data-test-review-tab]').exists('the section stays');
  });

  test('the overview line says when there are none yet', async function (assert) {
    parent.review_apps = { count: 0, failed: 0, deploying: 0 };
    await visit('/apps/blog-staging');
    assert.dom('[data-test-review-line-empty]').exists();
  });

  test('a parent without hooks creates without data', async function (assert) {
    parent.dump_hook_command = '';
    await visit('/apps/blog-staging/review-apps');
    await click('[data-test-review-create]');
    assert.dom('[data-test-review-copy]').isNotChecked().isDisabled();
    assert.dom('[data-test-review-copy-hint]').includesText('ダンプフック');
  });

  test('a missing GitHub permission is explained', async function (assert) {
    fakes.api.fail(
      'GET',
      '/api/apps/blog-staging/pull-requests',
      502,
      'github_permission_missing',
    );
    await visit('/apps/blog-staging/review-apps');
    await click('[data-test-review-create]');
    await waitFor('[data-test-review-permission]');
    assert
      .dom('[data-test-review-permission]')
      .includesText('Pull requests: Read-only');
  });

  test('a production app has no review apps section', async function (assert) {
    parent.environment = 'production';
    parent.review_apps = { count: 0, failed: 0, deploying: 0 };
    await visit('/apps/blog-staging');
    assert.dom('[data-test-review-line]').doesNotExist();
    await visit('/apps/blog-staging/review-apps');
    assert.strictEqual(
      currentURL(),
      '/apps/blog-staging/deploy?section=review-apps',
    );
    assert.dom('[data-test-review-tab]').doesNotExist();
  });

  test('a review app links to its parent on every tab', async function (assert) {
    for (const url of [
      '/apps/blog-staging-pr-42',
      '/apps/blog-staging-pr-42/deploy',
      '/apps/blog-staging-pr-42/settings',
    ]) {
      await visit(url);
      assert
        .dom('[data-test-app-parent]')
        .includesText('blog-staging')
        .includesText('PR #42');
      if (url.endsWith('/deploy')) {
        assert.dom('[data-test-review-tab]').doesNotExist(url);
      }
    }
    await click('[data-test-app-parent-link]');
    assert.strictEqual(
      currentURL(),
      '/apps/blog-staging/deploy?section=review-apps',
    );
  });

  test('a review app shows its pull request and data', async function (assert) {
    child.review = review({
      pr_state: 'merged',
      data_copy: 'skipped',
      data_copy_detail: 'no_load_hook',
    });
    await visit('/apps/blog-staging-pr-42');
    assert
      .dom('[data-test-review-summary]')
      .includesText('#42 Add search')
      .includesText('マージ済み')
      .includesText('ロードフックがありません');
    assert.dom('[data-test-review-parent]').hasText('blog-staging');
    assert.dom('[data-test-review-finished]').exists();
    assert.dom('[data-test-review-line]').doesNotExist();
  });

  test('a review app follows the copy that was started', async function (assert) {
    child.review = review({ data_copy: 'started' });
    fakes.api.on('GET', '/api/apps/blog-staging-pr-42/db-copies', {
      copies: [
        {
          id: 3,
          target_app: 'blog-staging-pr-42',
          source_app: 'blog-staging',
          source_kind: 'live',
          status: 'failed',
          error: 'the load command failed',
          bytes: 0,
          backup_snapshot_id: '',
          started_at: '2026-11-18T03:00:00Z',
          finished_at: '2026-11-18T03:01:00Z',
          actor: null,
        },
      ],
    });
    await visit('/apps/blog-staging-pr-42');
    await waitFor('[data-test-review-copy-state]');
    assert
      .dom('[data-test-review-summary]')
      .includesText('コピーに失敗')
      .includesText('the load command failed');
  });
});
