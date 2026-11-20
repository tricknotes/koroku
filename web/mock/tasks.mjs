// One-off commands and registered tasks of the mock API (ARCHITECTURE 9.5):
// fixtures per scenario, runs that the mock carries out by itself, and their
// output as the backend writes it (docs/API.md).

const SECOND = 1000;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

/** Scenarios this module adds; listed by `GET /__mock`. */
export const TASK_SCENARIOS = {
  'tasks-running':
    'blog is running a registered task right now (its output streams in)',
  'tasks-failed':
    'the latest command on blog failed with exit code 1; shop-staging has no tasks and no runs',
};

function ago(ms) {
  return new Date(Date.now() - ms).toISOString();
}

const BLOG_TASKS = [
  {
    name: 'マイグレーションの状態',
    service: 'web',
    command: 'bin/rails db:migrate:status',
  },
  {
    name: '検索インデックスの再構築',
    service: 'worker',
    command: 'bin/rails search:reindex',
  },
  {
    name: 'DB の容量',
    service: 'db',
    command:
      'psql -U postgres -d app -c "select pg_size_pretty(pg_database_size(\'app\'))"',
  },
];

/** What each known command prints, and how it ends. */
const OUTPUTS = {
  'bin/rails db:migrate:status': {
    exit: 0,
    lines: [
      '',
      'database: app_production',
      '',
      ' Status   Migration ID    Migration Name',
      '--------------------------------------------------',
      '   up     20261101090000  Create posts',
      '   up     20261112103000  Add reading time to posts',
      '   up     20261118021500  Add index to posts slug',
    ],
  },
  'bin/rails search:reindex': {
    exit: 0,
    lines: [
      'Reindexing Post (1,240 records)',
      ...Array.from(
        { length: 12 },
        (_, i) => `  ${(i + 1) * 100} / 1240 indexed`,
      ),
      '  1240 / 1240 indexed',
      'Done in 38.2s',
    ],
  },
  "bin/rails runner 'Post.find_each(&:recount!)'": {
    exit: 1,
    lines: [
      "/app/vendor/bundle/ruby/3.4.0/gems/railties-8.1.2/lib/rails/commands/runner/runner_command.rb:51:in 'Kernel#eval': undefined method 'recount!' for an instance of Post (NoMethodError)",
      "\tfrom (eval at /app/vendor/bundle/ruby/3.4.0/gems/railties-8.1.2/lib/rails/commands/runner/runner_command.rb:51):1:in 'block in <main>'",
      "\tfrom /app/vendor/bundle/ruby/3.4.0/gems/activerecord-8.1.2/lib/active_record/relation/batches.rb:88:in 'ActiveRecord::Batches#find_each'",
    ],
  },
};

const DEFAULT_OUTPUT = { exit: 0, lines: ['ok'] };

let nextRunId = 300;

/**
 * A run as the API shows it. `mock` holds what the mock needs to carry it
 * out: the planned output lines and when the run (or each line) is due.
 */
function makeRun(slug, deploymentId, task, startedAgo, overrides = {}) {
  const output = OUTPUTS[task.command] ?? DEFAULT_OUTPUT;
  const id = nextRunId++;
  const duration = 3 * SECOND + (id % 5) * 4 * SECOND;
  const run = {
    id,
    app_slug: slug,
    task_name: task.name ?? '',
    service: task.service,
    command: task.command,
    deployment_id: deploymentId,
    status: output.exit === 0 ? 'succeeded' : 'failed',
    exit_code: output.exit,
    error: output.exit === 0 ? '' : `exited with code ${output.exit}`,
    started_at: ago(startedAgo),
    finished_at: ago(Math.max(0, startedAgo - duration)),
    ...overrides,
  };
  return run;
}

function blogRuns(deploymentId, headFailed) {
  const [status, reindex] = BLOG_TASKS;
  const runner = {
    name: '',
    service: 'web',
    command: "bin/rails runner 'Post.find_each(&:recount!)'",
  };
  const runs = [
    makeRun('blog', deploymentId, status, 25 * MINUTE),
    makeRun('blog', deploymentId, reindex, 3 * HOUR),
    makeRun('blog', deploymentId, status, 2 * DAY),
  ];
  if (headFailed)
    runs.unshift(makeRun('blog', deploymentId, runner, 6 * MINUTE));
  return runs;
}

