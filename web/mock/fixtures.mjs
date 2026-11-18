// Fixture data of the mock API (see server.mjs). Every scenario is built
// fresh relative to "now", so relative times always look alive. Nothing
// here is real: names, domains and addresses are placeholders.

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const GB = 1024 ** 3;

export const BASE_DOMAIN = 'dev.example.com';
export const PUBLIC_IP = '203.0.113.10';

/** Age of the last good backup in the `warnings` scenario. */
const STALE_BACKUP = 2 * DAY + 3 * HOUR;

/** What each scenario shows; listed by `GET /__mock`. */
export const SCENARIOS = {
  healthy: 'four apps, everything fine',
  warnings:
    'a deployment in progress, a failed one, a stopped app, a pending domain, disk above the limit, stale backup',
  empty: 'no apps yet',
  undeployed: 'one app that was created and never deployed',
  error: 'every API call fails with 500',
  loading: 'API calls take a minute to answer',
  login: 'not logged in',
};

function ago(ms) {
  return new Date(Date.now() - ms).toISOString();
}

/** A stable, made-up 40-digit commit SHA. */
function sha(seed) {
  let value = (seed * 7919 + 104729) % 2147483647;
  let out = '';
  for (let i = 0; i < 40; i++) {
    value = (value * 48271) % 2147483647;
    out += (value % 16).toString(16);
  }
  return out;
}

let nextDeploymentId = 100;

function deployment(slug, message, finishedAgo, overrides = {}) {
  const id = nextDeploymentId++;
  const duration = 50 * 1000 + (id % 7) * 9000;
  return {
    id,
    app_slug: slug,
    ref: 'main',
    commit_sha: sha(id),
    commit_message: message,
    status: 'succeeded',
    error: '',
    queued_at: ago(finishedAgo + duration + 1000),
    started_at: ago(finishedAgo + duration),
    finished_at: ago(finishedAgo),
    ...overrides,
  };
}

function app(slug, overrides = {}) {
  const production = overrides.environment === 'production';
  const url = `https://${slug}.${BASE_DOMAIN}`;
  return {
    slug,
    repo_full_name: `octo/${slug.replace(/-staging$/, '')}`,
    repo_url: '',
    default_branch: 'main',
    compose_path: '',
    web_service: 'web',
    web_port: 3000,
    host_port: 20000,
    healthcheck_path: '/up',
    desired_state: 'running',
    environment: 'staging',
    relax_policy: false,
    memory_limit: '',
    service_memory_limits: { db: '1g' },
    backup_exclude_volumes: ['pgdata'],
    dump_hook_service: 'db',
    dump_hook_command: 'pg_dump -U postgres app',
    backup_before_deploy: production,
    url,
    primary_url: url,
    pending_domains: 0,
    current_deployment: null,
    latest_deployment: null,
    created_at: ago(90 * DAY),
    updated_at: ago(2 * DAY),
    ...overrides,
  };
}

function domain(id, hostname, slug, overrides = {}) {
  const apex = hostname.split('.').length === 2;
  return {
    id,
    hostname,
    status: 'active',
    is_primary: false,
    force_active: false,
    last_checked_at: ago(4 * MINUTE),
    last_error: '',
    records: apex
      ? [{ type: 'A', name: hostname, value: PUBLIC_IP }]
      : [
          { type: 'CNAME', name: hostname, value: `${slug}.${BASE_DOMAIN}` },
          { type: 'A', name: hostname, value: PUBLIC_IP },
        ],
    ...overrides,
  };
}

function system(overrides = {}) {
  return {
    version: 'v0.5.0',
    base_domain: BASE_DOMAIN,
    disk: { total_bytes: 100 * GB, used_bytes: 41.3 * GB, used_percent: 41.3 },
    memory: { total_bytes: 4 * GB, available_bytes: 1.7 * GB },
    docker_version: '29.8.0',
    disk_limit_percent: 85,
    log_archive: {
      configured: true,
      last_success_at: ago(1 * HOUR + 12 * MINUTE),
      last_error: '',
    },
    caddy: { enabled: true, last_loaded_at: ago(3 * HOUR), last_error: '' },
    ...overrides,
  };
}

