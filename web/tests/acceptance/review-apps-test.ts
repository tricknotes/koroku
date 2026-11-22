import { module, test } from 'qunit';
import { click, visit, waitFor } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeBackups,
  makeDeployment,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, PullRequest, ReviewInfo } from 'koroku-console/utils/types';

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
    });
    child = makeApp({
      slug: 'blog-staging-pr-42',
      review: review(),
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

  test('the app list puts review apps under their parent', async function (assert) {
    const other = makeApp({ slug: 'alpha' });
    fakes.api.on('GET', '/api/apps', {
      apps: [other, parent, child],
    });
    await visit('/');
    const rows = [...document.querySelectorAll('[data-test-app]')].map((row) =>
      row.getAttribute('data-test-app'),
    );
    assert.deepEqual(rows, ['alpha', 'blog-staging', 'blog-staging-pr-42']);
    assert
      .dom('[data-test-app="blog-staging-pr-42"]')
      .hasClass('app-row-review');
    assert
      .dom('[data-test-app="blog-staging-pr-42"] [data-test-review-of]')
      .includesText('PR #42');
  });

  test('the parent lists its review apps and creates one', async function (assert) {
    const created = makeApp({
      slug: 'blog-staging-pr-46',
      review: review({ pr_number: 46, pr_title: 'Paginate the archive' }),
    });
    fakes.api.on('POST', '/api/apps/blog-staging/review-apps', () => ({
      ...created,
      dns_warning: true,
    }));
    await visit('/apps/blog-staging');
    await waitFor('[data-test-review-app]');
    assert
      .dom('[data-test-review-app="blog-staging-pr-42"]')
      .includesText('#42')
      .includesText('オープン');

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

  test('a parent without hooks creates without data', async function (assert) {
    parent.dump_hook_command = '';
    await visit('/apps/blog-staging');
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
    await visit('/apps/blog-staging');
    await click('[data-test-review-create]');
    await waitFor('[data-test-review-permission]');
    assert
      .dom('[data-test-review-permission]')
      .includesText('Pull requests: Read-only');
  });

  test('a production app has no review apps', async function (assert) {
    parent.environment = 'production';
    await visit('/apps/blog-staging');
    assert.dom('[data-test-review-apps]').doesNotExist();
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
    assert.dom('[data-test-review-apps]').doesNotExist();
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
