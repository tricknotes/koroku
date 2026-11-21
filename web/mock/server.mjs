// Mock of the koroku HTTP API for design work on the console. It serves the
// built app (web/dist) and answers the API from fixtures, so every state of
// a screen can be looked at without a host, Docker or GitHub.
//
// A development tool only: it is not part of the production build or the
// embedded binary, has no dependencies and listens on 127.0.0.1 alone.
//
//   node mock/server.mjs            (from web/, after `pnpm build`)
//   http://127.0.0.1:4300/?scenario=warnings&theme=dark
//
// See web/README.md for the scenarios and options.

import { randomBytes } from 'node:crypto';
import { createServer } from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import { dirname, extname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  BASE_DOMAIN,
  COMPOSE,
  ENV_PRESETS,
  ENV_VARS,
  MEMBER,
  MEMBER_ROLES,
  OWNER,
  REPOS,
  SCENARIOS,
  appVolumes,
  buildScenario,
  commits,
  composeRequirements,
  containerResources,
  dockerUsage,
  members,
  newApp,
  newDomain,
  promotionPreview,
  services,
  settle,
  settleBackups,
  snapshots,
  startBackup,
  startDeployment,
  startRollback,
} from './fixtures.mjs';
import { activityPage, recordActivity } from './activity.mjs';
import { deploymentLog } from './deploy-log.mjs';
import { copyHistory, copyLog, newCopy, settleCopies } from './db-copy.mjs';
import { LOG_SCENARIOS, registerLogRoutes } from './logs.mjs';
import { memoryHistory } from './memory.mjs';
import {
  TASK_SCENARIOS,
  cancelTaskRun,
  settleTaskRuns,
  startTaskRun,
  taskRunLog,
  tasksState,
} from './tasks.mjs';

const HOST = '127.0.0.1';
const PORT = Number(process.env.MOCK_PORT ?? 4300);
const DEFAULT_SCENARIO = process.env.MOCK_SCENARIO ?? 'healthy';
const DIST = resolve(dirname(fileURLToPath(import.meta.url)), '..', 'dist');
const SLOW = 60 * 1000;

// The policy koroku serves the console with (internal/api/auth.go), so that
// a violation shows up here first: no inline scripts, same-origin API.
const CONTENT_SECURITY_POLICY = [
  "default-src 'self'",
  "script-src 'self'",
  "style-src 'self' 'unsafe-inline'",
  "img-src 'self' data: https://avatars.githubusercontent.com",
  "connect-src 'self'",
  "object-src 'none'",
  "base-uri 'none'",
  "form-action 'self'",
  "frame-ancestors 'none'",
].join('; ');

const MIME = {
  '.css': 'text/css; charset=utf-8',
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.json': 'application/json',
  '.map': 'application/json',
  '.svg': 'image/svg+xml',
  '.txt': 'text/plain; charset=utf-8',
};

/** Mutable state per scenario, so changes made in the UI stick. */
const states = new Map();

function stateOf(name) {
  if (!states.has(name)) {
    const state = buildScenario(name);
    state.dbCopies = copyHistory(name);
    states.set(name, state);
  }
  const state = states.get(name);
  settle(state);
  settleBackups(state);
  settleCopies(state);
  return state;
}

