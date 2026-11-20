import { module, test } from 'qunit';
import { click, fillIn, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeBackups,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, DbCopy } from 'koroku-console/utils/types';

function makeCopy(overrides: Partial<DbCopy> = {}): DbCopy {
  return {
    id: 7,
    target_app: 'blog-staging',
    source_app: 'blog',
    source_kind: 'live',
    status: 'succeeded',
    error: '',
    bytes: 18350080,
    backup_snapshot_id: '4bba301e5d2c7a90',
    started_at: '2026-11-17T03:00:00Z',
    finished_at: '2026-11-17T03:01:12Z',
    ...overrides,
  };
}

const LOG = [
  '==> 2026-11-17T03:00:00Z check Checking blog and blog-staging',
  '==> 2026-11-17T03:00:01Z backup Backing up the data of blog-staging',
  '==> 2026-11-17T03:00:03Z stop Stopping the other services of blog-staging',
  'Stopped: web, worker',
  '==> 2026-11-17T03:00:04Z copy Copying the data',
  'load | pg_restore: error: could not execute query',
  '==> 2026-11-17T03:00:40Z failed Copy failed: the load command failed: exit status 1',
  'The services stopped for the copy stay stopped, because the command after the copy did not run to the end: web, worker.',
];

module('Acceptance | database copy', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let staging: App;
  let production: App;
  let copies: DbCopy[];

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    production = makeApp({
      slug: 'blog',
      environment: 'production',
      current_deployment: deployment,
      backup_exclude_volumes: [],
      backup_before_deploy: true,
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -Fc app',
      load_hook_service: '',
      load_hook_command: '',
      post_load_service: '',
      post_load_command: '',
    });
    staging = makeApp({
      slug: 'blog-staging',
      current_deployment: deployment,
      latest_deployment: deployment,
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -Fc app',
      backup_exclude_volumes: [],
      backup_before_deploy: false,
      load_hook_service: 'db',
      load_hook_command: 'pg_restore -d app',
      post_load_service: 'web',
      post_load_command: 'bin/rails db:anonymize',
    });
    const other = makeApp({ slug: 'wiki', repo_full_name: 'octo/wiki' });
    copies = [makeCopy()];
    fakes.api
      .on('GET', '/api/apps', () => ({ apps: [other, staging, production] }))
      .on('GET', '/api/apps/blog-staging', () => staging)
      .on('PATCH', '/api/apps/blog-staging', (call: FakeCall) => {
        staging = { ...staging, ...(call.body as Partial<App>) };
        return staging;
      })
      .on('GET', '/api/apps/blog', () => production)
      .on('GET', '/api/backups', makeBackups())
      .on('GET', '/api/apps/blog-staging/backups', { snapshots: [] })
      .on('GET', '/api/apps/blog-staging/volumes', { volumes: [] })
      .on('GET', '/api/apps/blog/backups', { snapshots: [] })
      .on('GET', '/api/apps/blog/volumes', { volumes: [] })
      .on('GET', '/api/apps/blog-staging/db-copies', () => ({ copies }));
  });

  test('a staging app offers the production app of its repository first', async function (assert) {
    await visit('/apps/blog-staging/backups');

    assert.dom('[data-test-db-copy]').exists();
    assert
      .dom('[data-test-copy-source] option')
      .exists({ count: 2 }, 'every other app');
    assert
      .dom('[data-test-copy-source] option:first-child')
      .hasValue('blog')
      .includesText('本番・同じリポジトリ');
    assert
      .dom('[data-test-copy-plan]')
      .includesText('pg_dump -Fc app')
      .includesText('pg_restore -d app')
      .includesText('bin/rails db:anonymize');
    assert.dom('[data-test-copy-run]').isNotDisabled();
    assert
      .dom('[data-test-copy="7"]')
      .includesText('blog')
      .includesText('完了');
    assert.dom('[data-test-copy-detail]').exists('the newest copy is open');
  });

  test('a production app has no copy section', async function (assert) {
    await visit('/apps/blog/backups');
    assert.dom('[data-test-db-copy]').doesNotExist();
    assert.dom('[data-test-dump-preset]').exists();
  });

  test('starting a copy asks for the app name and sends it', async function (assert) {
    fakes.api.on('POST', '/api/apps/blog-staging/db-copies', () => {
      const started = makeCopy({
        id: 8,
        status: 'running',
        bytes: 0,
        finished_at: null,
      });
      copies = [started, ...copies];
      return started;
    });
    await visit('/apps/blog-staging/backups');

    await click('[data-test-copy-run]');
    assert
      .dom('[data-test-dialog]')
      .includesText('blog-staging のいまのデータは置き換わります');
    assert.dom('[data-test-dialog-confirm]').isDisabled();
    await fillIn('[data-test-dialog-input]', 'blog-staging');
    await click('[data-test-dialog-confirm]');

    const [call] = fakes.api.callsTo(
      'POST',
      '/api/apps/blog-staging/db-copies',
    );
    assert.deepEqual(call?.body, {
      source: 'blog',
      source_kind: 'live',
      confirm: 'blog-staging',
    });
    assert.dom('[data-test-copy="8"]').includesText('コピー中');
    assert.dom('[data-test-copy-run]').isDisabled('one copy at a time');
    assert.true(
      fakes.eventSource.last.url.endsWith('/api/db-copies/8/logs'),
      'the new copy is followed',
    );
  });

  test('a source without a dump hook cannot be copied live', async function (assert) {
    production = {
      ...production,
      dump_hook_service: '',
      dump_hook_command: '',
    };
    await visit('/apps/blog-staging/backups');
    assert
      .dom('[data-test-copy-blocked]')
      .includesText('ダンプフックがありません');
    assert.dom('[data-test-copy-run]').isDisabled();
  });

  test('a failed copy shows the step, the reason and the stopped services', async function (assert) {
    copies = [
      makeCopy({
        status: 'failed',
        error: 'the load command failed: exit status 1',
      }),
    ];
    await visit('/apps/blog-staging/backups');
    for (const line of LOG) fakes.eventSource.last.emit(line);
    fakes.eventSource.last.end('failed');
    await settled();

    assert
      .dom('[data-test-copy-failed]')
      .includesText('「コピー」の手順で失敗しました')
      .includesText('the load command failed')
      .includesText('4bba301e5d2c7a90');
    assert.dom('[data-test-copy-left-stopped]').exists();
    assert.dom('[data-test-step="copy"]').hasAttribute('data-state', 'failed');
  });

  test('the load hook is filled from a preset and saved', async function (assert) {
    await visit('/apps/blog-staging/backups');
    await fillIn('[data-test-load-preset]', 'mysql');
    assert
      .dom('[data-test-field="load_hook_command"]')
      .hasValue('mysql -u root -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"');
    await fillIn('[data-test-field="post_load_command"]', '');
    await click('[data-test-copy-settings-save]');
    assert
      .dom('[data-test-field-group="post_load_command"]')
      .includesText('サービスを指定するときはコマンドも入力してください');

    await fillIn('[data-test-field="post_load_service"]', '');
    await click('[data-test-copy-settings-save]');
    const [call] = fakes.api.callsTo('PATCH', '/api/apps/blog-staging');
    assert.deepEqual(call?.body, {
      load_hook_command:
        'mysql -u root -p"$MYSQL_ROOT_PASSWORD" "$MYSQL_DATABASE"',
      post_load_service: '',
      post_load_command: '',
    });
    assert.dom('[data-test-copy-settings-saved]').exists();
  });
});