function backups(overrides = {}, last = 9 * HOUR) {
  return {
    configured: true,
    running: false,
    last_success_at: ago(last),
    warning: false,
    next_run_at: new Date(Date.now() + 15 * HOUR).toISOString(),
    runs: [0, 1, 2, 3].map((n) => ({
      id: 40 - n,
      trigger: n === 2 ? 'pre_deploy' : 'scheduled',
      status: 'succeeded',
      started_at: ago(last + n * DAY + 40 * 1000),
      finished_at: ago(last + n * DAY),
      error: '',
    })),
    ...overrides,
  };
}

/** Older deployments of an app, newest first, behind its current ones. */
function history(slug, messages, startAgo) {
  return messages.map((message, index) =>
    deployment(slug, message, startAgo + (index + 1) * 2 * DAY),
  );
}

function scenario(apps, extra = {}) {
  const state = {
    apps: new Map(),
    deployments: new Map(),
    domains: new Map(),
    system: system(),
    backups: backups(),
    ...extra,
  };
  for (const entry of apps) {
    const { slug } = entry.app;
    state.apps.set(slug, entry.app);
    state.domains.set(slug, entry.domains ?? []);
    const { current_deployment: current, latest_deployment: latest } =
      entry.app;
    const list = [];
    if (latest && latest !== current) list.push(latest);
    if (current) list.push(current);
    list.push(
      ...history(
        slug,
        ['Update dependencies', 'Tidy up the footer', 'Initial import'],
        4 * DAY,
      ),
    );
    state.deployments.set(slug, list);
  }
  return state;
}

function healthyApps() {
  const blog = deployment('blog', 'Add reading time to post pages', 3 * HOUR);
  const blogStaging = deployment(
    'blog-staging',
    'Try the new image pipeline',
    26 * MINUTE,
  );
  const shop = deployment('shop', 'Fix rounding of tax amounts', 2 * DAY);
  const shopStaging = deployment(
    'shop-staging',
    'Bump Rails to 8.1.2',
    5 * HOUR,
  );
  return [
    {
      app: app('blog', {
        environment: 'production',
        host_port: 20000,
        primary_url: 'https://www.example.org',
        current_deployment: blog,
        latest_deployment: blog,
      }),
      domains: [
        domain(1, 'www.example.org', 'blog', { is_primary: true }),
        domain(2, 'example.org', 'blog'),
      ],
    },
    {
      app: app('blog-staging', {
        host_port: 20001,
        current_deployment: blogStaging,
        latest_deployment: blogStaging,
      }),
    },
    {
      app: app('shop', {
        environment: 'production',
        host_port: 20002,
        primary_url: 'https://shop.example.org',
        current_deployment: shop,
        latest_deployment: shop,
      }),
      domains: [domain(3, 'shop.example.org', 'shop', { is_primary: true })],
    },
    {
      app: app('shop-staging', {
        host_port: 20003,
        current_deployment: shopStaging,
        latest_deployment: shopStaging,
      }),
    },
  ];
}

function warningApps() {
  const [blog, blogStaging, shop, shopStaging] = healthyApps();

  // A custom domain that is still waiting for its DNS record.
  blog.app.pending_domains = 1;
  blog.domains.push(
    domain(4, 'blog.example.org', 'blog', {
      status: 'pending',
      last_error: 'lookup blog.example.org: no such host',
    }),
  );

  // A deployment that started a moment ago; the previous release serves.
  blogStaging.app.latest_deployment = deployment(
    'blog-staging',
    'Cache rendered pages for anonymous visitors',
    0,
    {
      status: 'running',
      queued_at: ago(48 * 1000),
      started_at: ago(45 * 1000),
      finished_at: null,
    },
  );

  // A deployment that failed; the previous release keeps serving.
  shopStaging.app.latest_deployment = deployment(
    'shop-staging',
    'Move checkout to the new payment API',
    12 * MINUTE,
    { status: 'failed', error: 'health check failed: GET /up returned 500' },
  );

  const wiki = deployment('wiki', 'Archive the 2025 pages', 21 * DAY);
  return [
    blog,
    blogStaging,
    shop,
    shopStaging,
    {
      app: app('wiki', {
        host_port: 20004,
        desired_state: 'stopped',
        current_deployment: wiki,
        latest_deployment: wiki,
      }),
    },
  ];
}