class HttpError extends Error {
  constructor(status, code, details) {
    super(code);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

function notFound(code = 'not_found') {
  return new HttpError(404, code);
}

// --- API routes -----------------------------------------------------------

const routes = [];

/** Registers `handler(context)`; `:name` path segments become params. */
function route(method, pattern, handler) {
  const names = [];
  const source = pattern.replace(/:(\w+)/g, (_, name) => {
    names.push(name);
    return '([^/]+)';
  });
  routes.push({ method, regexp: new RegExp(`^${source}$`), names, handler });
}

function appOf({ state, params }) {
  const app = state.apps.get(params.slug);
  if (!app) throw notFound('app_not_found');
  return app;
}

function findDeployment(state, id) {
  for (const list of state.deployments.values()) {
    const found = list.find((entry) => entry.id === Number(id));
    if (found) return found;
  }
  throw notFound('deployment_not_found');
}

/** What the log of a deployment depends on besides the deployment. */
function logContext(state, entry) {
  const app = state.apps.get(entry.app_slug);
  const list = state.deployments.get(entry.app_slug) ?? [];
  const previous = list
    .slice(list.indexOf(entry) + 1)
    .find((older) => older.status === 'succeeded');
  return {
    slug: entry.app_slug,
    hostPort: app?.host_port ?? 20000,
    healthPath: app?.healthcheck_path ?? '/',
    backup: Boolean(app?.backup_before_deploy),
    previousId: previous?.id ?? null,
  };
}

function domainOf(context) {
  const app = appOf(context);
  const found = context.state.domains
    .get(app.slug)
    .find((entry) => entry.id === Number(context.params.id));
  if (!found) throw notFound('domain_not_found');
  return found;
}

function countPending(state, slug) {
  const pending = state.domains
    .get(slug)
    .filter((entry) => entry.status === 'pending');
  state.apps.get(slug).pending_domains = pending.length;
}

route('GET', '/api/system', ({ state }) => state.system);
route('GET', '/api/system/github', () => ({
  configured: true,
  app: {
    slug: 'koroku-octo',
    html_url: 'https://github.com/apps/koroku-octo',
    settings_url: 'https://github.com/settings/apps/koroku-octo',
    install_url: 'https://github.com/apps/koroku-octo/installations/new',
  },
  installations: [
    { account: 'octo', repository_selection: 'selected', used: true },
    { account: 'example-org', repository_selection: 'all', used: false },
  ],
  installation_accounts: [],
}));
route('GET', '/api/system/docker', ({ state }) => {
  if (state.backupsDown) throw new HttpError(500, 'internal');
  return dockerUsage(state);
});
route('GET', '/api/system/containers', async ({ state }) => {
  if (state.backupsDown) throw new HttpError(500, 'internal');
  // docker stats samples twice; the real call takes a couple of seconds.
  await new Promise((done) => setTimeout(done, 600));
  return containerResources(
    state,
    (target) => state.services?.get(target.slug) ?? services(target),
  );
});
route('POST', '/api/system/prune', async ({ state, body }) => {
  // Long enough to see the "in progress" state.
  await new Promise((done) => setTimeout(done, 1500));
  state.pruned = true;
  const lines = [];
  let reclaimed = 0;
  if (body.images !== false) {
    lines.push('$ docker image prune -f', 'Total reclaimed space: 3.1GB');
    reclaimed += 3.1 * 2 ** 30;
  }
  if (body.builder !== false) {
    lines.push(
      '$ docker builder prune --all -f --filter until=168h',
      'Total:\t4.2GB',
    );
    reclaimed += 4.2 * 2 ** 30;
  }
  return { output: lines.join('\n') + '\n', reclaimed_bytes: reclaimed };
});

route('GET', '/api/apps', ({ state }) => ({
  apps: [...state.apps.values()].sort((a, b) => a.slug.localeCompare(b.slug)),
}));
route('POST', '/api/apps', ({ state, body: { env, ...body }, scenario }) => {
  if (!/^[a-z][a-z0-9-]{0,39}$/.test(body.slug ?? '')) {
    throw new HttpError(422, 'validation_failed', { slug: 'is invalid' });
  }
  if (state.apps.has(body.slug)) throw new HttpError(409, 'slug_taken');
  // Variables saved with the app need a recent login (ARCHITECTURE 9.8).
  if (env?.length && scenario === 'stale-login') {
    throw new HttpError(403, 'reauth_required');
  }
  const app = newApp(body, 20000 + state.apps.size);
  state.apps.set(app.slug, app);
  if (env?.length) {
    state.env ??= new Map();
    state.env.set(
      app.slug,
      [...env].sort((a, b) => (a.key < b.key ? -1 : 1)),
    );
  }
  state.deployments.set(app.slug, []);
  state.domains.set(app.slug, []);
  return { status: 201, json: app };
});
route('GET', '/api/apps/:slug', appOf);
route('PATCH', '/api/apps/:slug', (context) => {
  const target = appOf(context);
  const { body } = context;
  // Making a production app deploy pushes needs a recent login (10.3).
  if (
    context.scenario === 'stale-login' &&
    target.environment === 'production' &&
    body.auto_deploy === true
  ) {
    throw new HttpError(403, 'reauth_required');
  }
  if (
    body.auto_deploy === true &&
    (!target.repo_full_name || target.repo_url)
  ) {
    throw new HttpError(422, 'validation_failed', {
      auto_deploy:
        'needs a repository fetched through the GitHub App (repo_full_name without repo_url)',
    });
  }
  const changes = Object.entries(body)
    .filter(
      ([field, value]) =>
        JSON.stringify(target[field] ?? '') !== JSON.stringify(value ?? ''),
    )
    .map(([field, to]) => ({ field, from: target[field] ?? '', to }));
  if (changes.length) {
    record(context, target.slug, 'app.settings_changed', { changes });
  }
  return Object.assign(target, body);
});
route('DELETE', '/api/apps/:slug', (context) => {
  context.state.apps.delete(appOf(context).slug);
  return { status: 204 };
});
for (const [action, desired, done] of [
  ['start', 'running', 'app.started'],
  ['stop', 'stopped', 'app.stopped'],
  ['restart', 'running', 'app.restarted'],
]) {
  route('POST', `/api/apps/:slug/${action}`, (context) => {
    const app = appOf(context);
    if (!app.current_deployment) throw new HttpError(409, 'not_deployed');
    app.desired_state = desired;
    record(context, app.slug, done);
    return app;
  });
}

/** Adds an activity entry by the scenario's user (ARCHITECTURE 10.7). */
function record(context, slug, action, details) {
  recordActivity(
    context.state,
    slug,
    userOf(context.scenario),
    action,
    details,
  );
}

function envChange(change) {
  return {
    added: [],
    changed: [],
    removed: [],
    renamed: [],
    replaced: false,
    ...change,
  };
}

route('GET', '/api/apps/:slug/activity', (context) =>
  activityPage(context.state, appOf(context).slug, context.query),
);
route('GET', '/api/apps/:slug/services', (context) => ({
  services:
    context.state.services?.get(context.params.slug) ??
    services(appOf(context)),
}));

route('GET', '/api/apps/:slug/memory', (context) => {
  const range = context.query.get('range') ?? '24h';
  if (!['24h', '7d', '14d'].includes(range)) {
    throw new HttpError(400, 'invalid_range');
  }
  // The app of `fresh` was deployed minutes ago: hours of data at most.
  const collectingHours = context.scenario === 'fresh' ? 3 : undefined;
  return memoryHistory(appOf(context), range, { collectingHours });
});

function savedEnv(context) {
  return context.state.env?.get(context.params.slug) ?? ENV_VARS;
}

function storeEnv(context, vars) {
  context.state.env ??= new Map();
  context.state.env.set(
    context.params.slug,
    [...vars].sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0)),
  );
}

/** The API's answer: the saved variables and how they differ from the release. */
function envList(context) {
  const app = appOf(context);
  const vars = savedEnv(context);
  const deployed = app.current_deployment
    ? (context.state.deployedEnv?.get(app.slug) ?? ENV_VARS)
    : null;
  const running = new Map((deployed ?? []).map((v) => [v.key, v.value]));
  // Below manage, a member sees the keys only (ARCHITECTURE 10.7).
  const forbidden = !atLeast(roleOf(context.scenario, app.slug), 'manage');
  const hidden = forbidden || context.scenario === 'stale-login';
  return {
    vars: vars.map(({ key, value }) => ({
      key,
      value: hidden ? null : value,
      pending: !deployed
        ? null
        : !running.has(key)
          ? 'added'
          : running.get(key) !== value
            ? 'changed'
            : null,
    })),
    removed: deployed
      ? [...running.keys()].filter((key) => !vars.some((v) => v.key === key))
      : [],
    compared: Boolean(deployed),
    values_hidden: hidden,
    values_forbidden: forbidden,
  };
}

route('GET', '/api/apps/:slug/env', envList);
route('PUT', '/api/apps/:slug/env', (context) => {
  const app = appOf(context);
  const before = new Map(savedEnv(context).map((v) => [v.key, v.value]));
  const vars = context.body.vars ?? [];
  storeEnv(context, vars);
  const after = new Map(vars.map((v) => [v.key, v.value]));
  record(
    context,
    app.slug,
    'env.changed',
    envChange({
      added: [...after.keys()].filter((key) => !before.has(key)),
      changed: [...after.keys()].filter(
        (key) => before.has(key) && before.get(key) !== after.get(key),
      ),
      removed: [...before.keys()].filter((key) => !after.has(key)),
      replaced: true,
    }),
  );
  return envList(context);
});
route('PUT', '/api/apps/:slug/env/:key', (context) => {
  appOf(context);
  const from = decodeURIComponent(context.params.key);
  const key = context.body.key ?? from;
  const vars = savedEnv(context);
  const current = vars.find((v) => v.key === from);
  if (key !== from) {
    if (!current) throw notFound('env_var_not_found');
    if (vars.some((v) => v.key === key)) {
      throw new HttpError(409, 'env_var_exists');
    }
  }
  const value = context.body.value ?? current?.value;
  if (value === undefined) {
    throw new HttpError(422, 'validation_failed', { value: 'is required' });
  }
  storeEnv(context, [
    ...vars.filter((v) => v.key !== from && v.key !== key),
    { key, value },
  ]);
  record(
    context,
    context.params.slug,
    'env.changed',
    envChange(
      key !== from
        ? { renamed: [{ from, to: key }] }
        : current
          ? { changed: [key] }
          : { added: [key] },
    ),
  );
  return envList(context);
});
route('DELETE', '/api/apps/:slug/env/:key', (context) => {
  appOf(context);
  const key = decodeURIComponent(context.params.key);
  const vars = savedEnv(context);
  if (!vars.some((v) => v.key === key)) throw notFound('env_var_not_found');
  storeEnv(
    context,
    vars.filter((v) => v.key !== key),
  );
  record(
    context,
    context.params.slug,
    'env.changed',
    envChange({ removed: [key] }),
  );
  return envList(context);
});

