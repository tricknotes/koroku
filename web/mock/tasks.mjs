// One-off commands and registered tasks of the mock API (ARCHITECTURE 9.6):
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
  'tasks-skipped':
    'a scheduled task of blog skipped its latest time because the previous run was still going',
};

/** The zone the mock reads schedules in, as config.toml's timezone. */
export const TIME_ZONE = 'Asia/Tokyo';
const ZONE_OFFSET = 9 * HOUR;

const FIELDS = [
  ['minute', 0, 59],
  ['hour', 0, 23],
  ['day of month', 1, 31],
  ['month', 1, 12],
  ['day of week', 0, 7],
];

/**
 * Parses a schedule like the backend (internal/cron): the sets of each
 * field, or an error message.
 */
export function parseSchedule(expr) {
  const parts = expr.trim().split(/\s+/).filter(Boolean);
  if (parts.length !== 5) {
    return {
      error: `needs 5 fields (minute hour day-of-month month day-of-week) separated by spaces, got ${parts.length}`,
    };
  }
  const sets = [];
  for (const [i, part] of parts.entries()) {
    const [name, min, max] = FIELDS[i];
    const set = new Set();
    for (const item of part.split(',')) {
      const [range, stepText] = item.split('/');
      const step = stepText === undefined ? 1 : Number(stepText);
      if (!Number.isInteger(step) || step < 1) {
        return {
          error: `${name}: step "${stepText}" must be a whole number of at least 1`,
        };
      }
      let lo = min;
      let hi = max;
      if (range !== '*') {
        const [a, b = a] = range.split('-');
        for (const text of [a, b]) {
          if (!/^\d+$/.test(text)) {
            return { error: `${name}: "${text}" is not a number` };
          }
          const n = Number(text);
          if (n < min || n > max) {
            return { error: `${name}: ${n} is out of range ${min}-${max}` };
          }
        }
        lo = Number(a);
        hi = Number(b);
        if (lo > hi) return { error: `${name}: range ${range} goes backwards` };
      }
      for (let v = lo; v <= hi; v += step) set.add(v === 7 && i === 4 ? 0 : v);
    }
    sets.push(set);
  }
  return {
    sets,
    domStar: parts[2].startsWith('*'),
    dowStar: parts[4].startsWith('*'),
  };
}

/** The next time of a schedule after now (ISO), or null. */
export function nextRunAt(expr, now = Date.now()) {
  const parsed = parseSchedule(expr);
  if (parsed.error) return null;
  const [minutes, hours, doms, months, dows] = parsed.sets;
  let t = Math.floor((now + ZONE_OFFSET) / MINUTE) * MINUTE + MINUTE;
  const end = t + 400 * DAY;
  for (; t < end; t += MINUTE) {
    const d = new Date(t);
    const dom = doms.has(d.getUTCDate());
    const dow = dows.has(d.getUTCDay());
    const day = parsed.domStar || parsed.dowStar ? dom && dow : dom || dow;
    if (
      months.has(d.getUTCMonth() + 1) &&
      day &&
      hours.has(d.getUTCHours()) &&
      minutes.has(d.getUTCMinutes())
    ) {
      return new Date(t - ZONE_OFFSET).toISOString();
    }
  }
  return null;
}

/** The latest time of a schedule before now (ISO). */
function previousTime(expr) {
  for (let back = HOUR; back <= 400 * DAY; back += HOUR) {
    const next = nextRunAt(expr, Date.now() - back);
    if (next && Date.parse(next) < Date.now()) return next;
  }
  return ago(HOUR);
}

/** A registered task as the API returns it. */
export function publicTask(task) {
  const schedule = task.schedule ?? '';
  return {
    name: task.name,
    service: task.service,
    command: task.command,
    schedule,
    next_run_at: schedule ? nextRunAt(schedule) : null,
    last_skipped_at: task.last_skipped_at ?? null,
    last_skip_reason: task.last_skip_reason ?? '',
    last_skip_detail: task.last_skip_detail ?? '',
  };
}

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
    schedule: '0 4 * * *',
  },
  {
    name: '期限切れセッションの掃除',
    service: 'web',
    command: "bin/rails runner 'SessionSweepJob.perform_now'",
    schedule: '*/30 * * * *',
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
    actor: { id: 1, login: 'octo' },
    trigger: 'manual',
    ...overrides,
  };
  return run;
}

/** A run the schedule started: nobody started it. */
function scheduled(overrides = {}) {
  return { actor: null, trigger: 'scheduled', ...overrides };
}

function blogRuns(deploymentId, headFailed) {
  const [status, reindex, sweep] = BLOG_TASKS;
  const runner = {
    name: '',
    service: 'web',
    command: "bin/rails runner 'Post.find_each(&:recount!)'",
  };
  const runs = [
    makeRun('blog', deploymentId, sweep, 12 * MINUTE, scheduled()),
    makeRun('blog', deploymentId, status, 25 * MINUTE),
    makeRun('blog', deploymentId, sweep, 42 * MINUTE, scheduled()),
    makeRun('blog', deploymentId, reindex, 3 * HOUR, {
      actor: { id: 2, login: 'mia' },
    }),
    makeRun('blog', deploymentId, reindex, 9 * HOUR, scheduled()),
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
      if (name === 'tasks-skipped') {
        // 04:00 came while the reindex started by hand was still going.
        const reindex = tasks.get(slug)[1];
        const skipped = previousTime(reindex.schedule);
        reindex.last_skipped_at = skipped;
        reindex.last_skip_reason = 'overlap';
        // No scheduled reindex since: the skip is the latest news.
        const list = runs
          .get(slug)
          .filter(
            (run) =>
              !(run.trigger === 'scheduled' && run.task_name === reindex.name),
          );
        runs.set(slug, list);
        list.push(
          makeRun(
            'blog',
            deploymentId,
            reindex,
            Date.now() - Date.parse(skipped) + 20 * MINUTE,
            { actor: { id: 2, login: 'mia' } },
          ),
        );
        list.sort(
          (a, b) => Date.parse(b.started_at) - Date.parse(a.started_at),
        );
      }
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
  const lines = [`$ ${run.command}`];
  if (run.trigger === 'scheduled') {
    lines.push(
      `Scheduled run of task "${run.task_name}" for ${run.started_at.replace(/\.\d+Z$/, 'Z')}.`,
    );
  }
  lines.push(
    `Running in service ${run.service} of deployment #${run.deployment_id}.`,
  );
  return lines;
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