/** Builds the mutable state of a scenario; unknown names mean `healthy`. */
export function buildScenario(name) {
  switch (name) {
    case 'warnings':
      return scenario(warningApps(), {
        system: system({
          disk: {
            total_bytes: 100 * GB,
            used_bytes: 88.4 * GB,
            used_percent: 88.4,
          },
          memory: { total_bytes: 4 * GB, available_bytes: 0.6 * GB },
        }),
        backups: backups(
          {
            warning: true,
            runs: [
              {
                id: 41,
                trigger: 'scheduled',
                status: 'failed',
                started_at: ago(9 * HOUR + 30 * 1000),
                finished_at: ago(9 * HOUR),
                error: 'restic backup: no space left on device',
              },
              ...backups({}, STALE_BACKUP).runs,
            ],
          },
          STALE_BACKUP,
        ),
      });
    case 'empty':
      return scenario([]);
    case 'undeployed': {
      const state = scenario([]);
      const fresh = app('notes', { host_port: 20000, created_at: ago(MINUTE) });
      state.apps.set(fresh.slug, fresh);
      state.deployments.set(fresh.slug, []);
      state.domains.set(fresh.slug, []);
      return state;
    }
    default:
      return scenario(healthyApps());
  }
}

/** Starts a deployment that the mock finishes by itself a little later. */
export function startDeployment(state, slug, { ref, sha: wanted }) {
  const target = state.apps.get(slug);
  const started = deployment(slug, 'Deploy from the mock console', 0, {
    ref: ref || target.default_branch,
    status: 'running',
    queued_at: ago(1000),
    started_at: ago(0),
    finished_at: null,
  });
  if (wanted) started.commit_sha = wanted;
  started.mockFinishAt = Date.now() + 12 * 1000;
  state.deployments.get(slug).unshift(started);
  target.latest_deployment = started;
  return started;
}

/** Finishes deployments started through the mock once their time is up. */
export function settle(state) {
  for (const [slug, list] of state.deployments) {
    for (const entry of list) {
      if (entry.mockFinishAt && entry.mockFinishAt <= Date.now()) {
        delete entry.mockFinishAt;
        entry.status = 'succeeded';
        entry.finished_at = new Date().toISOString();
        const target = state.apps.get(slug);
        if (target) target.current_deployment = entry;
      }
    }
  }
}

export function services(target) {
  if (target.desired_state === 'stopped' || !target.current_deployment) {
    return [];
  }
  return [
    {
      name: 'web',
      state: 'running',
      status: 'Up 3 hours (healthy)',
      health: 'healthy',
      memory_bytes: 0.31 * GB,
      memory_limit_bytes: 0.5 * GB,
    },
    {
      name: 'db',
      state: 'running',
      status: 'Up 3 hours (healthy)',
      health: 'healthy',
      memory_bytes: 0.42 * GB,
      memory_limit_bytes: 1 * GB,
    },
    {
      name: 'worker',
      state: 'running',
      status: 'Up 3 hours',
      health: '',
      memory_bytes: 0.18 * GB,
      memory_limit_bytes: 0.5 * GB,
    },
  ];
}

export const ENV_VARS = [
  { key: 'DATABASE_URL', value: 'postgres://app:not-a-real-password@db/app' },
  { key: 'RAILS_ENV', value: 'production' },
  { key: 'SECRET_KEY_BASE', value: '0000000000000000000000000000000000000000' },
];