// The container registry credential (ARCHITECTURE 6.1). The production
// blog pulls its images from GHCR with one; the other apps have none. The
// token is kept only to tell whether one is set, and never sent back.
function registryOf(context) {
  const app = appOf(context);
  context.state.registry ??= new Map([
    [
      'blog',
      {
        registry: 'ghcr.io',
        username: 'octo-bot',
        token: 'set',
        updated_at: new Date(Date.now() - 2 * 86400000).toISOString(),
      },
    ],
  ]);
  return { app, saved: context.state.registry.get(app.slug) };
}

function registryJSON(saved) {
  return saved
    ? {
        configured: true,
        registry: saved.registry,
        username: saved.username,
        updated_at: saved.updated_at,
      }
    : {
        configured: false,
        registry: 'ghcr.io',
        username: '',
        updated_at: null,
      };
}

route('GET', '/api/apps/:slug/registry-credential', (context) =>
  registryJSON(registryOf(context).saved),
);
route('PUT', '/api/apps/:slug/registry-credential', (context) => {
  const { app, saved } = registryOf(context);
  if (context.scenario === 'stale-login') {
    throw new HttpError(403, 'reauth_required');
  }
  const registry = String(context.body.registry ?? '')
    .trim()
    .toLowerCase();
  const username = String(context.body.username ?? '').trim();
  const token = String(context.body.token ?? '').trim();
  const errors = {};
  if (!/^[a-z0-9.-]+\.[a-z0-9-]+(:\d+)?$|^localhost(:\d+)?$/.test(registry)) {
    errors.registry = 'enter the registry host';
  }
  if (!username || /[:\s]/.test(username))
    errors.username = 'enter the username';
  if (/\s/.test(token) || (!token && !saved)) errors.token = 'enter the token';
  if (Object.keys(errors).length) {
    throw new HttpError(422, 'validation_failed', errors);
  }
  const next = {
    registry,
    username,
    token: token || saved.token,
    updated_at: new Date().toISOString(),
  };
  context.state.registry.set(app.slug, next);
  return registryJSON(next);
});
route('DELETE', '/api/apps/:slug/registry-credential', (context) => {
  const { app, saved } = registryOf(context);
  if (context.scenario === 'stale-login') {
    throw new HttpError(403, 'reauth_required');
  }
  if (!saved) throw notFound('registry_credential_not_found');
  context.state.registry.delete(app.slug);
  return registryJSON(null);
});

/**
 * What the repository needs at a commit, compared with the saved keys. The
 * body carries a `status` field of its own, so it is sent as `json`.
 */
route('GET', '/api/apps/:slug/env/requirements', (context) => ({
  status: 200,
  json: envRequirements(context),
}));

function envRequirements(context) {
  const app = appOf(context);
  const source = app.env_source ?? 'compose';
  const sha = context.query.get('sha');
  const ref = sha ? '' : (context.query.get('ref') ?? app.default_branch);
  const head = {
    source,
    ref,
    commit_sha: sha ?? commits()[0].sha,
    path: '',
    reason: '',
    variables: [],
    unlisted: [],
  };
  if (source === 'off') return { ...head, status: 'off', commit_sha: '' };
  // Without the GitHub App, public repositories are read anonymously, so
  // a bare host compares as usual (ARCHITECTURE 9.4).
  const spec =
    context.state.requirements?.get(app.slug)?.[source] ??
    (source === 'compose'
      ? composeRequirements()
      : {
          status: 'unavailable',
          reason: 'file_not_found',
          path: app.env_example_path || '.env.example',
        });
  if (spec.status !== 'ok') return { ...head, ...spec, variables: [] };
  const saved = new Set(savedEnv(context).map((v) => v.key));
  const listed = new Set(spec.variables.map((v) => v.key));
  return {
    ...head,
    status: 'ok',
    path: spec.path,
    variables: spec.variables
      .map((v) => ({ ...v, set: saved.has(v.key) }))
      .sort((a, b) => (a.key < b.key ? -1 : 1)),
    unlisted: [...saved].filter((key) => !listed.has(key)).sort(),
  };
}

route('GET', '/api/apps/:slug/deployments', (context) => ({
  deployments: context.state.deployments.get(appOf(context).slug),
}));
route('POST', '/api/apps/:slug/deployments', (context) => {
  const app = appOf(context);
  // Deploying to production needs a recent login (ARCHITECTURE 10.3).
  if (context.scenario === 'stale-login' && app.environment === 'production') {
    throw new HttpError(403, 'reauth_required');
  }
  if (context.body.rollback_to !== undefined) {
    if (context.body.ref || context.body.sha) {
      throw new HttpError(422, 'validation_failed', {
        rollback_to: 'cannot be combined with ref or sha',
      });
    }
    // No build: the disk limit does not apply (ARCHITECTURE 7.1).
    const result = startRollback(
      context.state,
      app.slug,
      Number(context.body.rollback_to),
    );
    if (result.error === 'validation_failed') {
      throw new HttpError(422, 'validation_failed', {
        rollback_to: 'no such deployment of this app',
      });
    }
    if (result.error) throw new HttpError(422, result.error);
    return { status: 202, json: result.deployment };
  }
  const { disk, disk_limit_percent: limit } = context.state.system;
  if (disk.used_percent > limit) {
    throw new HttpError(507, 'disk_full', {
      used_percent: disk.used_percent,
      limit_percent: limit,
    });
  }
  const started = startDeployment(context.state, app.slug, context.body);
  if (!started) throw new HttpError(409, 'promotion_stale');
  started.actor = userOf(context.scenario);
  return { status: 202, json: started };
});
// The preview has a `status` field of its own: wrap it so that the router
// does not take it for the HTTP status.
route('GET', '/api/apps/:slug/promotion', (context) => ({
  status: 200,
  json: promotionPreview(context.state, appOf(context)),
}));
route('GET', '/api/deployments/:id', ({ state, params }) =>
  findDeployment(state, params.id),
);
route('POST', '/api/deployments/:id/cancel', ({ state, params }) => {
  const entry = findDeployment(state, params.id);
  if (entry.status !== 'running' && entry.status !== 'queued') {
    throw new HttpError(409, 'not_cancelable');
  }
  delete entry.mockFinishAt;
  entry.status = 'canceled';
  entry.finished_at = new Date().toISOString();
  return { status: 202, json: entry };
});
route('GET', '/api/deployments/:id/logs', ({ state, params, query }) => {
  const entry = findDeployment(state, params.id);
  const log = deploymentLog(entry, logContext(state, entry));
  if (query.get('format') === 'text') {
    return { text: log.lines.join('\n') + '\n' };
  }
  return {
    sse: log.lines,
    end: log.end,
    // What the deployment writes from here on, until it is over.
    next: log.next && (() => (settle(state), log.next())),
  };
});

