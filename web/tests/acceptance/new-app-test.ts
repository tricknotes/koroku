import { module, test } from 'qunit';
import { click, currentURL, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  apiError,
  installFakes,
  makeApp,
  type FakeCall,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';

function fieldError(name: string): string {
  return `[data-test-field-group="${name}"] [data-test-field-error]`;
}

module('Acceptance | create app', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    fakes.api
      .on('GET', '/api/repos', {
        repos: [
          {
            id: 1,
            full_name: 'octo/blog',
            private: true,
            default_branch: 'main',
          },
          {
            id: 2,
            full_name: 'octo/Docs.Site',
            private: false,
            default_branch: 'trunk',
          },
        ],
      })
      .on('GET', '/api/repos/octo/blog/compose', {
        files: ['compose.yaml'],
        path: 'compose.yaml',
        services: [
          { name: 'db', ports: [5432] },
          { name: 'web', ports: [3000, 9229] },
        ],
      })
      // The page the app lands on after creation.
      .on('GET', '/api/apps/blog-prod', makeApp({ slug: 'blog-prod' }))
      .on('GET', '/api/apps/blog-prod/deployments', { deployments: [] })
      .on('GET', '/api/repos/octo/blog/branches', { branches: [] })
      .on('GET', '/api/repos/octo/blog/commits', { commits: [] });
  });

  test('repository → compose detection → service and port → create', async function (assert) {
    fakes.api.on('POST', '/api/apps', (call: FakeCall) =>
      makeApp(call.body as { slug: string }),
    );

    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');

    assert.deepEqual(
      fakes.api.callsTo('GET', '/api/repos/octo/blog/compose')[0]?.query,
      { ref: 'main' },
      'compose files are detected on the default branch',
    );
    assert.dom('[data-test-compose-path]').includesText('compose.yaml');
    assert.dom('[data-test-field="default_branch"]').hasValue('main');
    assert
      .dom('[data-test-field="web_service"]')
      .hasValue('web', 'the web service is preselected');
    assert
      .dom('[data-test-field="web_port"]')
      .hasValue('3000', 'its first port is prefilled');
    assert
      .dom('[data-test-field="slug"]')
      .hasValue('blog', 'the slug is suggested from the repository name');

    await fillIn('[data-test-field="web_service"]', 'db');
    assert.dom('[data-test-field="web_port"]').hasValue('5432');
    await fillIn('[data-test-field="web_service"]', 'web');

    await fillIn('[data-test-field="slug"]', 'blog-prod');
    await click('[data-test-environment-option="production"]');
    await click('[data-test-submit]');

    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps')[0]?.body, {
      slug: 'blog-prod',
      web_port: 3000,
      environment: 'production',
      repo_full_name: 'octo/blog',
      default_branch: 'main',
      web_service: 'web',
      healthcheck_path: '/',
    });
    assert.strictEqual(currentURL(), '/apps/blog-prod/deploy');
  });

  test('field errors of a 422 are shown next to the fields', async function (assert) {
    fakes.api.on('POST', '/api/apps', () => {
      throw apiError(422, 'validation_failed', {
        web_port: 'must be between 1 and 65535',
        healthcheck_path: 'must start with /',
        mystery: 'is not understood',
      });
    });

    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');
    await click('[data-test-submit]');

    assert.strictEqual(currentURL(), '/apps/new');
    assert.dom(fieldError('web_port')).hasText('must be between 1 and 65535');
    assert.dom(fieldError('healthcheck_path')).hasText('must start with /');
    assert
      .dom('[data-test-field="web_port"]')
      .hasAttribute('aria-invalid', 'true');
    assert.dom('[data-test-error]').hasText('入力内容に誤りがあります。');
    assert
      .dom('[data-test-other-errors]')
      .hasText('mystery: is not understood');
  });

  test('a taken slug is reported on the slug field', async function (assert) {
    fakes.api.fail('POST', '/api/apps', 409, 'slug_taken');

    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');
    await click('[data-test-submit]');

    assert
      .dom(fieldError('slug'))
      .hasText('この slug はすでに使われています。');
  });

  test('obvious mistakes are caught before calling the API', async function (assert) {
    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');
    await fillIn('[data-test-field="slug"]', 'Blog_1');
    await fillIn('[data-test-field="web_port"]', '70000');
    await click('[data-test-submit]');

    assert.dom(fieldError('slug')).exists();
    assert.dom(fieldError('web_port')).exists();
    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps'), []);

    // `/apps/new` is the create page itself.
    await fillIn('[data-test-field="slug"]', 'new');
    await fillIn('[data-test-field="web_port"]', '3000');
    await click('[data-test-submit]');

    assert.dom(fieldError('slug')).includesText('使えません');
    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps'), []);
  });

  test('service and port are typed by hand when none could be read', async function (assert) {
    fakes.api
      .on('GET', '/api/repos/octo/Docs.Site/compose', (call: FakeCall) => ({
        files: ['docker-compose.yml', 'compose.yaml'],
        path: call.query['path'] ?? 'docker-compose.yml',
        services: [],
      }))
      .on('POST', '/api/apps', (call: FakeCall) =>
        makeApp(call.body as { slug: string }),
      )
      .on('GET', '/api/apps/docs-site', makeApp({ slug: 'docs-site' }));

    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/Docs.Site');

    assert.dom('[data-test-compose-manual]').exists();
    assert.dom('[data-test-field="slug"]').hasValue('docs-site');
    assert.dom('[data-test-field="default_branch"]').hasValue('trunk');
    assert.dom('input[data-test-field="web_service"]').hasValue('');
    assert.dom('[data-test-field="web_port"]').hasValue('');
    assert
      .dom('[data-test-field="compose_path"]')
      .hasValue('docker-compose.yml', 'several candidates can be chosen from');

    await fillIn('[data-test-field="compose_path"]', 'compose.yaml');
    assert.strictEqual(
      fakes.api.callsTo('GET', '/api/repos/octo/Docs.Site/compose')[1]?.query[
        'path'
      ],
      'compose.yaml',
    );

    await fillIn('[data-test-field="web_service"]', 'site');
    await fillIn('[data-test-field="web_port"]', '8080');
    await click('[data-test-submit]');

    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps')[0]?.body, {
      slug: 'docs-site',
      web_port: 8080,
      environment: 'staging',
      repo_full_name: 'octo/Docs.Site',
      default_branch: 'trunk',
      compose_path: 'compose.yaml',
      web_service: 'site',
      healthcheck_path: '/',
    });
  });

  test('nothing can be submitted before a repository is chosen', async function (assert) {
    await visit('/apps/new');

    assert.dom('[data-test-field="repo_full_name"]').hasValue('');
    assert.dom('[data-test-submit]').doesNotExist();
    assert.dom('[data-test-field="slug"]').doesNotExist();

    await click('[data-test-cancel]');
    assert.strictEqual(currentURL(), '/');
  });

  test('a failing repository list offers a retry', async function (assert) {
    fakes.api.fail('GET', '/api/repos', 502, 'github_error');

    await visit('/apps/new');

    assert
      .dom('[data-test-error]')
      .includesText('リポジトリの一覧を取得できませんでした')
      .includesText('GitHub との通信に失敗しました');
    assert.dom('[data-test-field="repo_full_name"]').doesNotExist();

    fakes.api.on('GET', '/api/repos', {
      repos: [
        {
          id: 1,
          full_name: 'octo/blog',
          private: true,
          default_branch: 'main',
        },
      ],
    });
    await click('[data-test-error] button');

    assert.dom('[data-test-field="repo_full_name"]').exists();
    assert.dom('[data-test-error]').doesNotExist();
  });

  test('a failing or empty compose detection is explained', async function (assert) {
    fakes.api.fail(
      'GET',
      '/api/repos/octo/blog/compose',
      404,
      'repo_not_found',
    );

    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');

    assert
      .dom('[data-test-compose-error]')
      .includesText('Compose ファイルを検出できませんでした');
    assert
      .dom('input[data-test-field="web_service"]')
      .exists('service and port can still be typed in');

    fakes.api.on('GET', '/api/repos/octo/Docs.Site/compose', {
      files: [],
      path: '',
      services: [],
    });
    await fillIn('[data-test-field="repo_full_name"]', 'octo/Docs.Site');

    assert.dom('[data-test-compose-error]').doesNotExist();
    assert.dom('[data-test-compose-missing]').includesText('見つかりません');
    assert.dom('[data-test-compose-manual]').exists();
  });

  test('without the GitHub App a repository URL is asked for', async function (assert) {
    fakes.api
      .fail('GET', '/api/repos', 503, 'not_configured')
      .on('POST', '/api/apps', (call: FakeCall) =>
        makeApp({ ...(call.body as { slug: string }), repo_full_name: '' }),
      )
      .on('GET', '/api/apps/sample', makeApp({ slug: 'sample' }));

    await visit('/apps/new');

    assert
      .dom('[data-test-unavailable="not_configured"]')
      .includesText('GitHub 連携がこのサーバーに設定されていない');
    assert.dom('[data-test-field="repo_full_name"]').doesNotExist();

    await fillIn(
      '[data-test-field="repo_url"]',
      'https://github.com/octo/sample.git',
    );
    assert.dom('[data-test-field="slug"]').hasValue('sample');

    await fillIn('[data-test-field="web_port"]', '8080');
    await click('[data-test-submit]');

    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps')[0]?.body, {
      slug: 'sample',
      web_port: 8080,
      environment: 'staging',
      repo_url: 'https://github.com/octo/sample.git',
      healthcheck_path: '/',
    });
  });
});