const LOG_MESSAGES = [
  ['web', 'Started GET "/" for 198.51.100.7'],
  ['web', 'Completed 200 OK in 23ms (Views: 14.1ms | ActiveRecord: 3.2ms)'],
  ['worker', 'Performed SessionCleanupJob in 41.8ms'],
  ['db', 'checkpoint complete: wrote 12 buffers (0.1%)'],
  ['web', 'Started GET "/posts/42" for 198.51.100.23'],
  ['web', 'Completed 200 OK in 31ms (Views: 20.4ms | ActiveRecord: 5.9ms)'],
];

/** `count` runtime log lines ending now, oldest first. */
export function logLines(count, service) {
  const lines = [];
  for (let i = count; i > 0; i--) {
    const [name, line] = LOG_MESSAGES[i % LOG_MESSAGES.length];
    if (service && service !== name) continue;
    lines.push({ service: name, time: ago(i * 7000), line });
  }
  return lines;
}

export function logArchives(slug) {
  const hour = 4 * HOUR;
  const latest = Math.floor(Date.now() / hour) * hour;
  return [0, 1, 2, 3, 4, 5].flatMap((n) =>
    ['web', 'db'].map((service) => {
      const at = new Date(latest - n * hour);
      const day = at.toISOString().slice(0, 10).replaceAll('-', '/');
      return {
        key: `logs/${slug}/${service}/${day}/${String(at.getUTCHours()).padStart(2, '0')}.log.gz`,
        service,
        hour: at.toISOString(),
        size: 18000 + n * 2311 + service.length * 977,
      };
    }),
  );
}

export function snapshots() {
  return [0, 1, 2].flatMap((n) => [
    {
      id: sha(900 + n).slice(0, 8),
      time: ago(9 * HOUR + n * DAY),
      kind: 'volume',
      target: 'uploads',
      size_bytes: 0.2 * GB + n * 1024 ** 2,
    },
    {
      id: sha(950 + n).slice(0, 8),
      time: ago(9 * HOUR + n * DAY + 20 * 1000),
      kind: 'dump',
      target: 'db',
      size_bytes: null,
    },
  ]);
}

export const DOCKER_USAGE = {
  items: [
    {
      type: 'images',
      total_bytes: 12.4 * GB,
      reclaimable_bytes: 3.1 * GB,
      count: 18,
    },
    {
      type: 'containers',
      total_bytes: 0.02 * GB,
      reclaimable_bytes: 0,
      count: 11,
    },
    { type: 'volumes', total_bytes: 6.8 * GB, reclaimable_bytes: 0, count: 7 },
    {
      type: 'build_cache',
      total_bytes: 4.2 * GB,
      reclaimable_bytes: 4.2 * GB,
      count: 61,
    },
  ],
};

export const REPOS = [
  { id: 1, full_name: 'octo/blog', private: true, default_branch: 'main' },
  { id: 2, full_name: 'octo/shop', private: true, default_branch: 'main' },
  { id: 3, full_name: 'octo/wiki', private: false, default_branch: 'main' },
];

export function commits() {
  return [
    'Add reading time to post pages',
    'Cache rendered pages for anonymous visitors',
    'Try the new image pipeline',
    'Update dependencies',
    'Tidy up the footer',
  ].map((message, index) => ({
    sha: sha(500 + index),
    message,
    author: 'octo',
    date: ago((index * 9 + 1) * HOUR),
  }));
}

export const COMPOSE = {
  files: ['compose.yaml'],
  path: 'compose.yaml',
  services: [
    { name: 'web', ports: [3000] },
    { name: 'db', ports: [5432] },
    { name: 'worker', ports: [] },
  ],
};

/** A new app as `POST /api/apps` would return it. */
export function newApp(body, hostPort) {
  return app(body.slug, { ...body, host_port: hostPort, created_at: ago(0) });
}

/** A new, unverified custom domain. */
export function newDomain(id, hostname, slug) {
  return domain(id, hostname, slug, {
    status: 'pending',
    last_error: `lookup ${hostname}: no such host`,
  });
}