// One-off commands and registered tasks (ARCHITECTURE 9.6).

function tasksOf(context) {
  const taskState = tasksState(context.state, context.scenario);
  settleTaskRuns(taskState);
  return taskState;
}

function findTaskRun(context) {
  for (const list of tasksOf(context).runs.values()) {
    const found = list.find((run) => run.id === Number(context.params.id));
    if (found) return found;
  }
  throw notFound('task_run_not_found');
}

/** A run without the mock's own bookkeeping, as the API returns it. */
function publicRun(run) {
  const rest = { ...run };
  delete rest.mock;
  return rest;
}

const SERVICE_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/;

route('GET', '/api/apps/:slug/tasks', (context) => ({
  tasks: tasksOf(context).tasks.get(appOf(context).slug),
}));
route('PUT', '/api/apps/:slug/tasks', (context) => {
  const app = appOf(context);
  const list = Array.isArray(context.body.tasks) ? context.body.tasks : null;
  const errors = {};
  if (!list) errors.tasks = 'is required';
  const seen = new Set();
  const tasks = (list ?? []).map((task, index) => {
    const name = String(task.name ?? '').trim();
    const service = String(task.service ?? '').trim();
    const command = String(task.command ?? '').trim();
    if (!name) errors[`tasks[${index}].name`] = 'is required';
    else if (seen.has(name))
      errors[`tasks[${index}].name`] = 'is used by another task';
    seen.add(name);
    if (!SERVICE_NAME.test(service))
      errors[`tasks[${index}].service`] = 'is not a valid service name';
    if (!command) errors[`tasks[${index}].command`] = 'is required';
    return { name, service, command };
  });
  if (tasks.length > 20) errors.tasks = 'at most 20 tasks can be registered';
  if (Object.keys(errors).length > 0) {
    throw new HttpError(422, 'validation_failed', errors);
  }
  tasksOf(context).tasks.set(app.slug, tasks);
  return { tasks };
});
route('GET', '/api/apps/:slug/task-runs', (context) => ({
  runs: tasksOf(context).runs.get(appOf(context).slug).map(publicRun),
}));
route('POST', '/api/apps/:slug/task-runs', (context) => {
  const app = appOf(context);
  const taskState = tasksOf(context);
  const { task, service, command } = context.body;
  let spec;
  if (task !== undefined) {
    const found = taskState.tasks.get(app.slug).find((t) => t.name === task);
    if (!found) throw notFound('task_not_found');
    spec = found;
  } else {
    const errors = {};
    if (!SERVICE_NAME.test(String(service ?? '')))
      errors.service = 'is not a valid service name';
    if (!String(command ?? '').trim()) errors.command = 'is required';
    if (Object.keys(errors).length > 0) {
      throw new HttpError(422, 'validation_failed', errors);
    }
    spec = { name: '', service, command: String(command).trim() };
  }
  if (!app.current_deployment) throw new HttpError(409, 'not_deployed');
  const deploying = ['queued', 'running'].includes(
    app.latest_deployment?.status,
  );
  const running = taskState.runs
    .get(app.slug)
    .some((run) => run.status === 'running');
  if (deploying || running) throw new HttpError(409, 'busy');
  const run = startTaskRun(taskState, app, spec);
  run.actor = userOf(context.scenario);
  record(context, app.slug, 'task_run.started', {
    run_id: run.id,
    task_name: run.task_name,
    service: run.service,
    command: run.command,
  });
  return { status: 202, json: publicRun(run) };
});
route('GET', '/api/task-runs/:id', (context) =>
  publicRun(findTaskRun(context)),
);
route('POST', '/api/task-runs/:id/cancel', (context) => {
  const run = findTaskRun(context);
  if (run.status !== 'running') throw new HttpError(409, 'not_cancelable');
  cancelTaskRun(run);
  return { status: 202, json: publicRun(run) };
});
route('GET', '/api/task-runs/:id/logs', (context) => {
  const run = findTaskRun(context);
  const log = taskRunLog(run, () => tasksOf(context));
  if (context.query.get('format') === 'text') {
    return { text: log.lines.join('\n') + '\n' };
  }
  return { sse: log.lines, end: log.end, next: log.next };
});

route('GET', '/api/apps/:slug/domains', (context) => ({
  default_hostname: `${appOf(context).slug}.${BASE_DOMAIN}`,
  domains: context.state.domains.get(context.params.slug),
}));
route('POST', '/api/apps/:slug/domains', (context) => {
  const { state, body } = context;
  const app = appOf(context);
  const hostname = String(body.hostname ?? '').toLowerCase();
  if (!hostname.includes('.') || hostname.endsWith(BASE_DOMAIN)) {
    throw new HttpError(422, 'validation_failed', { hostname: 'is invalid' });
  }
  const all = [...state.domains.values()].flat();
  if (all.some((entry) => entry.hostname === hostname)) {
    throw new HttpError(409, 'hostname_taken');
  }
  const created = newDomain(1 + all.length * 7, hostname, app.slug);
  state.domains.get(app.slug).push(created);
  countPending(state, app.slug);
  record(context, app.slug, 'domain.added', {
    hostname,
    status: created.status,
  });
  return { status: 201, json: created };
});
route('POST', '/api/apps/:slug/domains/:id/verify', (context) => {
  // The second check succeeds, as if the DNS record had just propagated.
  const found = domainOf(context);
  found.last_checked_at = new Date().toISOString();
  if (found.mockChecked) {
    found.status = 'active';
    found.last_error = '';
  }
  found.mockChecked = true;
  countPending(context.state, context.params.slug);
  return found;
});
route('PATCH', '/api/apps/:slug/domains/:id', (context) => {
  const { state, params, body } = context;
  const found = domainOf(context);
  const app = state.apps.get(params.slug);
  if (body.force_active !== undefined) {
    found.force_active = body.force_active;
    if (body.force_active) found.status = 'active';
  }
  if (body.is_primary !== undefined) {
    for (const entry of state.domains.get(params.slug)) {
      entry.is_primary = false;
    }
    found.is_primary = body.is_primary;
    app.primary_url = body.is_primary ? `https://${found.hostname}` : app.url;
  }
  countPending(state, params.slug);
  return found;
});
route('DELETE', '/api/apps/:slug/domains/:id', (context) => {
  const { state, params } = context;
  const found = domainOf(context);
  const list = state.domains.get(params.slug);
  list.splice(list.indexOf(found), 1);
  if (found.is_primary)
    state.apps.get(params.slug).primary_url = appOf(context).url;
  countPending(state, params.slug);
  record(context, params.slug, 'domain.deleted', {
    hostname: found.hostname,
  });
  return { status: 204 };
});

// Runtime logs: the live tail, the search and the archives (logs.mjs).
registerLogRoutes(route, { appOf, HttpError });

