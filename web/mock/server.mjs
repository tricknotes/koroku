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

import { createServer } from 'node:http';
import { readFile, stat } from 'node:fs/promises';
import { dirname, extname, join, resolve, sep } from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  BASE_DOMAIN,
  COMPOSE,
  ENV_VARS,
  REPOS,
  SCENARIOS,
  appVolumes,
  buildScenario,
  commits,
  dockerUsage,
  newApp,
  newDomain,
  services,
  settle,
  settleBackups,
  snapshots,
  startBackup,
  startDeployment,
} from './fixtures.mjs';
import { deploymentLog } from './deploy-log.mjs';
import { LOG_SCENARIOS, registerLogRoutes } from './logs.mjs';

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
  if (!states.has(name)) states.set(name, buildScenario(name));
  const state = states.get(name);
  settle(state);
  settleBackups(state);
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
route('GET', '/api/system/docker', ({ state }) => {
  if (state.backupsDown) throw new HttpError(500, 'internal');
  return dockerUsage(state);
});
route('POST', '/api/system/prune', async ({ state, body }) => {
  // Long enough to see the "in progress" state.
  await new Promise((done) => setTimeout(done, 1500));
  state.pruned = true;
  const lines = [];
  if (body.images !== false) {
    lines.push('$ docker image prune -f', 'Total reclaimed space: 3.1GB');
  }
  if (body.builder !== false) {
    lines.push(
      '$ docker builder prune -f --filter until=168h',
      'Total reclaimed space: 4.2GB',
    );
  }
  return { output: lines.join('\n') + '\n' };
});

route('GET', '/api/apps', ({ state }) => ({
  apps: [...state.apps.values()].sort((a, b) => a.slug.localeCompare(b.slug)),
}));
route('POST', '/api/apps', ({ state, body }) => {
  if (!/^[a-z][a-z0-9-]{0,39}$/.test(body.slug ?? '')) {
    throw new HttpError(422, 'validation_failed', { slug: 'is invalid' });
  }
  if (state.apps.has(body.slug)) throw new HttpError(409, 'slug_taken');
  const app = newApp(body, 20000 + state.apps.size);
  state.apps.set(app.slug, app);
  state.deployments.set(app.slug, []);
  state.domains.set(app.slug, []);
  return { status: 201, json: app };
});
route('GET', '/api/apps/:slug', appOf);
route('PATCH', '/api/apps/:slug', (context) =>
  Object.assign(appOf(context), context.body),
);
route('DELETE', '/api/apps/:slug', (context) => {
  context.state.apps.delete(appOf(context).slug);
  return { status: 204 };
});
for (const [action, desired] of [
  ['start', 'running'],
  ['stop', 'stopped'],
  ['restart', 'running'],
]) {
  route('POST', `/api/apps/:slug/${action}`, (context) => {
    const app = appOf(context);
    if (!app.current_deployment) throw new HttpError(409, 'not_deployed');
    app.desired_state = desired;
    return app;
  });
}
route('GET', '/api/apps/:slug/services', (context) => ({
  services:
    context.state.services?.get(context.params.slug) ??
    services(appOf(context)),
}));

route('GET', '/api/apps/:slug/env', (context) => {
  appOf(context);
  const vars = context.state.env?.get(context.params.slug) ?? ENV_VARS;
  if (context.scenario === 'stale-login') {
    return {
      vars: vars.map(({ key }) => ({ key, value: null })),
      values_hidden: true,
    };
  }
  return { vars, values_hidden: false };
});
route('PUT', '/api/apps/:slug/env', (context) => {
  appOf(context);
  context.state.env ??= new Map();
  context.state.env.set(context.params.slug, context.body.vars ?? []);
  return { vars: context.body.vars ?? [] };
});

route('GET', '/api/apps/:slug/deployments', (context) => ({
  deployments: context.state.deployments.get(appOf(context).slug),
}));
route('POST', '/api/apps/:slug/deployments', (context) => {
  const app = appOf(context);
  const { disk, disk_limit_percent: limit } = context.state.system;
  if (disk.used_percent > limit) {
    throw new HttpError(507, 'disk_full', {
      used_percent: disk.used_percent,
      limit_percent: limit,
    });
  }
  return {
    status: 202,
    json: startDeployment(context.state, app.slug, context.body),
  };
});
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

route('GET', '/api/repos', ({ state }) => {
  if (state.github === false) throw new HttpError(503, 'not_configured');
  return { repos: REPOS };
});
route('GET', '/api/repos/:owner/:repo/branches', () => ({
  branches: [
    { name: 'main', sha: commits()[0].sha },
    { name: 'next', sha: commits()[1].sha },
  ],
}));
route('GET', '/api/repos/:owner/:repo/commits', () => ({ commits: commits() }));
route('GET', '/api/repos/:owner/:repo/compose', () => COMPOSE);

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

function sessionsOf(state) {
  state.sessions ??= [
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
  ];
  return state.sessions;
}

route('GET', '/api/sessions', (context) => ({
  sessions: sessionsOf(context.state),
}));
route('DELETE', '/api/sessions/:id', (context) => {
  const sessions = sessionsOf(context.state);
  if (!sessions.some((s) => s.id === context.params.id)) {
    throw notFound('session_not_found');
  }
  context.state.sessions = sessions.filter((s) => s.id !== context.params.id);
  return { status: 204 };
});
route('POST', '/api/sessions/revoke-others', (context) => {
  const sessions = sessionsOf(context.state);
  context.state.sessions = sessions.filter((s) => s.current);
  return { revoked: sessions.length - context.state.sessions.length };
});
route('GET', '/api/login-events', () => ({
  events: [
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
  ],
}));

/** What the API refuses without a recent login (ARCHITECTURE 10.3). */
const NEEDS_RECENT_LOGIN = [
  ['DELETE', /^\/api\/apps\/[^/]+$/],
  ['PUT', /^\/api\/apps\/[^/]+\/env$/],
  ['POST', /^\/api\/apps\/[^/]+\/backups\/restore$/],
  ['GET', /^\/api\/apps\/[^/]+\/backups\/[^/]+\/download$/],
];

async function handleApi(request, response, url, scenario) {
  if (url.pathname === '/api/session') {
    const user = { id: 1, login: 'octo', avatar_url: '' };
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
    const result = await handler({
      state: stateOf(scenario),
      scenario,
      params,
      query: url.searchParams,
      body,
    });
    if (result?.sse) return sendEvents(request, response, result);
    if (result?.text !== undefined) {
      response.writeHead(200, { 'Content-Type': 'text/plain; charset=utf-8' });
      return response.end(result.text);
    }
    if (result?.status) return sendJson(response, result.status, result.json);
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
    scenarios: { ...SCENARIOS, ...LOG_SCENARIOS },
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
    `scenarios: ${Object.keys({ ...SCENARIOS, ...LOG_SCENARIOS }).join(', ')}`,
  );
});