/** Registered tasks and runs of every app, for one scenario. */
export function buildTasks(name, apps) {
  const tasks = new Map();
  const runs = new Map();
  for (const [slug, app] of apps) {
    tasks.set(slug, []);
    runs.set(slug, []);
    const deploymentId = app.current_deployment?.id ?? null;
    if (slug === 'blog' && deploymentId !== null) {
      tasks.set(
        slug,
        BLOG_TASKS.map((task) => ({ ...task })),
      );
      runs.set(slug, blogRuns(deploymentId, name === 'tasks-failed'));
    } else if (slug === 'blog-staging' && deploymentId !== null) {
      tasks.set(
        slug,
        BLOG_TASKS.slice(0, 2).map((task) => ({ ...task })),
      );
      runs.set(slug, [makeRun(slug, deploymentId, BLOG_TASKS[0], 40 * MINUTE)]);
    } else if (slug === 'shop' && deploymentId !== null) {
      tasks.set(slug, [
        {
          name: 'マイグレーションの状態',
          service: 'web',
          command: 'bin/rails db:migrate:status',
        },
      ]);
    }
  }
  if (name === 'tasks-running' && runs.has('blog')) {
    const blog = apps.get('blog');
    const run = makeRun('blog', blog.current_deployment.id, BLOG_TASKS[1], 0, {
      status: 'running',
      exit_code: null,
      error: '',
      started_at: ago(9 * SECOND),
      finished_at: null,
    });
    // Never finishes on its own: the screen keeps showing a live run.
    run.mock = {
      startedAt: Date.now() - 9 * SECOND,
      finishAt: null,
      // Slow enough that the output keeps arriving while one looks.
      every: 2500,
    };
    runs.get('blog').unshift(run);
  }
  return { tasks, runs };
}

/** The tasks and runs of a state, created on first use. */
export function tasksState(state, scenario) {
  state.taskState ??= buildTasks(scenario, state.apps);
  for (const slug of state.apps.keys()) {
    if (!state.taskState.tasks.has(slug)) state.taskState.tasks.set(slug, []);
    if (!state.taskState.runs.has(slug)) state.taskState.runs.set(slug, []);
  }
  return state.taskState;
}

/** How often a running command prints a line. */
const LINE_EVERY = 700;

/** Starts a run that the mock finishes by itself a little later. */
export function startTaskRun(taskState, app, spec) {
  const output = OUTPUTS[spec.command] ?? DEFAULT_OUTPUT;
  const run = makeRun(app.slug, app.current_deployment.id, spec, 0, {
    status: 'running',
    exit_code: null,
    error: '',
    started_at: ago(0),
    finished_at: null,
  });
  run.mock = {
    startedAt: Date.now(),
    finishAt: Date.now() + (output.lines.length + 2) * LINE_EVERY,
  };
  taskState.runs.get(app.slug).unshift(run);
  return run;
}

/** Ends runs whose time is up, the way the backend records them. */
export function settleTaskRuns(taskState) {
  for (const list of taskState.runs.values()) {
    for (const run of list) {
      if (run.status !== 'running' || !run.mock?.finishAt) continue;
      if (run.mock.finishAt > Date.now()) continue;
      const output = OUTPUTS[run.command] ?? DEFAULT_OUTPUT;
      run.status = output.exit === 0 ? 'succeeded' : 'failed';
      run.exit_code = output.exit;
      run.error = output.exit === 0 ? '' : `exited with code ${output.exit}`;
      run.finished_at = new Date().toISOString();
    }
  }
}

export function cancelTaskRun(run) {
  run.status = 'canceled';
  run.exit_code = null;
  run.error = 'canceled by user';
  run.finished_at = new Date().toISOString();
  if (run.mock) run.mock.canceledAfter = linesDue(run);
}

/** How many output lines of a running command are out by now. */
function linesDue(run) {
  if (!run.mock) return Infinity;
  return Math.floor(
    (Date.now() - run.mock.startedAt) / (run.mock.every ?? LINE_EVERY),
  );
}

function header(run) {
  return [
    `$ ${run.command}`,
    `Running in service ${run.service} of deployment #${run.deployment_id}.`,
  ];
}

function footer(run) {
  switch (run.status) {
    case 'succeeded':
    case 'failed':
      return [`Exited with code ${run.exit_code}.`];
    case 'canceled':
      return ['Canceled.'];
    default:
      return [];
  }
}

/** The log of a run so far: what the SSE stream sends, then follows. */
export function taskRunLog(run, settle) {
  const output = OUTPUTS[run.command] ?? DEFAULT_OUTPUT;
  const shown = (count) => output.lines.slice(0, count);
  if (run.status !== 'running') {
    const printed = run.mock?.canceledAfter ?? Infinity;
    return {
      lines: [...header(run), ...shown(printed), ...footer(run)],
      end: run.status,
    };
  }
  let sent = Math.min(linesDue(run), output.lines.length);
  return {
    lines: [...header(run), ...shown(sent)],
    next: () => {
      settle();
      if (run.status !== 'running') {
        const upTo = run.mock?.canceledAfter ?? output.lines.length;
        const lines = [...output.lines.slice(sent, upTo), ...footer(run)];
        sent = output.lines.length;
        return { lines, end: run.status };
      }
      const due = Math.min(linesDue(run), output.lines.length);
      const lines = output.lines.slice(sent, due);
      sent = due;
      return { lines };
    },
  };
}
