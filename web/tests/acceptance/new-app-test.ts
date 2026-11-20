import { module, test } from 'qunit';
import {
  click,
  currentURL,
  fillIn,
  triggerEvent,
  visit,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  apiError,
  installFakes,
  makeApp,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';

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

  test('environment presets are prefilled, editable and created with the app', async function (assert) {
    let secret = 0;
    fakes.api
      .on('GET', '/api/repos/octo/blog/env-presets', () => ({
        status: 'ok',
        reason: '',
        compose_path: 'compose.yaml',
        frameworks: ['rails'],
        presets: [
          {
            key: 'RAILS_ENV',
            value: 'production',
            generated: false,
            framework: 'rails',
            file: 'Gemfile',
            match: 'rails',
          },
          {
            key: 'SECRET_KEY_BASE',
            value: `generated-${++secret}`,
            generated: true,
            framework: 'rails',
            file: 'Gemfile',
            match: 'rails',
          },
          {
            key: 'RAILS_LOG_TO_STDOUT',
            value: 'true',
            generated: false,
            framework: 'rails',
            file: 'config/environments/production.rb',
            match: 'RAILS_LOG_TO_STDOUT',
          },
        ],
        skipped: [{ key: 'RACK_ENV', service: 'web' }],
      }))
      .on('POST', '/api/apps', (call: FakeCall) =>
        makeApp(call.body as { slug: string }),
      );

    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');

    assert.deepEqual(
      fakes.api.callsTo('GET', '/api/repos/octo/blog/env-presets')[0]?.query,
      { ref: 'main' },
    );
    assert.dom('[data-test-env-presets-frameworks]').hasText('Rails');
    assert.dom('[data-test-env-draft-row]').exists({ count: 3 });
    const secretRow = '[data-test-env-draft-row="SECRET_KEY_BASE"]';
    assert
      .dom(`${secretRow} [data-test-env-draft-masked]`)
      .exists('a generated secret is masked');
    assert.dom(`${secretRow} [data-test-env-draft-value]`).doesNotExist();
    assert.dom(`${secretRow} [data-test-env-draft-generated]`).exists();
    assert
      .dom(`${secretRow} [data-test-env-draft-reason]`)
      .hasText('Gemfile に rails があるため');
    assert
      .dom('[data-test-env-presets-skipped]')
      .includesText('RACK_ENV')
      .includesText('web');
    assert.dom('[data-test-env-presets-reauth]').doesNotExist();

    await click(`${secretRow} [data-test-env-draft-reveal]`);
    assert
      .dom(`${secretRow} [data-test-env-draft-value]`)
      .hasValue('generated-1');

    await fillIn(
      '[data-test-env-draft-row="RAILS_ENV"] [data-test-env-draft-value]',
      'staging',
    );
    await click(
      '[data-test-env-draft-row="RAILS_LOG_TO_STDOUT"] [data-test-env-draft-remove]',
    );
    await click('[data-test-env-draft-add]');
    await fillIn(
      '[data-test-env-draft-row=""] [data-test-env-draft-key]',
      'TZ',
    );
    await fillIn(
      '[data-test-env-draft-row="TZ"] [data-test-env-draft-value]',
      'Asia/Tokyo',
    );

    // Another branch: untouched suggestions are read again, edits stay.
    await fillIn('[data-test-field="default_branch"]', 'next');
    await triggerEvent('[data-test-field="default_branch"]', 'change');
    assert.deepEqual(
      fakes.api.callsTo('GET', '/api/repos/octo/blog/env-presets')[1]?.query,
      { ref: 'next' },
    );
    assert
      .dom('[data-test-env-draft-row="RAILS_LOG_TO_STDOUT"]')
      .doesNotExist('a removed suggestion does not come back');

    await click('[data-test-submit]');

    assert.deepEqual(
      (fakes.api.callsTo('POST', '/api/apps')[0]?.body as { env: unknown }).env,
      [
        { key: 'SECRET_KEY_BASE', value: 'generated-2' },
        { key: 'RAILS_ENV', value: 'staging' },
        { key: 'TZ', value: 'Asia/Tokyo' },
      ],
    );
    assert.strictEqual(currentURL(), '/apps/blog/deploy');
  });

  test('invalid keys stop the form before the API', async function (assert) {
    fakes.api.on('GET', '/api/repos/octo/blog/env-presets', {
      status: 'ok',
      reason: '',
      compose_path: '',
      frameworks: [],
      presets: [],
      skipped: [],
    });

    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');
    assert.dom('[data-test-env-presets-none]').exists();

    await click('[data-test-env-draft-add]');
    await fillIn('[data-test-env-draft-key]', 'KOROKU_COMMIT_SHA');
    await click('[data-test-submit]');

    assert.dom('[data-test-env-draft-error]').includesText('koroku が使う');
    assert
      .dom('[data-test-env-presets-error]')
      .hasText('環境変数のキーを直してください。');
    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps'), []);
  });

  test('the presets step stays away when the repository cannot be read', async function (assert) {
    fakes.api.on('GET', '/api/repos/octo/blog/env-presets', {
      status: 'unavailable',
      reason: 'repo_unavailable',
      compose_path: '',
      frameworks: [],
      presets: [],
      skipped: [],
    });
    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');
    assert.dom('[data-test-env-step]').doesNotExist();

    // An older server without the endpoint: no error either.
    fakes.api.fail('GET', '/api/repos/octo/blog/env-presets', 404, 'not_found');
    await fillIn('[data-test-field="repo_full_name"]', '');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');
    assert.dom('[data-test-env-step]').doesNotExist();
    assert.dom('[data-test-error]').doesNotExist();
  });

  test('saving variables with the app asks for a recent login first', async function (assert) {
    fakes.api
      .on('GET', '/api/session', {
        authenticated: true,
        user: { id: 1, login: 'octo', avatar_url: '' },
        reauth_until: '2026-01-01T00:00:00Z',
      })
      .on('GET', '/api/repos/octo/blog/env-presets', {
        status: 'ok',
        reason: '',
        compose_path: '',
        frameworks: ['node'],
        presets: [
          {
            key: 'NODE_ENV',
            value: 'production',
            generated: false,
            framework: 'node',
            file: 'package.json',
            match: '',
          },
        ],
        skipped: [],
      })
      .on('POST', '/api/apps', (call: FakeCall) =>
        makeApp(call.body as { slug: string }),
      );

    await visit('/apps/new');
    await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');

    assert
      .dom('[data-test-env-presets-reauth]')
      .includesText('再ログインが必要です');
    await click('[data-test-env-presets-reauth-button]');
    assert.deepEqual(fakes.browser.assigned, [
      '/auth/github/login?return_to=%2Fapps%2Fnew',
    ]);

    // Without variables the app is created as before.
    await click('[data-test-env-draft-remove]');
    assert.dom('[data-test-env-presets-reauth]').doesNotExist();
    await click('[data-test-submit]');
    assert.false(
      'env' in (fakes.api.callsTo('POST', '/api/apps')[0]?.body as object),
    );
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
