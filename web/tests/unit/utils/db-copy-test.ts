import { module, test } from 'qunit';
import { makeApp, makeDeployment } from 'koroku-console/tests/helpers/fakes';
import {
  CopySettingsForm,
  canRestoreDump,
  copyPlan,
  liveProblem,
  parseCopyLog,
  restoreProblem,
  sourceChoices,
  targetProblem,
} from 'koroku-console/utils/db-copy';

module('Unit | Utility | db-copy', function () {
  test('restoreProblem names what a restore of the app needs', function (assert) {
    const ready = makeApp({
      current_deployment: makeDeployment(),
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump app',
      load_hook_service: 'db',
      load_hook_command: 'pg_restore -d app',
    });
    assert.strictEqual(restoreProblem(ready), '');
    assert.true(
      restoreProblem({ ...ready, dump_hook_command: '' }).includes(
        'ダンプフック',
      ),
    );
    assert.true(
      restoreProblem({ ...ready, load_hook_service: '' }).includes(
        'ロードフック',
      ),
    );
    assert.true(
      restoreProblem({ ...ready, current_deployment: null }).includes(
        'デプロイされていません',
      ),
    );
    assert.true(
      restoreProblem({ ...ready, desired_state: 'stopped' }).includes('停止中'),
    );

    const dump = { kind: 'dump', target: 'db' };
    assert.true(canRestoreDump(ready, dump));
    assert.false(canRestoreDump(ready, { ...dump, target: 'mysql' }));
    assert.false(canRestoreDump(ready, { ...dump, kind: 'volume' }));
    assert.false(canRestoreDump({ ...ready, load_hook_command: '' }, dump));
  });

  test('a restore whose load failed and was undone fails at the load', function (assert) {
    const lines = [
      '==> 2026-11-18T01:00:04Z load Loading the dump of 2026-11-17T09:00:12Z',
      'Loading the dump failed: exit status 1',
      '==> 2026-11-18T01:00:09Z reload Loading the data of blog back from snapshot 4bba301e',
      'Loaded the data before the restore back.',
      '==> 2026-11-18T01:00:20Z failed Restore failed: the load command failed',
    ];
    const done = parseCopyLog(lines, {
      status: 'failed',
      finished_at: '2026-11-18T01:00:20Z',
    });
    assert.deepEqual(
      done.steps.map((step) => [step.key, step.state]),
      [
        ['load', 'failed'],
        ['reload', 'done'],
      ],
    );
    assert.strictEqual(done.failure, 'the load command failed');

    const broken = parseCopyLog(
      [
        ...lines.slice(0, 3),
        'Loading the data back failed: exit status 1',
        'The stopped services stay stopped, because the database may hold part of a dump: web.',
        lines[4]!,
      ],
      { status: 'failed', finished_at: '2026-11-18T01:00:20Z' },
    );
    assert.deepEqual(
      broken.steps.map((step) => [step.key, step.state]),
      [
        ['load', 'failed'],
        ['reload', 'failed'],
      ],
    );
    assert.true(broken.leftStopped);
  });

  test('sourceChoices puts the production app of the same repository first', function (assert) {
    const target = makeApp({ slug: 'blog-staging' });
    const apps = [
      makeApp({
        slug: 'wiki',
        repo_full_name: 'octo/wiki',
        environment: 'production',
      }),
      makeApp({ slug: 'blog-preview' }),
      target,
      makeApp({ slug: 'blog', environment: 'production' }),
    ];
    assert.deepEqual(
      sourceChoices(apps, target).map((c) => [c.app.slug, c.sameRepo]),
      [
        ['blog', true],
        ['blog-preview', true],
        ['wiki', false],
      ],
    );
  });

  test('problems of the source and the target', function (assert) {
    const deployment = makeDeployment();
    const ready = makeApp({
      current_deployment: deployment,
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump',
      load_hook_service: 'db',
      load_hook_command: 'pg_restore',
    });
    assert.strictEqual(liveProblem(ready), '');
    assert.strictEqual(targetProblem(ready), '');
    assert.true(
      liveProblem({ ...ready, dump_hook_command: '' }).includes('ダンプフック'),
    );
    assert.true(
      liveProblem({ ...ready, desired_state: 'stopped' }).includes(
        '稼働していません',
      ),
    );
    assert.true(
      targetProblem({ ...ready, environment: 'production' }).includes('本番'),
    );
    assert.true(
      targetProblem({ ...ready, load_hook_command: '' }).includes(
        'ロードフック',
      ),
    );
    assert.true(
      targetProblem({ ...ready, current_deployment: null }).includes(
        'デプロイ',
      ),
    );
  });

  test('copyPlan says when nothing is backed up first', function (assert) {
    const source = makeApp({ slug: 'blog', dump_hook_command: 'pg_dump' });
    const target = makeApp({
      slug: 'blog-staging',
      load_hook_service: 'db',
      load_hook_command: 'pg_restore',
    });
    const titles = (backupsConfigured: boolean, kind: 'live' | 'snapshot') =>
      copyPlan({ source, target, kind, backupsConfigured });
    assert.true(titles(false, 'live')[0]?.detail.includes('設定されていない'));
    assert.true(titles(true, 'live')[0]?.detail.includes('ダンプフックがない'));
    assert.true(
      titles(true, 'snapshot')[2]?.detail.includes('最新のバックアップ'),
    );
    assert.deepEqual(
      titles(true, 'live').map((step) => step.title),
      ['バックアップ', '停止', 'ダンプ', 'ロード', '起動'],
    );
  });

  test('parseCopyLog marks the failed and the running step', function (assert) {
    const lines = [
      '==> 2026-11-17T03:00:00Z check Checking',
      '==> 2026-11-17T03:00:04Z copy Copying the data',
      'load | error',
      '==> 2026-11-17T03:00:40Z failed Copy failed: the load command failed',
      'The services stopped for the copy stay stopped, because ...',
    ];
    const failed = parseCopyLog(lines, {
      status: 'failed',
      finished_at: '2026-11-17T03:00:41Z',
    });
    assert.deepEqual(
      failed.steps.map((s) => [s.key, s.state]),
      [
        ['check', 'done'],
        ['copy', 'failed'],
      ],
    );
    assert.strictEqual(failed.failure, 'the load command failed');
    assert.true(failed.leftStopped);
    assert.strictEqual(failed.steps[1]?.endedAt, '2026-11-17T03:00:40Z');

    const running = parseCopyLog(lines.slice(0, 3), {
      status: 'running',
      finished_at: null,
    });
    assert.strictEqual(running.steps[1]?.state, 'running');
  });

  test('CopySettingsForm sends only what changed', function (assert) {
    const app = makeApp({
      load_hook_service: 'db',
      load_hook_command: 'pg_restore',
      post_load_service: '',
      post_load_command: '',
    });
    const form = new CopySettingsForm(app);
    assert.true(form.available);
    form.postLoadService = 'web';
    assert.deepEqual(form.validate(), {
      post_load_command: 'サービスを指定するときはコマンドも入力してください。',
    });
    form.postLoadCommand = ' bin/rails db:anonymize ';
    assert.deepEqual(form.patch(app), {
      post_load_service: 'web',
      post_load_command: 'bin/rails db:anonymize',
    });
    assert.false(new CopySettingsForm(makeApp()).available);
  });
});
