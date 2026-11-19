import { module, test } from 'qunit';
import { click, fillIn, findAll, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  SHA_A,
  SHA_B,
  installFakes,
  makeApp,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type {
  App,
  EnvRequirement,
  EnvRequirements,
} from 'koroku-console/utils/types';

const ENV = '/api/apps/blog/env';
const REQUIREMENTS = '/api/apps/blog/env/requirements';

type Var = { key: string; value: string };

function variable(
  key: string,
  overrides: Partial<EnvRequirement> = {},
): Omit<EnvRequirement, 'set'> {
  return {
    key,
    required: true,
    default: null,
    description: '',
    ...overrides,
  };
}

/** The API's comparison of `variables` with what is saved. */
function requirements(
  saved: Var[],
  variables: Omit<EnvRequirement, 'set'>[],
  overrides: Partial<EnvRequirements> = {},
): EnvRequirements {
  const keys = new Set(saved.map((v) => v.key));
  const listed = new Set(variables.map((v) => v.key));
  return {
    source: 'compose',
    status: 'ok',
    reason: '',
    ref: 'main',
    commit_sha: SHA_A,
    path: 'compose.yaml',
    variables: variables.map((v) => ({ ...v, set: keys.has(v.key) })),
    unlisted: [...keys].filter((key) => !listed.has(key)),
    ...overrides,
  };
}

function missingKeys(): string[] {
  return findAll('[data-test-env-req]').map(
    (item) => item.getAttribute('data-test-env-req') ?? '',
  );
}

module('Acceptance | environment variable requirements', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let saved: Var[];

  function list() {
    return {
      vars: saved.map((v) => ({ ...v, pending: null })),
      removed: [],
      compared: true,
      values_hidden: false,
    };
  }

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const running = makeDeployment();
    app = makeApp({ current_deployment: running, latest_deployment: running });
    saved = [{ key: 'DATABASE_URL', value: 'postgres://db/app' }];
    fakes.api
      .on('GET', '/api/apps/blog', () => app)
      .on('GET', ENV, () => list())
      .on('PUT', `${ENV}/SMTP_HOST`, (call: FakeCall) => {
        const { value } = call.body as { value: string };
        saved = [...saved, { key: 'SMTP_HOST', value }];
        return list();
      });
  });

  module('environment tab', function () {
    test('missing variables are listed, required first, and can be added', async function (assert) {
      fakes.api.on('GET', REQUIREMENTS, () =>
        requirements(saved, [
          variable('DATABASE_URL'),
          variable('LOG_LEVEL', { required: false, default: 'info' }),
          variable('SMTP_HOST', { description: 'Outgoing mail server' }),
        ]),
      );
      await visit('/apps/blog/env');

      assert.deepEqual(
        fakes.api.callsTo('GET', REQUIREMENTS)[0]?.query,
        {},
        'the default branch is compared',
      );
      assert
        .dom('[data-test-env-reqs-missing]')
        .includesText('必須の環境変数が 1 件未設定です')
        .includesText('main（9fceb02）')
        .includesText('compose.yaml');
      assert.deepEqual(missingKeys(), ['SMTP_HOST', 'LOG_LEVEL']);
      assert
        .dom('[data-test-env-req="SMTP_HOST"] [data-test-env-req-required]')
        .hasText('必須');
      assert
        .dom('[data-test-env-req="SMTP_HOST"]')
        .includesText('Outgoing mail server');
      assert
        .dom('[data-test-env-req="LOG_LEVEL"]')
        .includesText('任意')
        .includesText('既定値: info');

      await click('[data-test-env-req="SMTP_HOST"] [data-test-env-req-add]');
      assert
        .dom('[data-test-env-row]:last-child [data-test-env-key-input]')
        .hasValue('SMTP_HOST', 'the key is filled in');
      assert
        .dom('[data-test-env-row]:last-child [data-test-env-value-input]')
        .isFocused();

      await fillIn(
        '[data-test-env-row]:last-child [data-test-env-value-input]',
        'smtp.example.com',
      );
      await click('[data-test-env-row]:last-child [data-test-env-save]');

      assert.deepEqual(missingKeys(), ['LOG_LEVEL']);
      assert
        .dom('[data-test-env-reqs-missing]')
        .includesText('任意の環境変数が 1 件未設定です');
      assert.strictEqual(
        fakes.api.callsTo('GET', REQUIREMENTS).length,
        1,
        'saving compares with the rows on screen, without a new request',
      );
    });

    test('everything set is one quiet line', async function (assert) {
      fakes.api.on('GET', REQUIREMENTS, () =>
        requirements(saved, [variable('DATABASE_URL')]),
      );
      await visit('/apps/blog/env');

      assert.dom('[data-test-env-reqs-missing]').doesNotExist();
      assert
        .dom('[data-test-env-reqs-ok]')
        .includesText('1 件の変数は、すべて設定済みです');
    });

    test('in .env.example mode, saved keys it does not list are marked', async function (assert) {
      saved = [...saved, { key: 'OLD_FLAG', value: '1' }];
      fakes.api.on('GET', REQUIREMENTS, () =>
        requirements(saved, [variable('DATABASE_URL')], {
          source: 'dotenv',
          path: '.env.example',
        }),
      );
      await visit('/apps/blog/env');

      assert.dom('[data-test-env-unlisted]').exists({ count: 1 });
      assert
        .dom('[data-test-env-row]:nth-child(2) [data-test-env-unlisted]')
        .hasText('例にない');
      assert.dom('[data-test-env-reqs-unlisted]').includesText('OLD_FLAG');
    });

    test('in Compose mode, keys the file does not mention are not marked', async function (assert) {
      saved = [...saved, { key: 'READ_BY_THE_APP', value: '1' }];
      fakes.api.on('GET', REQUIREMENTS, () =>
        requirements(saved, [variable('DATABASE_URL')]),
      );
      await visit('/apps/blog/env');

      assert.dom('[data-test-env-unlisted]').doesNotExist();
      assert.dom('[data-test-env-reqs-unlisted]').doesNotExist();
    });

    test('a repository that cannot be read leaves the tab usable', async function (assert) {
      fakes.api.on('GET', REQUIREMENTS, () =>
        requirements(saved, [], {
          source: 'dotenv',
          status: 'unavailable',
          reason: 'file_not_found',
          path: '.env.example',
        }),
      );
      await visit('/apps/blog/env');

      assert
        .dom('[data-test-env-reqs-unavailable]')
        .includesText('.env.example')
        .includesText('ファイルがリポジトリにありません');
      assert.dom('[data-test-env-row]').exists({ count: 1 });

      fakes.api.fail('GET', REQUIREMENTS, 500, 'internal');
      await visit('/apps/blog');
      await visit('/apps/blog/env');
      assert.dom('[data-test-env-reqs-error]').exists();
      assert.dom('[data-test-env-row]').exists({ count: 1 });
    });

    test('nothing is shown when the comparison is off', async function (assert) {
      fakes.api.on('GET', REQUIREMENTS, () =>
        requirements(saved, [], { source: 'off', status: 'off' }),
      );
      await visit('/apps/blog/env');
      assert.dom('[data-test-env-reqs]').hasText('');
    });
  });

  module('deploy tab', function (hooks) {
    hooks.beforeEach(function () {
      fakes.api
        .on('GET', '/api/apps/blog/deployments', {
          deployments: [makeDeployment()],
        })
        .on('POST', '/api/apps/blog/deployments', () =>
          makeDeployment({ id: 43, status: 'queued' }),
        )
        .on('GET', '/api/repos/octo/blog/branches', {
          branches: [{ name: 'main', sha: SHA_A }],
        })
        .on('GET', '/api/repos/octo/blog/commits', {
          commits: [
            {
              sha: SHA_A,
              message: 'Send mail',
              author: 'octo',
              date: '2026-11-18T03:00:00Z',
            },
            {
              sha: SHA_B,
              message: 'Before mail',
              author: 'octo',
              date: '2026-11-17T03:00:00Z',
            },
          ],
        })
        .on('GET', REQUIREMENTS, (call: FakeCall) => {
          const sha = call.query['sha'] ?? SHA_A;
          const needs =
            sha === SHA_A
              ? [variable('DATABASE_URL'), variable('SMTP_HOST')]
              : [variable('DATABASE_URL')];
          return requirements(saved, needs, { ref: '', commit_sha: sha });
        });
    });

    test('the picked commit is checked, and the warning does not block', async function (assert) {
      await visit('/apps/blog/deploy');

      assert.deepEqual(fakes.api.callsTo('GET', REQUIREMENTS)[0]?.query, {
        sha: SHA_A,
      });
      assert
        .dom('[data-test-deploy-env-warning]')
        .includesText('必須の環境変数が 1 件未設定です')
        .includesText('SMTP_HOST');
      assert
        .dom('[data-test-deploy-env-link]')
        .hasAttribute('href', '/apps/blog/env');

      await fillIn('[data-test-deploy-commit]', SHA_B);
      assert.dom('[data-test-deploy-env-warning]').doesNotExist();

      await fillIn('[data-test-deploy-commit]', SHA_A);
      await click('[data-test-deploy-submit]');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments').length,
        1,
        'deploying is still possible',
      );
    });

    test('the production dialog repeats the warning', async function (assert) {
      app = { ...app, environment: 'production' };
      await visit('/apps/blog/deploy');
      await click('[data-test-deploy-submit]');

      assert
        .dom('[data-test-dialog] [data-test-deploy-env-warning]')
        .includesText('SMTP_HOST');
      assert
        .dom('[data-test-dialog] [data-test-deploy-env-link]')
        .doesNotExist();
      assert.deepEqual(fakes.api.callsTo('GET', REQUIREMENTS).at(-1)?.query, {
        ref: 'main',
      });
      await click('[data-test-dialog-confirm]');
      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments').length,
        1,
      );
    });

    test('a ref typed by hand is checked too (no branch list without the App)', async function (assert) {
      app = {
        ...app,
        repo_full_name: '',
        repo_url: 'https://github.com/octo/blog.git',
      };
      fakes.api.on('GET', REQUIREMENTS, (call: FakeCall) => {
        const ref = call.query['ref'] ?? '';
        const needs =
          ref === 'main'
            ? [variable('DATABASE_URL'), variable('SMTP_HOST')]
            : [variable('DATABASE_URL')];
        return requirements(saved, needs, { ref, commit_sha: SHA_A });
      });
      await visit('/apps/blog/deploy');

      assert.deepEqual(fakes.api.callsTo('GET', REQUIREMENTS)[0]?.query, {
        ref: 'main',
      });
      assert.dom('[data-test-deploy-env-warning]').includesText('SMTP_HOST');

      await fillIn('[data-test-deploy-ref]', 'old-release');
      assert.deepEqual(fakes.api.callsTo('GET', REQUIREMENTS).at(-1)?.query, {
        ref: 'old-release',
      });
      assert.dom('[data-test-deploy-env-warning]').doesNotExist();
    });

    test('apps that are not compared ask nothing', async function (assert) {
      app = { ...app, env_source: 'off' };
      await visit('/apps/blog/deploy');
      assert.deepEqual(fakes.api.callsTo('GET', REQUIREMENTS), []);
      assert.dom('[data-test-deploy-env-warning]').doesNotExist();
    });
  });

  test('the source is chosen on the settings tab', async function (assert) {
    fakes.api.on('PATCH', '/api/apps/blog', (call: FakeCall) => {
      app = { ...app, ...(call.body as Partial<App>) };
      return app;
    });
    await visit('/apps/blog/settings');

    assert.dom('[data-test-env-source-option="compose"]').isChecked();
    assert.dom('[data-test-field="env_example_path"]').doesNotExist();

    await click('[data-test-env-source-option="dotenv"]');
    await fillIn('[data-test-field="env_example_path"]', 'config/.env.sample');
    await click('[data-test-settings-save]');

    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/apps/blog')[0]?.body, {
      env_source: 'dotenv',
      env_example_path: 'config/.env.sample',
    });
  });
});