route('GET', '/api/backups', ({ state }) => {
  if (state.backupsDown) throw new HttpError(500, 'internal');
  return state.backups;
});
route('POST', '/api/backups', ({ state }) => {
  if (!state.backups.configured) throw new HttpError(503, 'not_configured');
  if (state.backups.running) throw new HttpError(409, 'busy');
  return { status: 202, json: startBackup(state) };
});
route('GET', '/api/apps/:slug/backups', (context) => {
  const { slug } = appOf(context);
  if (!context.state.backups.configured) {
    throw new HttpError(503, 'not_configured');
  }
  return { snapshots: context.state.snapshots?.get(slug) ?? snapshots() };
});
route('POST', '/api/apps/:slug/backups/restore', async (context) => {
  appOf(context);
  // The real restore stops the app, moves data and starts it again.
  await new Promise((done) => setTimeout(done, 2000));
  return { restored: true };
});
route('GET', '/api/apps/:slug/backups/:id/download', () => ({
  text: '-- mock database dump\n',
}));
// Listing volumes takes Docker only: it answers without backups, too.
route('GET', '/api/apps/:slug/volumes', (context) => {
  const app = appOf(context);
  if (context.state.volumesDown) throw new HttpError(500, 'internal');
  return { volumes: appVolumes(context.state, app) };
});

// Database copies into staging apps (db-copy.mjs).
function findCopy(state, id) {
  for (const list of state.dbCopies.values()) {
    const found = list.find((entry) => entry.id === Number(id));
    if (found) return found;
  }
  throw notFound('copy_not_found');
}

function copyJson(entry) {
  const { mockFinishAt, ...json } = entry;
  void mockFinishAt;
  return json;
}

route('GET', '/api/apps/:slug/db-copies', (context) => {
  const app = appOf(context);
  return {
    copies: (context.state.dbCopies.get(app.slug) ?? []).map(copyJson),
  };
});
route('POST', '/api/apps/:slug/db-copies', (context) => {
  const { state, body } = context;
  const app = appOf(context);
  if (app.environment !== 'staging') {
    throw new HttpError(422, 'target_not_staging');
  }
  if (body.confirm !== app.slug) {
    throw new HttpError(422, 'confirmation_mismatch');
  }
  const source = state.apps.get(body.source);
  if (!source) throw notFound('source_not_found');
  if (!app.load_hook_command) {
    throw new HttpError(422, 'target_load_hook_missing');
  }
  if ((body.source_kind ?? 'live') === 'live' && !source.dump_hook_command) {
    throw new HttpError(422, 'source_dump_hook_missing');
  }
  const list = state.dbCopies.get(app.slug) ?? [];
  if (list.some((entry) => entry.status === 'running')) {
    throw new HttpError(409, 'busy');
  }
  const entry = newCopy(state, app.slug, body);
  entry.actor = userOf(context.scenario);
  state.dbCopies.set(app.slug, [entry, ...list]);
  record(context, app.slug, 'db_copy.started', {
    copy_id: entry.id,
    source: entry.source_app,
    source_kind: entry.source_kind,
  });
  return { status: 202, json: copyJson(entry) };
});
route('GET', '/api/db-copies/:id', ({ state, params }) =>
  copyJson(findCopy(state, params.id)),
);
route('GET', '/api/db-copies/:id/logs', ({ state, params, query }) => {
  const entry = findCopy(state, params.id);
  const log = copyLog(entry, state.apps.get(entry.target_app));
  if (query.get('format') === 'text') {
    return { text: log.lines.join('\n') + '\n' };
  }
  return {
    sse: log.lines,
    end: log.end,
    next: log.next && (() => (settleCopies(state), log.next())),
  };
});

route('GET', '/api/repos', ({ state }) => {
  if (state.github === false) throw new HttpError(503, 'not_configured');
  return { repos: REPOS };
});
// Without the GitHub App the deploy form falls back to a typed ref.
route('GET', '/api/repos/:owner/:repo/branches', ({ state }) => {
  if (state.github === false) throw new HttpError(503, 'not_configured');
  return {
    branches: [
      { name: 'main', sha: commits()[0].sha },
      { name: 'next', sha: commits()[1].sha },
    ],
  };
});
route('GET', '/api/repos/:owner/:repo/commits', ({ state }) => {
  if (state.github === false) throw new HttpError(503, 'not_configured');
  return { commits: commits() };
});
route('GET', '/api/repos/:owner/:repo/compose', () => COMPOSE);
// Secrets are generated on every request, as koroku does.
route('GET', '/api/repos/:owner/:repo/env-presets', ({ state, params }) => {
  const suggestion = ENV_PRESETS[`${params.owner}/${params.repo}`];
  if (state.github === false || !suggestion) {
    return {
      status: 'unavailable',
      reason: state.github === false ? 'not_configured' : 'repo_unavailable',
      compose_path: '',
      frameworks: [],
      presets: [],
      skipped: [],
    };
  }
  const generate = (kind) =>
    kind === 'laravel'
      ? `base64:${randomBytes(32).toString('base64')}`
      : randomBytes(64).toString('hex');
  return {
    status: 'ok',
    reason: '',
    ...suggestion,
    presets: suggestion.presets.map(({ secret, ...preset }) => ({
      ...preset,
      match: preset.match ?? '',
      value: secret ? generate(secret) : preset.value,
      generated: Boolean(secret),
    })),
  };
});

// --- HTTP plumbing --------------------------------------------------------

function cookie(request, name) {
  const pairs = (request.headers.cookie ?? '').split(';');
  for (const pair of pairs) {
    const [key, value] = pair.trim().split('=');
    if (key === name) return decodeURIComponent(value ?? '');
  }
  return undefined;
}

function scenarioCookie(name) {
  return `mock_scenario=${encodeURIComponent(name)}; Path=/; SameSite=Lax`;
}

function sendJson(response, status, value, headers = {}) {
  response.writeHead(status, {
    'Content-Type': 'application/json',
    'Cache-Control': 'no-store',
    ...headers,
  });
  response.end(value === undefined ? '' : JSON.stringify(value));
}

function sendError(response, error) {
  sendJson(response, error.status, {
    error: {
      code: error.code,
      message: `${error.code} (from the mock API)`,
      details: error.details,
    },
  });
}

/** Server-sent events: the given lines, then `end`, a feed or a live tail. */
function sendEvents(request, response, { sse, end, live, next }) {
  response.writeHead(200, {
    'Content-Type': 'text/event-stream',
    'Cache-Control': 'no-store',
  });
  // The stream is open even before there is a first line to send.
  response.flushHeaders();
  let offset = 0;
  const write = (line) => {
    offset += Buffer.byteLength(line) + 1;
    response.write(`id: ${offset}\ndata: ${line}\n\n`);
  };
  sse.forEach(write);
  if (end !== undefined) {
    response.end(`event: end\ndata: ${end}\n\n`);
    return;
  }
  if (next) {
    // A deployment in progress: pass on what it writes until it is over.
    const feed = setInterval(() => {
      const { lines, end: status } = next();
      lines.forEach(write);
      if (status === undefined) return;
      clearInterval(feed);
      response.end(`event: end\ndata: ${status}\n\n`);
    }, 500);
    request.on('close', () => clearInterval(feed));
    return;
  }
  // Still running (or a live tail): keep the stream open and trickle lines.
  // A live feed may have nothing to say (`undefined`) on a tick.
  const timer = setInterval(() => {
    const line = live ? live() : `#${offset} still working...`;
    if (line !== undefined) write(line);
  }, 2000);
  request.on('close', () => clearInterval(timer));
}

