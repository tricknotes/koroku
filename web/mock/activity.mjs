// The activity of each app in the mock API (ARCHITECTURE 10.7): who
// changed settings, variables and domains, started a task, and so on, as
// GET /api/apps/{slug}/activity returns it (docs/API.md). Nothing is real.

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;

const OCTO = { id: 1, login: 'octo' };
const MIA = { id: 2, login: 'mia' };

let nextId = 500;

function ago(ms) {
  return new Date(Date.now() - ms).toISOString();
}

function entry(agoMs, actor, action, details = {}) {
  return { id: nextId++, at: ago(agoMs), actor, action, details };
}

function env(change) {
  return {
    added: [],
    changed: [],
    removed: [],
    renamed: [],
    replaced: false,
    ...change,
  };
}

/** Newest first, like the API. */
function blogActivity() {
  return [
    entry(12 * MINUTE, MIA, 'env.changed', env({ changed: ['SMTP_PASSWORD'] })),
    entry(25 * MINUTE, OCTO, 'task_run.started', {
      run_id: 301,
      task_name: 'マイグレーションの状態',
      service: 'web',
      command: 'bin/rails db:migrate:status',
    }),
    entry(3 * HOUR, OCTO, 'app.settings_changed', {
      changes: [
        { field: 'healthcheck_path', from: '/', to: '/up' },
        { field: 'memory_limit', from: '512m', to: '768m' },
      ],
    }),
    entry(6 * HOUR, OCTO, 'registry.set', {
      registry: 'ghcr.io',
      username: 'octo-bot',
      created: false,
      token_replaced: true,
    }),
    entry(1 * DAY, OCTO, 'domain.primary', {
      hostname: 'www.example.com',
      is_primary: true,
    }),
    entry(1 * DAY + 5 * MINUTE, OCTO, 'domain.added', {
      hostname: 'www.example.com',
      status: 'active',
    }),
    entry(3 * DAY, OCTO, 'member.role_set', {
      member: MIA,
      from: '',
      to: 'deploy',
    }),
    entry(4 * DAY, MIA, 'app.restarted'),
    entry(
      5 * DAY,
      OCTO,
      'env.changed',
      env({
        added: ['SMTP_HOST', 'SMTP_PASSWORD'],
        renamed: [{ from: 'MAIL_FROM', to: 'SMTP_FROM' }],
        replaced: true,
      }),
    ),
    entry(6 * DAY, OCTO, 'backup.downloaded', {
      snapshot_id: '4bba301e9c2d7a15',
    }),
    entry(8 * DAY, OCTO, 'tasks.changed', {
      added: ['マイグレーションの状態', '検索インデックスの再構築'],
      changed: [],
      removed: [],
      reordered: false,
    }),
    entry(9 * DAY, OCTO, 'app.created', {
      environment: 'production',
      env_added: [],
    }),
  ];
}

function stagingActivity() {
  return [
    entry(2 * DAY, OCTO, 'db_copy.started', {
      copy_id: 20,
      source: 'blog',
      source_kind: 'live',
    }),
    entry(2 * DAY + HOUR, OCTO, 'app.settings_changed', {
      changes: [
        { field: 'load_hook_service', from: '', to: 'db' },
        {
          field: 'load_hook_command',
          from: '',
          to: 'pg_restore --clean --if-exists --no-owner -U "$POSTGRES_USER" -d "$POSTGRES_DB"',
        },
      ],
    }),
    entry(7 * DAY, OCTO, 'app.created', {
      environment: 'staging',
      env_added: ['DATABASE_URL', 'RAILS_MASTER_KEY'],
    }),
  ];
}

/**
 * blog-staging when it waits for its image workflow (ARCHITECTURE 5.2):
 * koroku itself noted a run that failed, after the owner set the workflow.
 */
function workflowGatedStagingActivity() {
  return [
    entry(40 * MINUTE, null, 'auto_deploy.skipped', {
      workflow: '.github/workflows/image.yml',
      conclusion: 'failure',
      commit_sha: 'c0ffee1234567890c0ffee1234567890c0ffee12',
      branch: 'main',
      run_url: 'https://github.com/octo/blog/actions/runs/1234',
    }),
    entry(2 * HOUR, OCTO, 'app.settings_changed', {
      changes: [
        {
          field: 'auto_deploy_workflow',
          from: '',
          to: '.github/workflows/image.yml',
        },
      ],
    }),
    ...stagingActivity(),
  ];
}

/**
 * Starts blog-staging's activity with a push that was kept during a command
 * and then not deployed, because the disk was full (ARCHITECTURE 5.2).
 */
export function useDeferredActivity(state) {
  state.activity ??= new Map();
  const initial = [
    entry(3 * HOUR, null, 'auto_deploy.dropped', {
      commit_sha: 'feedface1234567890feedface1234567890feed',
      branch: 'main',
      pusher: 'octo',
      workflow: '',
      reason: 'disk_full',
    }),
    ...stagingActivity(),
  ];
  for (const item of [...initial].reverse()) item.id = nextId++;
  state.activity.set('blog-staging', initial);
}

/** Starts blog-staging's activity as an app waiting for a workflow. */
export function useWorkflowGatedActivity(state) {
  state.activity ??= new Map();
  const initial = workflowGatedStagingActivity();
  for (const item of [...initial].reverse()) item.id = nextId++;
  state.activity.set('blog-staging', initial);
}

/** The activity of every app of a state, created on first use. */
export function activityOf(state, slug) {
  state.activity ??= new Map();
  if (!state.activity.has(slug)) {
    const initial =
      slug === 'blog'
        ? blogActivity()
        : slug === 'blog-staging'
          ? stagingActivity()
          : [];
    // IDs grow with time, as in the database: the oldest gets the lowest.
    for (const item of [...initial].reverse()) item.id = nextId++;
    state.activity.set(slug, initial);
  }
  return state.activity.get(slug);
}

/** Adds what the UI just did, by the scenario's user. */
export function recordActivity(state, slug, actor, action, details = {}) {
  activityOf(state, slug).unshift({
    id: nextId++,
    at: new Date().toISOString(),
    actor,
    action,
    details,
  });
}

/** One page, as `GET /api/apps/{slug}/activity?limit&before` answers. */
export function activityPage(state, slug, query) {
  const limit = Number(query.get('limit') ?? 20);
  const before = Number(query.get('before') ?? 0);
  const all = activityOf(state, slug).filter(
    (item) => !before || item.id < before,
  );
  const entries = all.slice(0, limit);
  return {
    entries,
    next_before: all.length > limit ? entries.at(-1).id : null,
  };
}
