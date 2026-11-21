// Database copies of the mock API (ARCHITECTURE 10.5): the history of the
// staging apps and the log a copy writes, as internal/deploy/dbcopy.go
// would. Nothing here is real.

const SECOND = 1000;
const MINUTE = 60 * SECOND;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const MB = 1000 * 1000;

let nextId = 20;

function ago(ms) {
  return new Date(Date.now() - ms).toISOString();
}

function stamp(ms) {
  return new Date(ms).toISOString().replace(/\.\d{3}Z$/, 'Z');
}

/** One copy into `target`, finished `finishedAgo` ago (null: running). */
function copy(target, source, finishedAgo, overrides = {}) {
  const duration = 72 * SECOND;
  const startedAgo = (finishedAgo ?? 0) + duration;
  return {
    id: nextId++,
    target_app: target,
    source_app: source,
    source_kind: 'live',
    status: 'succeeded',
    error: '',
    bytes: 18.4 * MB,
    backup_snapshot_id: '7c1e09ab52f4d613',
    started_at: ago(startedAgo),
    finished_at: finishedAgo === null ? null : ago(finishedAgo),
    actor: { id: 1, login: 'octo' },
    ...overrides,
  };
}

/** The history of each staging app in a scenario. */
export function copyHistory(scenario) {
  const blog = [
    copy('blog-staging', 'blog', 2 * DAY),
    copy('blog-staging', 'blog', 9 * DAY, {
      source_kind: 'snapshot',
      bytes: 17.9 * MB,
    }),
  ];
  if (scenario === 'copy-running') {
    blog.unshift(
      copy('blog-staging', 'blog', null, {
        status: 'running',
        bytes: 0,
        // The mock finishes it a while after the screen opened.
        mockFinishAt: Date.now() + 10 * MINUTE,
      }),
    );
  }
  if (scenario === 'copy-failed') {
    blog.unshift(
      copy('blog-staging', 'blog', 6 * MINUTE, {
        status: 'failed',
        bytes: 9.1 * MB,
        error: 'the load command failed: docker compose exec: exit status 1',
      }),
    );
  }
  return new Map([
    ['blog-staging', blog],
    ['shop-staging', []],
  ]);
}

/** A copy started from the UI: it runs for a few seconds. */
export function newCopy(state, target, body) {
  const entry = copy(target, body.source, null, {
    source_kind: body.source_kind ?? 'live',
    status: 'running',
    bytes: 0,
    backup_snapshot_id: state.backups.configured ? '9a41d27c03be58f1' : '',
    mockFinishAt: Date.now() + 8 * SECOND,
  });
  entry.started_at = new Date().toISOString();
  return entry;
}

/** Finishes running copies whose time has come. */
export function settleCopies(state) {
  for (const list of state.dbCopies?.values() ?? []) {
    for (const entry of list) {
      if (entry.status !== 'running' || !entry.mockFinishAt) continue;
      if (Date.now() < entry.mockFinishAt) continue;
      entry.status = 'succeeded';
      entry.bytes = 18.4 * MB;
      entry.finished_at = new Date(entry.mockFinishAt).toISOString();
      delete entry.mockFinishAt;
    }
  }
}

/**
 * The script of a copy log: `[seconds after the start, line]`. A failed
 * copy stops in the load and leaves the services stopped, as a copy with a
 * post-load command does.
 */
function script(entry, app) {
  const target = entry.target_app;
  const live = entry.source_kind === 'live';
  const lines = [
    [0, `check Checking ${entry.source_app} and ${target}`],
    [
      0,
      live
        ? `Source: ${entry.source_app}, live dump in service db: pg_dump -Fc -U "$POSTGRES_USER" "$POSTGRES_DB"`
        : `Source: snapshot 4bba301e of ${entry.source_app}, taken ${stamp(Date.parse(entry.started_at) - 9 * HOUR)}`,
    ],
    [
      0,
      `Target: ${target}, load hook in service ${app?.load_hook_service || 'db'}: ${app?.load_hook_command || 'pg_restore'}`,
    ],
    [1, `backup Backing up the data of ${target}`],
    entry.backup_snapshot_id
      ? [
          1,
          `Stored the dump of ${target} as snapshot ${entry.backup_snapshot_id.slice(0, 8)}.`,
        ]
      : [
          1,
          `Backups are not configured; the current data of ${target} is not backed up first.`,
        ],
    [3, `stop Stopping the other services of ${target}`],
    [3, ` Container koroku-${target}-web-1 Stopping`],
    [3, ` Container koroku-${target}-worker-1 Stopping`],
    [4, ` Container koroku-${target}-worker-1 Stopped`],
    [4, ` Container koroku-${target}-web-1 Stopped`],
    [4, 'Stopped: web, worker'],
    [4, 'copy Copying the data'],
  ];
  if (entry.status === 'failed') {
    lines.push(
      [
        40,
        'load | pg_restore: error: could not execute query: ERROR:  column "nickname" of relation "users" does not exist',
      ],
      [
        40,
        'load | Command was: COPY public.users (id, email, nickname) FROM stdin;',
      ],
      [40, 'failed Copy failed: ' + entry.error],
      [
        40,
        'The services stopped for the copy stay stopped, because the command after the copy did not run to the end: web, worker.',
      ],
      [40, 'Run the copy again, or start the app from the console.'],
    );
    return lines;
  }
  lines.push(
    [58, `Copied ${(entry.bytes / MB || 18.4).toFixed(1)} MB.`],
    [
      58,
      `after Running the command after the copy in ${app?.post_load_service || 'web'}`,
    ],
    [58, `after |  Container koroku-${target}-web-run-3f0c2a91 Creating`],
    [59, `after |  Container koroku-${target}-web-run-3f0c2a91 Created`],
    [66, 'after | Anonymized 1284 users and 3902 orders.'],
    [67, 'start Starting the stopped services'],
    [67, ` Container koroku-${target}-worker-1 Starting`],
    [67, ` Container koroku-${target}-web-1 Starting`],
    [71, ` Container koroku-${target}-worker-1 Started`],
    [71, ` Container koroku-${target}-web-1 Started`],
    [71, 'Started: web, worker'],
    [72, `done Copied the data of ${entry.source_app} into ${target}`],
  );
  return lines;
}

const STEP = /^(check|backup|stop|copy|after|start|done|failed) /;

/**
 * The log of a copy so far. A running copy written from the UI advances
 * with the clock; one that runs for minutes stops at the copy step.
 */
export function copyLog(entry, app) {
  const start = Date.parse(entry.started_at);
  const all = script(entry, app).map(([seconds, text]) => ({
    at: start + seconds * SECOND,
    text: STEP.test(text)
      ? `==> ${stamp(start + seconds * SECOND)} ${text}`
      : text,
  }));
  const running = entry.status === 'running';
  // A short mock copy runs its whole script; a long one sits in the copy.
  const scale = running ? (entry.mockFinishAt - start) / (72 * SECOND) : 1;
  const due = (line) =>
    !running || start + (line.at - start) * scale <= Date.now();
  let sent = all.filter(due);
  const lines = sent.map((line) => line.text);
  if (!running) return { lines, end: entry.status };
  return {
    lines,
    next: () => {
      const more = all.filter((line) => due(line) && !sent.includes(line));
      sent = [...sent, ...more];
      if (entry.status !== 'running') {
        const rest = all.filter((line) => !sent.includes(line));
        sent = all;
        return {
          lines: [...more, ...rest].map((line) => line.text),
          end: entry.status,
        };
      }
      return { lines: more.map((line) => line.text) };
    },
  };
}
