import { module, test } from 'qunit';
import { click, findAll, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import { serveConsole } from 'koroku-console/tests/helpers/console';
import {
  installFakes,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type {
  ActivityEntry,
  DbCopy,
  TaskRun,
} from 'koroku-console/utils/types';

const MIA = { id: 2, login: 'mia' };
const OCTO = { id: 1, login: 'octo' };

function entry(
  id: number,
  action: string,
  details: Record<string, unknown> = {},
  actor: ActivityEntry['actor'] = MIA,
): ActivityEntry {
  return { id, at: '2026-11-18T02:55:00Z', actor, action, details };
}

const NO_ENV_CHANGE = {
  added: [],
  changed: [],
  removed: [],
  renamed: [],
  replaced: false,
};

// Who did what to an app (ARCHITECTURE 10.7), on the overview, and who
// started task runs and database copies.
module('Acceptance | activity', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    serveConsole(fakes);
  });

  test('the overview says who changed what, newest first', async function (assert) {
    const page1 = [
      entry(9, 'env.changed', {
        ...NO_ENV_CHANGE,
        added: ['API_KEY'],
        renamed: [{ from: 'MAIL_FROM', to: 'SMTP_FROM' }],
        removed: ['OLD'],
      }),
      entry(8, 'app.settings_changed', {
        changes: [
          { field: 'memory_limit', from: '256m', to: '512m' },
          { field: 'access_protection', from: true, to: false },
          { field: 'registry_password', secret: true },
        ],
      }),
      entry(7, 'domain.added', {
        hostname: 'www.example.com',
        status: 'pending',
      }),
      entry(
        6,
        'member.role_set',
        { member: MIA, from: 'view', to: 'deploy' },
        OCTO,
      ),
      entry(5, 'app.stopped', {}, null),
    ];
    const page2 = [entry(4, 'app.created', { environment: 'staging' }, OCTO)];
    fakes.api.on('GET', '/api/apps/blog/activity', (call: FakeCall) =>
      call.query['before'] === '5'
        ? { entries: page2, next_before: null }
        : { entries: page1, next_before: 5 },
    );

    await visit('/apps/blog');

    const items = findAll('[data-test-activity]');
    assert.deepEqual(
      items.map((item) => item.getAttribute('data-test-activity')),
      [
        'env.changed',
        'app.settings_changed',
        'domain.added',
        'member.role_set',
        'app.stopped',
      ],
    );
    assert.strictEqual(
      fakes.api.callsTo('GET', '/api/apps/blog/activity')[0]?.query['limit'],
      '5',
      'the latest few',
    );
    assert
      .dom(items[0])
      .includesText('環境変数を変更')
      .includesText('追加: API_KEY')
      .includesText('名前の変更: MAIL_FROM → SMTP_FROM')
      .includesText('削除: OLD');
    assert
      .dom(items[1])
      .includesText('設定を変更')
      .includesText('メモリ上限（既定）: 256m → 512m')
      .includesText('アクセスの保護: 有効 → 無効')
      .includesText('registry_password（値は記録しません）');
    assert.dom(items[2]).includesText('ドメイン www.example.com を追加');
    assert.dom(items[3]).includesText('mia のロールを閲覧からデプロイに変更');
    assert.dom('[data-test-activity-actor]').hasText('mia');
    assert
      .dom(items[3]!.querySelector('[data-test-activity-actor]'))
      .hasText('octo');
    assert
      .dom(items[4]!.querySelector('[data-test-activity-actor]'))
      .hasText('koroku', 'what koroku did by itself');
    assert
      .dom(items[0]!.querySelector('time'))
      .hasAttribute('title', '2026/11/18 11:55 JST');

    await click('[data-test-activity-more]');
    assert.dom('[data-test-activity]').exists({ count: 6 });
    assert
      .dom('[data-test-activity="app.created"]')
      .includesText('アプリを作成');
    assert
      .dom('[data-test-activity-more]')
      .doesNotExist('nothing older is left');
  });

  test('a workflow that did not succeed is noted by koroku', async function (assert) {
    fakes.api.on('GET', '/api/apps/blog/activity', () => ({
      entries: [
        entry(
          3,
          'auto_deploy.skipped',
          {
            workflow: '.github/workflows/image.yml',
            conclusion: 'failure',
            commit_sha: 'c0ffee1234567890c0ffee1234567890c0ffee12',
            branch: 'main',
            run_url: 'https://github.com/octo/blog/actions/runs/1234',
          },
          null,
        ),
        entry(2, 'app.settings_changed', {
          changes: [
            {
              field: 'auto_deploy_workflow',
              from: '',
              to: '.github/workflows/image.yml',
            },
          ],
        }),
      ],
      next_before: null,
    }));
    await visit('/apps/blog');

    const items = findAll('[data-test-activity]');
    assert
      .dom(items[0])
      .includesText('ワークフローが失敗したため自動デプロイせず')
      .includesText('ワークフロー: .github/workflows/image.yml')
      .includesText('コミット: c0ffee1（main）');
    assert
      .dom(items[0]!.querySelector('[data-test-activity-actor]'))
      .hasText('koroku');
    assert
      .dom(items[1])
      .includesText(
        '先に待つワークフロー: （なし） → .github/workflows/image.yml',
      );
  });

  test('registry credentials are worded without the token', async function (assert) {
    fakes.api.on('GET', '/api/apps/blog/activity', () => ({
      entries: [
        entry(3, 'registry.removed', {
          registry: 'registry.example.com:5000',
          username: 'bot',
        }),
        entry(2, 'registry.set', {
          registry: 'registry.example.com:5000',
          username: 'bot',
          created: false,
          token_replaced: true,
          from: { registry: 'ghcr.io', username: 'octo-bot' },
        }),
        entry(1, 'registry.set', {
          registry: 'ghcr.io',
          username: 'octo-bot',
          created: true,
          token_replaced: true,
        }),
      ],
      next_before: null,
    }));

    await visit('/apps/blog');

    const items = findAll('[data-test-activity]');
    assert
      .dom(items[0])
      .includesText(
        'コンテナレジストリ registry.example.com:5000 の認証を削除',
      );
    assert
      .dom(items[1])
      .includesText('コンテナレジストリ registry.example.com:5000 の認証を変更')
      .includesText('レジストリ: ghcr.io → registry.example.com:5000')
      .includesText('ユーザー名: octo-bot → bot')
      .includesText('トークンを入れ替え');
    assert
      .dom(items[2])
      .includesText('コンテナレジストリ ghcr.io の認証を設定')
      .includesText('ユーザー名: octo-bot')
      .doesNotIncludeText('トークン');
  });

  test('an app without activity says so', async function (assert) {
    fakes.api.on('GET', '/api/apps/blog/activity', {
      entries: [],
      next_before: null,
    });
    await visit('/apps/blog');

    assert.dom('[data-test-activity-empty]').exists();
    assert.dom('[data-test-activity-more]').doesNotExist();
  });

  test('a backend without activity shows no section', async function (assert) {
    await visit('/apps/blog');

    assert.dom('[data-test-overview-backup]').exists();
    assert.dom('[data-test-activity-log]').doesNotExist();
  });

  test('task runs say who started them', async function (assert) {
    const run: TaskRun = {
      id: 7,
      app_slug: 'blog',
      task_name: 'status',
      service: 'web',
      command: 'bin/rails db:migrate:status',
      deployment_id: 42,
      status: 'succeeded',
      exit_code: 0,
      error: '',
      started_at: '2026-11-18T02:30:00Z',
      finished_at: '2026-11-18T02:30:04Z',
      actor: MIA,
    };
    fakes.api
      .on('GET', '/api/apps/blog/tasks', { tasks: [] })
      .on('GET', '/api/apps/blog/task-runs', {
        runs: [run, { ...run, id: 6, actor: null }],
      })
      .on('GET', '/api/task-runs/7', run);
    await visit('/apps/blog/tasks');

    assert.dom('[data-test-run-actor]').exists({ count: 1 });
    assert.dom('[data-test-run-actor]').hasText('実行: mia');
    await click('[data-test-task-run="7"] [data-test-show-output]');
    assert.dom('[data-test-run-detail-actor]').hasText('mia');
  });

  test('database copies say who started them', async function (assert) {
    const copy: DbCopy = {
      id: 8,
      target_app: 'blog',
      source_app: 'shop',
      source_kind: 'live',
      status: 'succeeded',
      error: '',
      bytes: 1024,
      backup_snapshot_id: '',
      started_at: '2026-11-18T02:00:00Z',
      finished_at: '2026-11-18T02:01:00Z',
      actor: OCTO,
    };
    // A backend that has copies sends the load hook settings.
    serveConsole(fakes, {
      load_hook_service: 'db',
      load_hook_command: 'pg_restore -d app',
      post_load_service: '',
      post_load_command: '',
    });
    fakes.api.on('GET', '/api/apps/blog/db-copies', { copies: [copy] });
    await visit('/apps/blog/backups');

    assert.dom('[data-test-copy-actor]').hasText('実行: octo');
  });
});
