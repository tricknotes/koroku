import { module, test } from 'qunit';
import { click, fillIn, settled, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, Task, TaskRun } from 'koroku-console/utils/types';

function makeRun(overrides: Partial<TaskRun> = {}): TaskRun {
  return {
    id: 7,
    app_slug: 'blog',
    task_name: 'status',
    service: 'web',
    command: 'bin/rails db:migrate:status',
    deployment_id: 42,
    status: 'succeeded',
    exit_code: 0,
    error: '',
    started_at: '2026-11-18T03:30:00Z',
    finished_at: '2026-11-18T03:30:04Z',
    ...overrides,
  };
}

const STATUS_TASK: Task = {
  name: 'status',
  service: 'web',
  command: 'bin/rails db:migrate:status',
};

module('Acceptance | tasks', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let tasks: Task[];
  let runs: TaskRun[];

  function serveApp(overrides: Partial<App> = {}): void {
    const running = makeDeployment({ id: 42 });
    const app = makeApp({
      current_deployment: running,
      latest_deployment: running,
      ...overrides,
    });
    const base = `/api/apps/${app.slug}`;
    fakes.api
      .on('GET', base, app)
      .on('GET', `${base}/services`, {
        services: [
          { name: 'db', state: 'running', status: 'Up', health: '' },
          { name: 'web', state: 'running', status: 'Up', health: '' },
        ],
      })
      .on('GET', `${base}/tasks`, () => ({ tasks }))
      .on('PUT', `${base}/tasks`, (call: FakeCall) => {
        tasks = (call.body as { tasks: Task[] }).tasks;
        return { tasks };
      })
      .on('GET', `${base}/task-runs`, () => ({ runs }))
      .on('POST', `${base}/task-runs`, (call: FakeCall) => {
        const body = call.body as Partial<Task> & { task?: string };
        const task = tasks.find((t) => t.name === body.task);
        const run = makeRun({
          id: 8,
          app_slug: app.slug,
          task_name: task?.name ?? '',
          service: task?.service ?? body.service ?? '',
          command: task?.command ?? body.command ?? '',
          status: 'running',
          exit_code: null,
          finished_at: null,
        });
        runs = [run, ...runs];
        return run;
      });
  }

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    tasks = [STATUS_TASK];
    runs = [makeRun()];
  });

  test('a registered task runs with one click on a staging app', async function (assert) {
    serveApp({ slug: 'blog' });
    await visit('/apps/blog/tasks');

    assert
      .dom('[data-test-task="status"]')
      .includesText('bin/rails db:migrate:status');
    assert.dom('[data-test-task-run="7"]').includesText('status');
    assert
      .dom('[data-test-task-run="7"] [data-test-exit-code]')
      .doesNotExist('a zero exit code stays out of the way');

    await click('[data-test-run-task]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/task-runs')[0]?.body,
      { task: 'status' },
    );
    assert
      .dom('[data-test-task-run="8"] [data-test-run-status]')
      .hasText('実行中');
    assert.dom('[data-test-task-run-detail]').exists('the new run opens');
    const stream = fakes.eventSource.last;
    assert.strictEqual(stream.url, '/api/task-runs/8/logs');
    assert
      .dom('[data-test-command-submit]')
      .isDisabled('one command at a time');
    assert.dom('[data-test-run-active]').exists();

    stream.emit('$ bin/rails db:migrate:status');
    stream.emit(' up 20261101090000 Create posts');
    await settled();
    assert.dom('[data-test-step-log]').includesText('Create posts');

    runs = [makeRun({ id: 8 }), ...runs.slice(1)];
    stream.emit('Exited with code 0.');
    stream.end('succeeded');
    await settled();
    assert.true(stream.closed);
    assert
      .dom('[data-test-task-run="8"] [data-test-run-status]')
      .hasText('成功');
    assert.dom('[data-test-command-submit]').isNotDisabled();
  });

  test('a command typed on the spot runs in the chosen service', async function (assert) {
    serveApp({ slug: 'blog' });
    await visit('/apps/blog/tasks');

    assert.dom('[data-test-command-service]').hasValue('web');
    await fillIn('[data-test-command-service]', 'db');
    await fillIn('[data-test-command-input]', "  psql -c 'select 1'  ");
    await click('[data-test-command-submit]');

    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/task-runs')[0]?.body,
      { service: 'db', command: "psql -c 'select 1'" },
    );
    assert.dom('[data-test-command-input]').hasValue('', 'the form is cleared');
  });

  test('a past run runs again', async function (assert) {
    runs = [
      makeRun({ id: 7 }),
      makeRun({
        id: 6,
        task_name: '',
        service: 'db',
        command: 'psql -c "select 1"',
      }),
    ];
    serveApp({ slug: 'blog' });
    await visit('/apps/blog/tasks');
    await fillIn('[data-test-command-input]', 'bin/rails about');

    await click('[data-test-task-run="7"] [data-test-rerun]');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/task-runs')[0]?.body,
      { task: 'status' },
      'a registered task that is unchanged runs by its name',
    );
    assert.dom('[data-test-task-run="8"]').exists('the new run shows');
    assert
      .dom('[data-test-rerun]')
      .isDisabled('one command at a time, while the new one runs');
  });

  test('a command typed on the spot, or a changed task, runs as it was', async function (assert) {
    tasks = [{ ...STATUS_TASK, command: 'bin/rails db:version' }];
    runs = [
      makeRun({ id: 7 }),
      makeRun({
        id: 6,
        task_name: '',
        service: 'db',
        command: 'psql -c "select 1"',
      }),
    ];
    serveApp({ slug: 'blog' });
    await visit('/apps/blog/tasks');
    await fillIn('[data-test-command-input]', 'bin/rails about');

    await click('[data-test-task-run="6"] [data-test-rerun]');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/task-runs')[0]?.body,
      { service: 'db', command: 'psql -c "select 1"' },
    );
    assert
      .dom('[data-test-command-input]')
      .hasValue('bin/rails about', 'the form keeps what was typed');
  });

  test('a production app asks before running a past run again', async function (assert) {
    serveApp({ environment: 'production' });
    await visit('/apps/blog/tasks');
    await click('[data-test-task-run="7"] [data-test-rerun]');

    assert.dom('[data-test-dialog]').exists();
    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps/blog/task-runs'), []);
    await click('[data-test-dialog-confirm]');
    assert.deepEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/task-runs')[0]?.body,
      { task: 'status' },
    );
  });

  test('without manage there is nothing to run again', async function (assert) {
    serveApp({ role: 'deploy' });
    await visit('/apps/blog/tasks');
    assert.dom('[data-test-task-run="7"]').exists();
    assert.dom('[data-test-rerun]').doesNotExist();
  });

  test('a production app asks before running anything', async function (assert) {
    serveApp({ environment: 'production' });
    await visit('/apps/blog/tasks');
    assert.dom('[data-test-production-hint]').exists();

    await click('[data-test-run-task]');
    assert.dom('[data-test-dialog]').exists();
    assert.dom('[data-test-dialog-task]').hasText('status');
    assert
      .dom('[data-test-dialog-command]')
      .hasText('bin/rails db:migrate:status');
    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps/blog/task-runs'), []);

    await click('[data-test-dialog-cancel]');
    assert.deepEqual(fakes.api.callsTo('POST', '/api/apps/blog/task-runs'), []);

    await click('[data-test-run-task]');
    await click('[data-test-dialog-confirm]');
    assert.strictEqual(
      fakes.api.callsTo('POST', '/api/apps/blog/task-runs').length,
      1,
    );
  });

  test('refusals of the backend are explained', async function (assert) {
    serveApp();
    fakes.api.fail('POST', '/api/apps/blog/task-runs', 409, 'busy');
    await visit('/apps/blog/tasks');
    await click('[data-test-run-task]');
    assert.dom('[data-test-error]').includesText('ほかの処理が実行中です');
  });

  test('a failed run opens with its exit code and the error lines marked', async function (assert) {
    runs = [
      makeRun({
        id: 9,
        task_name: '',
        command: "bin/rails runner 'Post.recount!'",
        status: 'failed',
        exit_code: 1,
        error: 'exited with code 1',
      }),
      ...runs,
    ];
    serveApp();
    await visit('/apps/blog/tasks');

    assert
      .dom('[data-test-task-run="9"] [data-test-show-output]')
      .hasText(
        "bin/rails runner 'Post.recount!'",
        'an ad-hoc run is named by its command',
      );
    assert
      .dom('[data-test-task-run="9"] [data-test-exit-code]')
      .hasText('終了コード 1');
    assert
      .dom('[data-test-run-notice="failed"]')
      .includesText('終了コード 1 で終了しました');
    const stream = fakes.eventSource.last;
    stream.emit("undefined method 'recount!' (NoMethodError)");
    stream.emit('Exited with code 1.');
    stream.end('failed');
    await settled();
    assert.dom('[data-test-error-line]').exists();
  });

  test('a running command can be canceled', async function (assert) {
    runs = [
      makeRun({
        id: 10,
        status: 'running',
        exit_code: null,
        finished_at: null,
      }),
    ];
    serveApp();
    fakes.api.on('POST', '/api/task-runs/10/cancel', () => {
      runs = [
        makeRun({
          id: 10,
          status: 'canceled',
          exit_code: null,
          error: 'canceled by user',
        }),
      ];
      return runs[0];
    });
    await visit('/apps/blog/tasks');

    await click('[data-test-cancel-run]');
    assert.strictEqual(
      fakes.api.callsTo('POST', '/api/task-runs/10/cancel').length,
      1,
    );
    assert
      .dom('[data-test-task-run="10"] [data-test-run-status]')
      .hasText('キャンセル');
    assert.dom('[data-test-cancel-run]').doesNotExist();
  });

  test('tasks are edited as a list and saved together', async function (assert) {
    tasks = [];
    runs = [];
    serveApp();
    await visit('/apps/blog/tasks');

    assert.dom('[data-test-no-tasks]').exists();
    assert.dom('[data-test-no-runs]').exists();
    await click('[data-test-add-first-task]');
    assert.dom('[data-test-task-draft]').exists({ count: 1 });
    assert.dom('[data-test-draft-service]').hasValue('web');

    await fillIn('[data-test-draft-name]', 'status');
    await fillIn('[data-test-draft-command]', 'bin/rails db:migrate:status');
    await click('[data-test-add-draft]');
    const names = document.querySelectorAll('[data-test-draft-name]');
    await fillIn(names[1] as Element, 'vacuum');
    await fillIn(
      document.querySelectorAll('[data-test-draft-service]')[1] as Element,
      'db',
    );
    await fillIn(
      document.querySelectorAll('[data-test-draft-command]')[1] as Element,
      'vacuumdb app',
    );
    await click('[data-test-save-tasks]');

    assert.deepEqual(
      fakes.api.callsTo('PUT', '/api/apps/blog/tasks')[0]?.body,
      {
        tasks: [
          STATUS_TASK,
          { name: 'vacuum', service: 'db', command: 'vacuumdb app' },
        ],
      },
    );
    assert.dom('[data-test-task-editor]').doesNotExist();
    assert.dom('[data-test-task]').exists({ count: 2 });

    await click('[data-test-edit-tasks]');
    await click('[data-test-remove-draft]');
    assert.dom('[data-test-task-draft]').exists({ count: 1 });
    await click('[data-test-cancel-edit]');
    assert
      .dom('[data-test-task]')
      .exists({ count: 2 }, 'canceling keeps the list');
  });

  test('validation errors show next to the field they are about', async function (assert) {
    serveApp();
    fakes.api.fail('PUT', '/api/apps/blog/tasks', 422, 'validation_failed', {
      'tasks[0].command': 'must be a single line of text',
    });
    await visit('/apps/blog/tasks');
    await click('[data-test-edit-tasks]');
    await click('[data-test-save-tasks]');

    assert.dom('[data-test-task-editor]').exists('the editor stays open');
    assert
      .dom('[data-test-task-draft] [data-test-field-error]')
      .hasText('must be a single line of text');
    assert
      .dom('[data-test-draft-command]')
      .hasAttribute('aria-invalid', 'true');
  });

  test('an app that was never deployed cannot run commands yet', async function (assert) {
    serveApp({ current_deployment: null, latest_deployment: null });
    await visit('/apps/blog/tasks');

    assert.dom('[data-test-tasks-not-deployed]').exists();
    assert.dom('[data-test-run-task]').isDisabled();
    assert.dom('[data-test-command-submit]').isDisabled();
  });
  test('the owner gets the koroku run line for the host', async function (assert) {
    serveApp({ slug: 'blog' });
    await visit('/apps/blog/tasks');

    assert
      .dom('[data-test-console-command]')
      .hasText('sudo koroku run blog web', 'a shell without a command');
    await fillIn('[data-test-command-service]', 'db');
    await fillIn('[data-test-command-input]', 'psql -U postgres app');
    assert
      .dom('[data-test-console-command]')
      .hasText('sudo koroku run blog db psql -U postgres app');
    await fillIn('[data-test-command-input]', 'echo $DATABASE_URL');
    assert
      .dom('[data-test-console-command]')
      .hasText("sudo koroku run blog db sh -c 'echo $DATABASE_URL'");
  });

  test('a member gets no koroku run line', async function (assert) {
    fakes.api.on('GET', '/api/session', {
      authenticated: true,
      user: { id: 2, login: 'mia', avatar_url: '', owner: false },
    });
    serveApp({ slug: 'blog', role: 'manage' });
    await visit('/apps/blog/tasks');

    assert.dom('[data-test-command-form]').exists();
    assert.dom('[data-test-console]').doesNotExist('only the owner has SSH');
  });
});