async function readBody(request) {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  if (chunks.length === 0) return {};
  try {
    return JSON.parse(Buffer.concat(chunks).toString('utf8')) ?? {};
  } catch {
    throw new HttpError(400, 'invalid_json');
  }
}

// Members and roles (ARCHITECTURE 10.7). The `member` scenario logs in as
// MEMBER with MEMBER_ROLES; every other scenario as the owner.

const RANK = { view: 1, deploy: 2, manage: 3, owner: 4 };

function roleOf(scenario, slug) {
  if (scenario !== 'member') return 'owner';
  return MEMBER_ROLES[slug] ?? null;
}

function atLeast(role, needed) {
  return (RANK[role] ?? 0) >= RANK[needed];
}

function userOf(scenario) {
  return scenario === 'member' ? MEMBER : OWNER;
}

/**
 * What each route needs, as the server's route table says: `owner`, or a
 * role on the app in the path. Routes not listed are open to every user.
 */
const ACCESS = [
  ['owner', 'GET', /^\/api\/members$/],
  ['owner', '*', /^\/api\/members\//],
  ['owner', 'POST', /^\/api\/members$/],
  ['owner', 'GET', /^\/api\/system\/./],
  ['owner', 'POST', /^\/api\/system\//],
  ['owner', 'GET', /^\/api\/repos$/],
  ['owner', 'GET', /^\/api\/repos\/[^/]+\/[^/]+\/(compose|env-presets)$/],
  ['owner', 'POST', /^\/api\/apps$/],
  ['owner', 'DELETE', /^\/api\/apps\/[^/]+$/],
  [
    'deploy',
    'POST',
    /^\/api\/apps\/([^/]+)\/(start|stop|restart|deployments)$/,
  ],
  ['deploy', 'GET', /^\/api\/apps\/([^/]+)\/promotion$/],
  ['manage', 'PATCH', /^\/api\/apps\/([^/]+)$/],
  ['manage', 'PUT', /^\/api\/apps\/([^/]+)\/(env|tasks)(\/.*)?$/],
  ['manage', 'DELETE', /^\/api\/apps\/([^/]+)\/env\//],
  ['manage', 'PUT', /^\/api\/apps\/([^/]+)\/registry-credential$/],
  ['manage', 'DELETE', /^\/api\/apps\/([^/]+)\/registry-credential$/],
  [
    'manage',
    'POST',
    /^\/api\/apps\/([^/]+)\/(domains|task-runs|db-copies|backups\/restore)(\/.*)?$/,
  ],
  ['manage', 'PATCH', /^\/api\/apps\/([^/]+)\/domains\//],
  ['manage', 'DELETE', /^\/api\/apps\/([^/]+)\/domains\//],
  ['manage', 'GET', /^\/api\/apps\/([^/]+)\/backups\/[^/]+\/download$/],
  ['view', 'GET', /^\/api\/apps\/([^/]+)(\/.*)?$/],
];

/** Refuses what the member scenario's roles do not allow. */
function checkAccess(scenario, method, path) {
  if (scenario !== 'member') return;
  for (const [needed, m, pattern] of ACCESS) {
    if (m !== '*' && m !== method) continue;
    const match = pattern.exec(path);
    if (!match) continue;
    if (needed === 'owner') {
      throw new HttpError(403, 'forbidden_role', { required: 'owner' });
    }
    const role = roleOf(scenario, decodeURIComponent(match[1]));
    if (!role) throw notFound('app_not_found');
    if (!atLeast(role, needed)) {
      throw new HttpError(403, 'forbidden_role', { required: needed });
    }
    return;
  }
}

/** Adds `role` to app JSON and leaves out the apps a member has none on. */
function withRoles(scenario, result) {
  const decorate = (app) => ({ ...app, role: roleOf(scenario, app.slug) });
  if (Array.isArray(result?.apps)) {
    return {
      ...result,
      apps: result.apps
        .filter((app) => roleOf(scenario, app.slug))
        .map(decorate),
    };
  }
  if (result && typeof result.slug === 'string' && 'environment' in result) {
    return decorate(result);
  }
  if (result?.json && typeof result.json.slug === 'string') {
    return { ...result, json: decorate(result.json) };
  }
  return result;
}

function membersOf(context) {
  context.state.members ??= members(context.scenario);
  return context.state.members;
}

function memberOf(context) {
  const found = membersOf(context).find(
    (m) => m.github_user_id === Number(context.params.id),
  );
  if (!found) throw notFound('member_not_found');
  return found;
}

route('GET', '/api/members', (context) => ({
  owner: { github_user_id: OWNER.id },
  members: membersOf(context),
}));
route('POST', '/api/members', (context) => {
  const login = String(context.body.login ?? '').replace(/^@/, '');
  if (!/^[A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38}$/.test(login)) {
    throw new HttpError(422, 'validation_failed', {
      login: 'is not a GitHub user name',
    });
  }
  if (login.toLowerCase() === OWNER.login) throw new HttpError(422, 'is_owner');
  if (login.toLowerCase() === 'nobody') throw notFound('user_not_found');
  const list = membersOf(context);
  if (list.some((m) => m.login.toLowerCase() === login.toLowerCase())) {
    throw new HttpError(409, 'member_exists');
  }
  const member = {
    github_user_id: 1000 + list.length,
    login,
    avatar_url: '',
    added_at: new Date().toISOString(),
    roles: [],
  };
  list.push(member);
  return { status: 201, json: member };
});
route('DELETE', '/api/members/:id', (context) => {
  const member = memberOf(context);
  context.state.members = membersOf(context).filter((m) => m !== member);
  return { status: 204 };
});
route('PUT', '/api/members/:id/roles/:slug', (context) => {
  const member = memberOf(context);
  appOf(context);
  if (!['view', 'deploy', 'manage'].includes(context.body.role)) {
    throw new HttpError(422, 'validation_failed', { role: 'is invalid' });
  }
  member.roles = [
    ...member.roles.filter((r) => r.app !== context.params.slug),
    { app: context.params.slug, role: context.body.role },
  ].sort((a, b) => a.app.localeCompare(b.app));
  return member;
});
route('DELETE', '/api/members/:id/roles/:slug', (context) => {
  const member = memberOf(context);
  member.roles = member.roles.filter((r) => r.app !== context.params.slug);
  return member;
});

// Sessions and login history (ARCHITECTURE 10.3). Made-up documentation
// addresses only.
const CHROME_MAC =
  'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36';
const SAFARI_IPHONE =
  'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1';
const FIREFOX_LINUX =
  'Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0';

function ago(ms) {
  return new Date(Date.now() - ms).toISOString();
}

const HOUR = 60 * 60 * 1000;
const DAY = 24 * HOUR;

function sessionsOf(state, scenario) {
  const me = userOf(scenario);
  const own = [
    {
      id: '9f2c41d07a1b3e55',
      created_at: ago(3 * HOUR),
      last_seen_at: ago(0),
      expires_at: ago(3 * HOUR - 7 * DAY),
      ip: '203.0.113.5',
      user_agent: CHROME_MAC,
      current: true,
    },
    {
      id: 'aa00bb11cc22dd33',
      created_at: ago(2 * DAY),
      last_seen_at: ago(5 * HOUR),
      expires_at: ago(2 * DAY - 7 * DAY),
      ip: '198.51.100.7',
      user_agent: SAFARI_IPHONE,
      current: false,
    },
    {
      id: '4e5f60718293a4b5',
      created_at: ago(5 * DAY),
      last_seen_at: ago(4 * DAY),
      expires_at: ago(5 * DAY - 7 * DAY),
      ip: '192.0.2.88',
      user_agent: FIREFOX_LINUX,
      current: false,
    },
  ].map((s) => ({ ...s, user: me }));
  // The owner also sees the members' sessions (ARCHITECTURE 10.7).
  const others =
    scenario === 'member' || scenario === 'empty'
      ? []
      : [
          {
            id: '77aa66bb55cc44dd',
            created_at: ago(9 * HOUR),
            last_seen_at: ago(40 * 60 * 1000),
            expires_at: ago(9 * HOUR - 7 * DAY),
            ip: '198.51.100.23',
            user_agent: FIREFOX_LINUX,
            current: false,
            user: MEMBER,
          },
        ];
  state.sessions ??= [...own, ...others];
  return state.sessions;
}

route('GET', '/api/sessions', (context) => ({
  sessions: sessionsOf(context.state, context.scenario),
}));
route('DELETE', '/api/sessions/:id', (context) => {
  const sessions = sessionsOf(context.state, context.scenario);
  if (!sessions.some((s) => s.id === context.params.id)) {
    throw notFound('session_not_found');
  }
  context.state.sessions = sessions.filter((s) => s.id !== context.params.id);
  return { status: 204 };
});
route('POST', '/api/sessions/revoke-others', (context) => {
  const me = userOf(context.scenario);
  const sessions = sessionsOf(context.state, context.scenario);
  context.state.sessions = sessions.filter(
    (s) => s.current || s.user.id !== me.id,
  );
  return { revoked: sessions.length - context.state.sessions.length };
});
route('GET', '/api/login-events', (context) => ({
  events: [
    {
      at: ago(9 * HOUR),
      outcome: 'succeeded',
      github_user_id: MEMBER.id,
      login: MEMBER.login,
      ip: '198.51.100.23',
      user_agent: FIREFOX_LINUX,
    },
    {
      at: ago(3 * HOUR),
      outcome: 'succeeded',
      github_user_id: 1,
      login: 'octo',
      ip: '203.0.113.5',
      user_agent: CHROME_MAC,
    },
    {
      at: ago(26 * HOUR),
      outcome: 'not_allowed',
      github_user_id: 9001,
      login: 'someone-else',
      ip: '192.0.2.44',
      user_agent: FIREFOX_LINUX,
    },
    {
      at: ago(2 * DAY),
      outcome: 'succeeded',
      github_user_id: 1,
      login: 'octo',
      ip: '198.51.100.7',
      user_agent: SAFARI_IPHONE,
    },
  ].filter(
    (ev) => context.scenario !== 'member' || ev.github_user_id === MEMBER.id,
  ),
}));

// Read-only API tokens (ARCHITECTURE 10.5). The values are made up and
// match the documented shape only.
function tokensOf(state, scenario) {
  const me = userOf(scenario);
  state.tokens ??=
    scenario === 'empty'
      ? []
      : [
          {
            id: '3fa9c2d17b40e815',
            name: 'ノート PC の Claude Code',
            created_at: ago(6 * DAY),
            expires_at: ago(6 * DAY - 30 * DAY),
            last_used_at: ago(2 * HOUR),
            last_used_ip: '203.0.113.5',
          },
          {
            id: '0b7e14c9d2a35f68',
            name: 'デスクトップ',
            created_at: ago(20 * DAY),
            expires_at: ago(20 * DAY - 90 * DAY),
            last_used_at: null,
            last_used_ip: '',
          },
        ].map((t) => ({ ...t, user: me }));
  return state.tokens;
}

function mockTokenValue() {
  const alphabet =
    'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-';
  let out = 'koroku_ro_';
  for (let i = 0; i < 43; i++) {
    out += alphabet[Math.floor(Math.random() * alphabet.length)];
  }
  return out;
}

route('GET', '/api/tokens', (context) => ({
  tokens: tokensOf(context.state, context.scenario),
}));
route('POST', '/api/tokens', (context) => {
  const name = String(context.body.name ?? '').trim();
  const days = context.body.expires_in_days;
  const details = {};
  if (name === '' || name.length > 100) details.name = 'is required';
  if (![7, 30, 90].includes(days))
    details.expires_in_days = 'must be 7, 30 or 90';
  if (Object.keys(details).length > 0) {
    throw new HttpError(422, 'validation_failed', details);
  }
  const token = {
    id: Math.random().toString(16).slice(2, 18).padEnd(16, '0'),
    name,
    created_at: ago(0),
    expires_at: ago(-days * DAY),
    last_used_at: null,
    last_used_ip: '',
    user: userOf(context.scenario),
  };
  const tokens = tokensOf(context.state, context.scenario);
  context.state.tokens = [token, ...tokens];
  return { status: 201, json: { token, value: mockTokenValue() } };
});
route('DELETE', '/api/tokens/:id', (context) => {
  const tokens = tokensOf(context.state, context.scenario);
  if (!tokens.some((t) => t.id === context.params.id)) {
    throw notFound('token_not_found');
  }
  context.state.tokens = tokens.filter((t) => t.id !== context.params.id);
  return { status: 204 };
});

/** What the API refuses without a recent login (ARCHITECTURE 10.3). */
const NEEDS_RECENT_LOGIN = [
  ['POST', /^\/api\/tokens$/],
  ['DELETE', /^\/api\/apps\/[^/]+$/],
  ['PUT', /^\/api\/apps\/[^/]+\/env(\/[^/]+)?$/],
  ['DELETE', /^\/api\/apps\/[^/]+\/env\/[^/]+$/],
  ['POST', /^\/api\/apps\/[^/]+\/backups\/restore$/],
  ['GET', /^\/api\/apps\/[^/]+\/backups\/[^/]+\/download$/],
  ['POST', /^\/api\/apps\/[^/]+\/db-copies$/],
  // The production apps of the scenarios (blog, shop).
  ['PUT', /^\/api\/apps\/(blog|shop)\/tasks$/],
  ['POST', /^\/api\/apps\/(blog|shop)\/task-runs$/],
];

async function handleApi(request, response, url, scenario) {
  if (url.pathname === '/api/session') {
    const user = {
      ...userOf(scenario),
      avatar_url: '',
      owner: scenario !== 'member',
    };
    const reauthUntil =
      scenario === 'stale-login'
        ? ago(3 * HOUR - 10 * 60 * 1000)
        : new Date(Date.now() + 7 * 60 * 1000).toISOString();
    return sendJson(
      response,
      200,
      scenario === 'login'
        ? { authenticated: false }
        : { authenticated: true, user, reauth_until: reauthUntil },
    );
  }
  if (
    scenario === 'stale-login' &&
    NEEDS_RECENT_LOGIN.some(
      ([method, path]) => request.method === method && path.test(url.pathname),
    )
  ) {
    throw new HttpError(403, 'reauth_required');
  }
  checkAccess(scenario, request.method, url.pathname);
  if (scenario === 'loading') {
    await new Promise((done) => setTimeout(done, SLOW));
  }
  if (scenario === 'error') throw new HttpError(500, 'internal');
  // The app itself loads; whatever one of its tabs asks for does not.
  const forTab = !/^\/api\/apps(\/[^/]+)?$/.test(url.pathname);
  if (scenario === 'tab-loading' && forTab) {
    await new Promise((done) => setTimeout(done, SLOW));
  }
  if (scenario === 'tab-error' && forTab) throw new HttpError(500, 'internal');

  for (const { method, regexp, names, handler } of routes) {
    const match = request.method === method && regexp.exec(url.pathname);
    if (!match) continue;
    const params = Object.fromEntries(
      names.map((name, index) => [name, decodeURIComponent(match[index + 1])]),
    );
    const body = method === 'GET' ? {} : await readBody(request);
    const raw = await handler({
      state: stateOf(scenario),
      scenario,
      params,
      query: url.searchParams,
      body,
    });
    const result = withRoles(scenario, raw);
    if (result?.sse) return sendEvents(request, response, result);
    if (result?.text !== undefined) {
      response.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8' });
      return response.end(result.text);
    }
    // A numeric status picks the HTTP status; a string one is part of the body.
    if (typeof result?.status === 'number') {
      return sendJson(response, result.status, result.json);
    }
    return sendJson(response, 200, result);
  }
  throw notFound();
}

/** `/auth/*`: logging in and out just switches between scenarios. */
function handleAuth(request, response, url) {
  if (url.pathname === '/auth/logout' && request.method === 'POST') {
    response.writeHead(204, { 'Set-Cookie': scenarioCookie('login') });
    return response.end();
  }
  if (url.pathname === '/auth/github/login') {
    response.writeHead(302, {
      Location: '/',
      'Set-Cookie': scenarioCookie(DEFAULT_SCENARIO),
    });
    return response.end();
  }
  throw notFound();
}

/** Helpers of the mock itself, outside the paths koroku serves. */
function handleMock(response, url) {
  if (url.pathname === '/__mock/theme.js') {
    // Presets the theme the way the theme switch would store it. An
    // external script, because the console forbids inline ones.
    const theme = url.searchParams.get('theme');
    const pinned = theme === 'light' || theme === 'dark';
    response.writeHead(200, { 'Content-Type': MIME['.js'] });
    return response.end(
      pinned
        ? `localStorage.setItem('koroku.theme', '${theme}');`
        : `localStorage.removeItem('koroku.theme');`,
    );
  }
  if (url.pathname === '/__mock/reset') {
    states.clear();
    return sendJson(response, 200, { reset: true });
  }
  if (url.pathname === '/__mock/download') {
    response.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8' });
    return response.end('mock file\n');
  }
  return sendJson(response, 200, {
    scenarios: { ...SCENARIOS, ...LOG_SCENARIOS, ...TASK_SCENARIOS },
    usage: '/?scenario=<name>&theme=<system|light|dark>',
  });
}

async function sendFile(response, file, headers = {}) {
  const content = await readFile(file);
  response.writeHead(200, {
    'Content-Type': MIME[extname(file)] ?? 'application/octet-stream',
    'Cache-Control': 'no-store',
    ...headers,
  });
  response.end(content);
}

/** The built app; any path that is not a file gets index.html (SPA). */
async function handleStatic(response, url) {
  const file = resolve(join(DIST, decodeURIComponent(url.pathname)));
  const inside = file === DIST || file.startsWith(DIST + sep);
  if (inside && (await stat(file).catch(() => null))?.isFile()) {
    return sendFile(response, file);
  }

  let html;
  try {
    html = await readFile(join(DIST, 'index.html'), 'utf8');
  } catch {
    response.writeHead(503, { 'Content-Type': 'text/plain; charset=utf-8' });
    return response.end('web/dist is empty. Run `pnpm build` first.\n');
  }
  const headers = {
    'Content-Type': MIME['.html'],
    'Cache-Control': 'no-store',
    'Content-Security-Policy': CONTENT_SECURITY_POLICY,
  };
  const scenario = url.searchParams.get('scenario');
  if (scenario) headers['Set-Cookie'] = scenarioCookie(scenario);
  const theme = url.searchParams.get('theme');
  if (theme) {
    const src = `/__mock/theme.js?theme=${encodeURIComponent(theme)}`;
    html = html.replace('<head>', `<head><script src="${src}"></script>`);
  }
  response.writeHead(200, headers);
  response.end(html);
}

const server = createServer((request, response) => {
  const url = new URL(request.url ?? '/', `http://${HOST}:${PORT}`);
  // The page URL names the scenario; the API calls it makes carry it on in
  // a cookie.
  const scenario = cookie(request, 'mock_scenario') ?? DEFAULT_SCENARIO;

  let handled;
  if (url.pathname.startsWith('/api/')) {
    handled = handleApi(request, response, url, scenario);
  } else if (url.pathname.startsWith('/auth/')) {
    handled = Promise.resolve().then(() => handleAuth(request, response, url));
  } else if (url.pathname.startsWith('/__mock')) {
    handled = Promise.resolve().then(() => handleMock(response, url));
  } else {
    handled = handleStatic(response, url);
  }

  handled.catch((error) => {
    if (response.headersSent) return response.end();
    if (error instanceof HttpError) return sendError(response, error);
    console.error(error);
    sendError(response, new HttpError(500, 'internal'));
  });
});

server.listen(PORT, HOST, () => {
  console.log(
    `mock koroku: http://${HOST}:${PORT}/  (scenario: ${DEFAULT_SCENARIO})`,
  );
  console.log(
    `scenarios: ${Object.keys({ ...SCENARIOS, ...LOG_SCENARIOS, ...TASK_SCENARIOS }).join(', ')}`,
  );
});
