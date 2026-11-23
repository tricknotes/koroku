// Fixture data of the mock API (see server.mjs). Every scenario is built
// fresh relative to "now", so relative times always look alive. Nothing
// here is real: names, domains and addresses are placeholders.

import { useDeferredActivity, useWorkflowGatedActivity } from './activity.mjs';

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const GB = 1024 ** 3;
const MB = 1024 ** 2;

export const BASE_DOMAIN = 'dev.example.com';
export const PUBLIC_IP = '203.0.113.10';

/** Age of the last good backup in the `warnings` scenario. */
const STALE_BACKUP = 2 * DAY + 3 * HOUR;

/** What each scenario shows; listed by `GET /__mock`. */
export const SCENARIOS = {
  healthy: 'four apps, everything fine',
  warnings:
    'a deployment in progress, a failed one, a stopped app, a pending domain, disk above the limit, stale backup',
  empty: 'no apps yet, and no container running',
  undeployed: 'one app that was created and never deployed',
  error: 'every API call fails with 500',
  'tab-error': 'apps load, everything a tab asks for fails with 500',
  'tab-loading': 'apps load, everything a tab asks for takes a minute',
  'app-loading': 'the app list loads, opening an app takes a minute',
  loading: 'API calls take a minute to answer',
  login: 'not logged in',
  'host-trouble':
    'warnings, plus a failing log archive and a Caddy that refuses its configuration',
  unconfigured:
    'a bare host: no object storage (no backups, no log archive), no GitHub App, Caddy disabled, no swap',
  fresh:
    'one app deployed a moment ago; no variables, no custom domains, no backup has run yet',
  'backups-down':
    'everything fine, but the backup status, the Docker disk usage and the container breakdown cannot be read',
  'volumes-down': 'everything fine, but the volumes of an app cannot be listed',
  'stale-login':
    'everything fine, but the login is older than 10 minutes: secrets are hidden and dangerous operations ask to log in again',
  'env-pending':
    'everything fine, but blog has saved variables that its running release does not have yet',
  'env-missing':
    'the Compose file of blog asks for variables that are not saved: two required, two optional',
  'env-optional':
    'the Compose file of blog asks for optional variables that are not saved (defaults apply)',
  'env-dotenv':
    'blog compares with .env.example: one key is missing, one saved key is no longer listed',
  'env-unavailable':
    'blog compares with .env.example, which the repository does not have; shop is private and the GitHub App is not installed on it',
  'compose-mismatch':
    "blog's web was killed for memory and restarted, its worker is left from the release before, its db keeps restarting with another OOM score; the merged configuration of its previous release fails",
  rollback:
    'blog and blog-staging were rolled back to a kept release after a bad deploy; older releases are kept or already removed',
  'copy-running':
    'everything fine, and a copy of the production database into blog-staging is running',
  'copy-failed':
    'everything fine, but the last copy into blog-staging failed while loading and left its services stopped',
  'review-apps':
    'blog-staging and shop-staging allow review apps and have some: one running, one whose pull request was merged (sleeping), one deploying, one whose first deployment failed',
  'review-permission':
    'review apps as in review-apps, but the GitHub App lacks Pull requests: Read-only, so no pull request can be listed',
  'review-allowed':
    'everything fine; the owner allowed review apps of blog-staging, which has none yet',
  'review-member':
    'review apps as in review-apps, logged in as the member mia (manage on blog-staging, no role on shop-staging); review apps are not allowed on blog-staging any more',
  'promote-unpaired':
    'everything fine, but blog has no promotion source (shop still promotes from shop-staging)',
  member:
    'logged in as the member mia, not the owner: deploy on blog, manage on blog-staging, view on shop, no role on shop-staging',
  'webhook-unset':
    'everything fine, but blog-staging deploys on push while the webhook secret is not set in config.toml',
  'workflow-gated':
    'everything fine; blog-staging deploys after its image workflow succeeds, and the last run failed',
  'deploy-deferred':
    'everything fine; a push to blog-staging arrived while a command ran and waits for it to end; an earlier one was dropped because the disk was full',
  sleeping:
    'everything fine; shop-staging is asleep after 30 minutes without requests, blog-staging sleeps after an hour',
  'reservations-pending':
    'everything fine; blog saved smaller reservations for db and web after its last deployment, which still reserves the defaults',
  'blue-green':
    'blog deploys blue/green and serves from green; its history has switches, a deploy that failed for want of memory and one whose new colour failed its check; blog-staging has blue/green on but its Compose file breaks the conditions; shop has not been checked yet',
  'blue-green-deploying':
    'blog is in the middle of a blue/green deploy, stopping blue after the switch to green; shop-staging is at the switch',
  'reservations-unrecorded':
    'everything fine; blog saved smaller reservations, but its running release was deployed before koroku recorded the memory shapes, so the next reservations are unknown',
  'reservations-tight':
    'production apps reserve more memory than a 4 GB host can keep with its 1 GB margin; docs was switched to staging but not redeployed',
};

/** The owner of every scenario, and the members it let in (ARCHITECTURE 10.7). */
export const OWNER = { id: 1, login: 'octo' };
export const MEMBER = { id: 2, login: 'mia' };

/** The roles of `MEMBER` in the `member` scenario. */
export const MEMBER_ROLES = {
  blog: 'deploy',
  'blog-staging': 'manage',
  shop: 'view',
};

/** The members the owner sees on the members screen. */
export function members(scenarioName) {
  if (scenarioName === 'empty') return [];
  return [
    {
      github_user_id: MEMBER.id,
      login: MEMBER.login,
      avatar_url: '',
      added_at: ago(12 * DAY),
      roles: Object.entries(MEMBER_ROLES)
        .map(([app, role]) => ({ app, role }))
        .sort((a, b) => a.app.localeCompare(b.app)),
    },
    {
      github_user_id: 3,
      login: 'kenta',
      avatar_url: '',
      added_at: ago(3 * DAY),
      roles: [{ app: 'shop-staging', role: 'view' }],
    },
    {
      github_user_id: 4,
      login: 'newcomer',
      avatar_url: '',
      added_at: ago(10 * MINUTE),
      roles: [],
    },
  ];
}

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
    source_deployment_id: null,
    rollback_available: false,
    actor: OWNER,
    trigger: 'manual',
    pusher: '',
    // A failed one says itself whether it got as far as the start step.
    containers_replaced: overrides.status !== 'failed',
    web_color: null,
    switched_at: null,
    ...overrides,
  };
}

/** How many distinct releases koroku keeps per app (ARCHITECTURE 7.1). */
const KEEP_RELEASES = 3;

function releaseOf(entry) {
  return entry.source_deployment_id ?? entry.id;
}

/**
 * Marks which deployments can be rolled back to, as koroku's cleanup leaves
 * them: the newest distinct releases of successful deployments, plus the
 * one that runs.
 */
export function markKept(state, slug) {
  const list = state.deployments.get(slug) ?? [];
  const kept = new Set();
  for (const entry of list) {
    if (kept.size >= KEEP_RELEASES) break;
    if (entry.status === 'succeeded') kept.add(releaseOf(entry));
  }
  const current = state.apps.get(slug)?.current_deployment;
  if (current) kept.add(current.id);
  for (const entry of list) {
    entry.rollback_available =
      entry.status === 'succeeded' && kept.has(releaseOf(entry));
  }
}

/** A rollback record: the commit of `source`, started again in seconds. */
function rollbackOf(source, finishedAgo, overrides = {}) {
  return deployment(source.app_slug, source.commit_message, finishedAgo, {
    ref: 'rollback',
    commit_sha: source.commit_sha,
    source_deployment_id: source.id,
    queued_at: ago(finishedAgo + 15 * 1000),
    started_at: ago(finishedAgo + 14 * 1000),
    ...overrides,
  });
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
    memory_reservation: '',
    service_memory_reservations: {},
    backup_exclude_volumes: ['pgdata'],
    dump_hook_service: 'db',
    dump_hook_command: 'pg_dump -U postgres app',
    backup_before_deploy: production,
    // Staging apps take copies of the production database (10.5).
    load_hook_service: production ? '' : 'db',
    load_hook_command: production
      ? ''
      : 'pg_restore --clean --if-exists --no-owner -U "$POSTGRES_USER" -d "$POSTGRES_DB"',
    post_load_service: production ? '' : 'web',
    post_load_command: production ? '' : 'bin/rails db:anonymize',
    env_source: 'compose',
    env_example_path: '',
    // Staging apps start protected (ARCHITECTURE 8.3).
    access_protection: !production,
    protection_exempt_paths: [],
    promote_from: '',
    auto_deploy: false,
    auto_deploy_branch: '',
    auto_deploy_workflow: '',
    idle_sleep: false,
    idle_sleep_minutes: 30,
    sleeping_since: null,
    review: null,
    // Only the owner allows review apps of an app (ARCHITECTURE 9.9).
    review_apps_allowed: false,
    // Blue/green deploys of the public service (ARCHITECTURE 7.2).
    blue_green: false,
    blue_green_drain_seconds: 10,
    alt_host_port: null,
    web_color: null,
    // Checked on every deployment; unknown before the first one.
    blue_green_problems: overrides.current_deployment ? [] : null,
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
    memory: { total_bytes: 4 * GB, available_bytes: 1.4 * GB },
    docker_version: '29.8.0',
    swap: { total_bytes: 2 * GB, used_bytes: 0.21 * GB },
    disk_limit_percent: 85,
    log_archive: {
      configured: true,
      last_success_at: ago(1 * HOUR + 12 * MINUTE),
      last_error: '',
      next_run_at: new Date(Date.now() + 2 * HOUR + 48 * MINUTE).toISOString(),
    },
    caddy: { enabled: true, last_loaded_at: ago(3 * HOUR), last_error: '' },
    webhook: {
      url: `https://console.${BASE_DOMAIN}/webhooks/github`,
      secret_configured: true,
    },
    idle_sleep: { available: true },
    ...overrides,
  };
}

const SHOP_DUMP_ERROR =
  'shop dump db: pg_dump: error: connection to server on socket "/var/run/postgresql/.s.PGSQL.5432" failed: No such file or directory';

/**
 * `GET /api/apps/{slug}/backups/status`: the app's part of its latest run
 * (ARCHITECTURE 10.2). By default it was stored when the host's last run
 * succeeded.
 */
export function appBackupStatus(overrides = {}, run = {}, last = 9 * HOUR) {
  return {
    configured: true,
    last_success_at: ago(last),
    warning: false,
    last_run: {
      id: 40,
      trigger: 'scheduled',
      started_at: ago(last + 40 * 1000),
      finished_at: ago(last),
      status: 'succeeded',
      whole_run: false,
      error: '',
      targets: [
        { kind: 'volume', target: 'uploads', status: 'succeeded', error: '' },
        { kind: 'dump', target: 'db', status: 'succeeded', error: '' },
      ],
      ...run,
    },
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
/** Older deployments; the member deployed one of them (ARCHITECTURE 10.7). */
function history(slug, messages, startAgo) {
  return messages.map((message, index) =>
    deployment(
      slug,
      message,
      startAgo + (index + 1) * 2 * DAY,
      index === 0 ? { actor: MEMBER } : {},
    ),
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
    // An app that was never deployed has no history either.
    if (list.length > 0) {
      list.push(
        ...history(
          slug,
          ['Update dependencies', 'Tidy up the footer', 'Initial import'],
          4 * DAY,
        ),
      );
    }
    state.deployments.set(slug, list);
    markKept(state, slug);
  }
  return state;
}

/**
 * An app that went back to an earlier release: a bad deploy, a fix that
 * failed (and was rolled back automatically), then "この版に戻す". Its
 * history has releases that are kept and one that is not any more.
 */
function rolledBack(state, slug) {
  const initial = deployment(slug, 'Initial import', 6 * DAY);
  const deps = deployment(slug, 'Update dependencies', 3 * DAY);
  const footer = deployment(slug, 'Tidy up the footer', 1 * DAY);
  const pipeline = deployment(slug, 'Try the new image pipeline', 50 * MINUTE);
  const fix = deployment(slug, 'Fix the image cache key', 22 * MINUTE, {
    status: 'failed',
    error: 'health check did not pass within 1m0s (last result: HTTP 502)',
    containers_replaced: true,
  });
  const back = rollbackOf(footer, 9 * MINUTE);
  const target = state.apps.get(slug);
  target.current_deployment = footer;
  target.latest_deployment = back;
  state.deployments.set(slug, [back, fix, pipeline, footer, deps, initial]);
  markKept(state, slug);
}

/**
 * blog's deployments over the memory history's two weeks (ARCHITECTURE
 * 9.5): manual, pushed and promoted ones, a failed one that rolled back
 * (marked), a failed build (not marked) and a rollback shortly after a
 * deployment, which share a marker in the longer ranges.
 */
function deploysAcrossTwoWeeks(state, slug) {
  const initial = deployment(slug, 'Initial import', 13 * DAY + 2 * HOUR);
  const deps = deployment(slug, 'Update dependencies', 9 * DAY + 5 * HOUR, {
    trigger: 'push',
    pusher: 'octo',
    actor: null,
  });
  const cache = deployment(
    slug,
    'Cache rendered Markdown',
    6 * DAY + 3 * HOUR,
    {
      actor: MEMBER,
    },
  );
  const build = deployment(
    slug,
    'Try the new image pipeline',
    3 * DAY + 6 * HOUR,
    {
      status: 'failed',
      error: 'build: exit status 17',
    },
  );
  const rails = deployment(slug, 'Bump Rails to 8.1.2', 3 * DAY + 2 * HOUR, {
    status: 'failed',
    error: 'health check did not pass within 1m0s (last result: HTTP 502)',
  });
  rails.containers_replaced = true;
  const fonts = deployment(slug, 'Preload the web fonts', 2 * DAY + 4 * HOUR, {
    ref: 'main',
    promoted_from: { deployment_id: 7, app_slug: 'blog-staging' },
  });
  const sitemap = deployment(slug, 'Generate the sitemap nightly', 20 * HOUR);
  const back = rollbackOf(fonts, 19 * HOUR + 35 * MINUTE);
  const current = deployment(slug, 'Add reading time to post pages', 3 * HOUR);
  const target = state.apps.get(slug);
  target.current_deployment = current;
  target.latest_deployment = current;
  state.deployments.set(slug, [
    current,
    back,
    sitemap,
    fonts,
    rails,
    build,
    cache,
    deps,
    initial,
  ]);
  markKept(state, slug);
  // The memory use steps up with the Markdown cache (mock/memory.mjs).
  state.memorySteps = new Map([
    [slug, [{ at: Date.parse(cache.finished_at), service: 'web', mib: 45 }]],
  ]);
}

/** When a blue/green deployment switched: a few seconds before it ended. */
function switchedAt(finishedAgo) {
  return ago(finishedAgo + 14 * 1000);
}

/**
 * blog deploys blue/green (ARCHITECTURE 7.2): the first deployment after
 * turning it on moved the public service to green, then the colours took
 * turns. Three failed: a worker after the switch (the requests went back),
 * the new colour's check, and the memory for a second copy.
 */
function blueGreenHistory(state, slug) {
  const target = state.apps.get(slug);
  Object.assign(target, {
    blue_green: true,
    alt_host_port: 20010,
    web_color: 'green',
    blue_green_problems: [],
  });
  const initial = deployment(slug, 'Initial import', 6 * DAY);
  const deps = deployment(slug, 'Update dependencies', 4 * DAY, {
    web_color: 'green',
    switched_at: switchedAt(4 * DAY),
  });
  const footer = deployment(slug, 'Tidy up the footer', 2 * DAY, {
    web_color: 'blue',
    switched_at: switchedAt(2 * DAY),
  });
  const job = deployment(slug, 'Move reading time to a job', 36 * HOUR, {
    status: 'failed',
    error:
      'recreate the other services: docker compose up: exit status 1; the previous release was restored',
    web_color: 'green',
    containers_replaced: true,
    mockFailure: 'services',
  });
  const rename = deployment(slug, 'Rename the reading column', 1 * DAY, {
    status: 'failed',
    error: 'health check did not pass within 1m0s (last result: HTTP 500)',
    web_color: 'green',
    containers_replaced: true,
    mockFailure: 'check',
  });
  const cache = deployment(slug, 'Cache rendered Markdown', 5 * HOUR, {
    status: 'failed',
    error:
      'Not enough memory to run two web containers (312 MiB available, 512 MiB needed).',
    containers_replaced: false,
    mockFailure: 'memory',
    mockColor: 'green',
  });
  const current = deployment(slug, 'Add reading time to post pages', 3 * HOUR, {
    web_color: 'green',
    switched_at: switchedAt(3 * HOUR),
  });
  target.current_deployment = current;
  target.latest_deployment = current;
  state.deployments.set(slug, [
    current,
    cache,
    rename,
    job,
    footer,
    deps,
    initial,
  ]);
  markKept(state, slug);
}

/** blog-staging turned blue/green on, but its Compose file breaks the rules. */
function blueGreenProblems(state, slug) {
  Object.assign(state.apps.get(slug), {
    blue_green: true,
    alt_host_port: 20011,
    blue_green_problems: [
      {
        service: 'web',
        rule: 'blue_green',
        message:
          'container_name blog-web would be used by both colours at once; remove it',
        relaxable: false,
      },
      {
        service: 'worker',
        rule: 'blue_green',
        message:
          'refers to the public service web (depends_on), which blue/green deploys disable',
        relaxable: false,
      },
    ],
  });
}

/**
 * A blue/green deployment that stays in `step` (it never ends): both
 * colours run, the new one `color`.
 */
function blueGreenInProgress(state, slug, step, color, oldColor) {
  const target = state.apps.get(slug);
  const previous = target.current_deployment;
  previous.web_color = oldColor;
  previous.switched_at = switchedAt(
    Date.now() - Date.parse(previous.finished_at),
  );
  const switched = step !== 'switch';
  const running = deployment(slug, 'Show related posts', 0, {
    status: 'running',
    queued_at: ago(50 * 1000),
    started_at: ago(48 * 1000),
    finished_at: null,
    containers_replaced: null,
    web_color: color,
    switched_at: switched ? ago(6 * 1000) : null,
    mockStopAt: step,
  });
  Object.assign(target, {
    blue_green: true,
    alt_host_port: target.alt_host_port ?? 20010 + target.host_port - 20000,
    web_color: switched ? color : oldColor,
    blue_green_problems: [],
    latest_deployment: running,
    // Both copies of the public service run (services()).
    mockColors: [oldColor, color],
  });
  state.deployments.get(slug).unshift(running);
}

function healthyApps() {
  const blog = deployment('blog', 'Add reading time to post pages', 3 * HOUR);
  // blog-staging deploys every push to main (ARCHITECTURE 5.2).
  const blogStaging = deployment(
    'blog-staging',
    'Try the new image pipeline',
    26 * MINUTE,
    { trigger: 'push', pusher: 'octo' },
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
        // The worker can wait for memory (ARCHITECTURE 6.2).
        service_memory_reservations: { worker: '0' },
        host_port: 20000,
        primary_url: 'https://www.example.org',
        promote_from: 'blog-staging',
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
        auto_deploy: true,
        current_deployment: blogStaging,
        latest_deployment: blogStaging,
      }),
    },
    {
      app: app('shop', {
        environment: 'production',
        host_port: 20002,
        primary_url: 'https://shop.example.org',
        promote_from: 'shop-staging',
        current_deployment: shop,
        latest_deployment: shop,
      }),
      domains: [domain(3, 'shop.example.org', 'shop', { is_primary: true })],
    },
    {
      app: app('shop-staging', {
        host_port: 20003,
        protection_exempt_paths: ['/webhooks/', '/api/payments/callback'],
        current_deployment: shopStaging,
        latest_deployment: shopStaging,
      }),
    },
  ];
}

/** The review part of an app's JSON (ARCHITECTURE 9.9). */
function review(parent, number, title, branch, state, copy) {
  return {
    parent,
    pr_number: number,
    pr_title: title,
    pr_url: `https://github.com/octo/${parent.replace(/-staging$/, '')}/pull/${number}`,
    pr_state: state,
    head_branch: branch,
    data_copy: copy,
    data_copy_detail: '',
  };
}

/** Review apps of blog-staging and shop-staging. */
function reviewApps() {
  const search = deployment(
    'blog-staging-pr-42',
    'Add full-text search to the archive',
    18 * MINUTE,
    { ref: 'search', trigger: 'push', pusher: 'mona' },
  );
  const dark = deployment(
    'blog-staging-pr-38',
    'Dark mode for the reader',
    5 * DAY,
    { ref: 'dark-mode' },
  );
  const feed = deployment('blog-staging-pr-44', 'Add an RSS feed', 0, {
    ref: 'rss-feed',
    status: 'running',
    queued_at: ago(70 * 1000),
    started_at: ago(64 * 1000),
    finished_at: null,
  });
  const checkout = deployment(
    'shop-staging-pr-7',
    'Split the checkout form into steps',
    4 * MINUTE,
    {
      ref: 'checkout-steps',
      status: 'failed',
      error: 'health check did not pass within 1m0s (last result: HTTP 500)',
    },
  );
  const reviewApp = (slug, repo, branch, overrides) =>
    app(slug, {
      repo_full_name: repo,
      default_branch: branch,
      auto_deploy: true,
      auto_deploy_branch: branch,
      idle_sleep: true,
      created_at: ago(2 * DAY),
      ...overrides,
    });
  return [
    {
      app: reviewApp('blog-staging-pr-42', 'octo/blog', 'search', {
        host_port: 20010,
        current_deployment: search,
        latest_deployment: search,
        review: review(
          'blog-staging',
          42,
          'Add full-text search to the archive',
          'search',
          'open',
          'started',
        ),
      }),
    },
    {
      app: reviewApp('blog-staging-pr-38', 'octo/blog', 'dark-mode', {
        host_port: 20011,
        sleeping_since: ago(20 * HOUR),
        current_deployment: dark,
        latest_deployment: dark,
        created_at: ago(6 * DAY),
        review: review(
          'blog-staging',
          38,
          'Dark mode for the reader',
          'dark-mode',
          'merged',
          'started',
        ),
      }),
    },
    {
      app: reviewApp('blog-staging-pr-44', 'octo/blog', 'rss-feed', {
        host_port: 20013,
        latest_deployment: feed,
        created_at: ago(2 * MINUTE),
        review: review(
          'blog-staging',
          44,
          'Add an RSS feed',
          'rss-feed',
          'open',
          'waiting',
        ),
      }),
    },
    {
      app: reviewApp('shop-staging-pr-7', 'octo/shop', 'checkout-steps', {
        host_port: 20012,
        protection_exempt_paths: ['/webhooks/', '/api/payments/callback'],
        latest_deployment: checkout,
        created_at: ago(5 * MINUTE),
        review: review(
          'shop-staging',
          7,
          'Split the checkout form into steps',
          'checkout-steps',
          'open',
          'waiting',
        ),
      }),
    },
  ];
}

/** The open pull requests of each repository (`GET .../pull-requests`). */
export const PULL_REQUESTS = {
  'octo/blog': [
    {
      number: 46,
      title: 'Paginate the archive by month',
      head_branch: 'archive-months',
      author: 'mona',
      updatedAgo: 25 * MINUTE,
    },
    {
      number: 45,
      title: 'Fix typo in the about page',
      head_branch: 'patch-1',
      author: 'drive-by',
      updatedAgo: 3 * HOUR,
      from_fork: true,
    },
    {
      number: 44,
      title: 'Add an RSS feed',
      head_branch: 'rss-feed',
      author: 'octo',
      updatedAgo: 4 * MINUTE,
    },
    {
      number: 42,
      title: 'Add full-text search to the archive',
      head_branch: 'search',
      author: 'mona',
      updatedAgo: 20 * MINUTE,
    },
    {
      number: 40,
      title: 'Move images to the CDN',
      head_branch: 'cdn-images',
      author: 'octo',
      updatedAgo: 2 * DAY,
    },
  ],
  'octo/shop': [
    {
      number: 9,
      title: 'Show stock levels on product pages',
      head_branch: 'stock-levels',
      author: 'octo',
      updatedAgo: 1 * HOUR,
    },
    {
      number: 7,
      title: 'Split the checkout form into steps',
      head_branch: 'checkout-steps',
      author: 'mona',
      updatedAgo: 6 * MINUTE,
    },
  ],
};

/** The pull requests of a parent, as the API lists them. */
export function pullRequestsOf(state, parent) {
  return (PULL_REQUESTS[parent.repo_full_name] ?? []).map(
    ({ updatedAgo, from_fork = false, ...pr }) => {
      const existing = [...state.apps.values()].find(
        (a) =>
          a.review?.parent === parent.slug && a.review.pr_number === pr.number,
      );
      return {
        ...pr,
        head_sha: `${pr.number}`.padEnd(40, 'a'),
        url: `https://github.com/${parent.repo_full_name}/pull/${pr.number}`,
        updated_at: ago(updatedAgo),
        from_fork,
        review_app: existing?.slug ?? null,
      };
    },
  );
}

/** A review app as `POST .../review-apps` would create it. */
export function newReviewApp(state, parent, pr, copyData, hostPort) {
  let slug = `${parent.slug}-pr-${pr.number}`;
  for (let n = 2; state.apps.has(slug); n++) {
    slug = `${parent.slug}-pr-${pr.number}-${n}`;
  }
  const hooks = parent.dump_hook_command && parent.load_hook_command;
  const created = app(slug, {
    ...Object.fromEntries(
      [
        'repo_full_name',
        'compose_path',
        'web_service',
        'web_port',
        'healthcheck_path',
        'memory_limit',
        'service_memory_limits',
        'backup_exclude_volumes',
        'dump_hook_service',
        'dump_hook_command',
        'load_hook_service',
        'load_hook_command',
        'post_load_service',
        'post_load_command',
        'protection_exempt_paths',
        'auto_deploy_workflow',
      ].map((key) => [key, parent[key]]),
    ),
    host_port: hostPort,
    default_branch: pr.head_branch,
    auto_deploy: true,
    auto_deploy_branch: pr.head_branch,
    idle_sleep: true,
    created_at: ago(0),
    review: {
      ...review(parent.slug, pr.number, pr.title, pr.head_branch, 'open', ''),
      pr_url: pr.url,
      data_copy: copyData ? (hooks ? 'waiting' : 'skipped') : '',
      data_copy_detail:
        copyData && !hooks
          ? parent.dump_hook_command
            ? 'no_load_hook'
            : 'no_dump_hook'
          : '',
    },
  });
  return created;
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
    {
      status: 'failed',
      error: 'health check did not pass within 1m0s (last result: HTTP 500)',
    },
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
        services: new Map([['shop', troubledServices()]]),
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
                error: SHOP_DUMP_ERROR,
              },
              ...backups({}, STALE_BACKUP).runs,
            ],
            // Only shop's dump failed; the other apps were stored.
            problems: [{ app: 'shop', status: 'failed', whole_run: false }],
            system_failed: false,
          },
          STALE_BACKUP,
        ),
        appBackupStatus: new Map([
          [
            'shop',
            appBackupStatus(
              { warning: true, last_success_at: ago(STALE_BACKUP) },
              {
                id: 41,
                status: 'failed',
                started_at: ago(9 * HOUR + 30 * 1000),
                finished_at: ago(9 * HOUR),
                targets: [
                  {
                    kind: 'volume',
                    target: 'uploads',
                    status: 'succeeded',
                    error: '',
                  },
                  {
                    kind: 'dump',
                    target: 'db',
                    status: 'failed',
                    error:
                      'pg_dump: error: connection to server on socket "/var/run/postgresql/.s.PGSQL.5432" failed: No such file or directory',
                  },
                ],
              },
            ),
          ],
        ]),
      });
    case 'empty':
      return scenario([], { noContainers: true });
    case 'host-trouble': {
      const state = buildScenario('warnings');
      state.system.swap = { total_bytes: 2 * GB, used_bytes: 1.6 * GB };
      state.system.log_archive = {
        configured: true,
        last_success_at: ago(9 * HOUR),
        last_error:
          'put logs/blog/web/2026/11/18/04.log.gz: 403 Forbidden: access denied',
        next_run_at: new Date(Date.now() + 40 * MINUTE).toISOString(),
      };
      state.system.caddy = {
        enabled: true,
        last_loaded_at: ago(2 * DAY),
        last_error:
          'load caddy config: dial tcp 127.0.0.1:2019: connect: connection refused',
      };
      return state;
    }
    case 'reservations-pending': {
      const state = scenario(healthyApps());
      const blog = state.apps.get('blog');
      snapshotReservations(blog);
      blog.service_memory_reservations = {
        worker: '0',
        db: '384m',
        web: '192m',
      };
      return state;
    }
    case 'reservations-unrecorded': {
      const state = scenario(healthyApps());
      const blog = state.apps.get('blog');
      snapshotReservations(blog);
      blog.service_memory_reservations = { db: '384m', web: '192m' };
      blog.shapes_unrecorded = true;
      return state;
    }
    case 'reservations-tight': {
      // Raised reservations on both production apps, and docs still runs
      // the production release it had before it became staging.
      const state = scenario([
        ...healthyApps(),
        {
          app: app('docs', {
            host_port: 20004,
            current_deployment: deployment(
              'docs',
              'Add the search page',
              6 * DAY,
            ),
          }),
        },
      ]);
      state.apps.get('blog').memory_reservation = '768m';
      state.apps.get('blog').service_memory_reservations = {};
      state.apps.get('docs').reserved = { web: 256 * MB, db: 512 * MB };
      return state;
    }
    case 'webhook-unset': {
      const state = scenario(healthyApps());
      state.system.webhook.secret_configured = false;
      return state;
    }
    case 'review-apps':
    case 'review-permission':
    case 'review-member': {
      const state = scenario([...healthyApps(), ...reviewApps()]);
      state.pullRequestsDenied = name === 'review-permission';
      // The parents that have review apps were allowed them.
      state.apps.get('blog-staging').review_apps_allowed =
        name !== 'review-member';
      state.apps.get('shop-staging').review_apps_allowed = true;
      return state;
    }
    case 'review-allowed': {
      const state = scenario(healthyApps());
      state.apps.get('blog-staging').review_apps_allowed = true;
      return state;
    }
    case 'sleeping': {
      // shop-staging went to sleep while idle; blog-staging sleeps after an
      // hour without requests but is awake (ARCHITECTURE 8.4).
      const state = scenario(healthyApps());
      Object.assign(state.apps.get('shop-staging'), {
        idle_sleep: true,
        idle_sleep_minutes: 30,
        sleeping_since: ago(2 * HOUR + 14 * MINUTE),
      });
      Object.assign(state.apps.get('blog-staging'), {
        idle_sleep: true,
        idle_sleep_minutes: 60,
      });
      return state;
    }
    case 'deploy-deferred': {
      const state = scenario(healthyApps());
      state.pendingAutoDeploys = new Map([
        [
          'blog-staging',
          {
            ref: 'main',
            commit_sha: '5eed5eed1234567890abcdef1234567890abcdef',
            commit_message:
              'Show the order history on the account page\n\nAlso paginate it.',
            pusher: 'octo',
            workflow: '',
            received_at: ago(4 * MINUTE),
          },
        ],
      ]);
      useDeferredActivity(state);
      return state;
    }
    case 'workflow-gated': {
      const state = scenario(healthyApps());
      state.apps.get('blog-staging').auto_deploy_workflow =
        '.github/workflows/image.yml';
      useWorkflowGatedActivity(state);
      return state;
    }
    case 'backups-down':
      return scenario(healthyApps(), { backupsDown: true });
    case 'volumes-down':
      return scenario(healthyApps(), { volumesDown: true });
    case 'unconfigured':
      return scenario(healthyApps(), {
        system: system({
          swap: null,
          log_archive: {
            configured: false,
            last_success_at: null,
            last_error: '',
            next_run_at: null,
          },
          caddy: { enabled: false, last_loaded_at: null, last_error: '' },
          webhook: {
            url: `https://console.${BASE_DOMAIN}/webhooks/github`,
            secret_configured: false,
          },
          idle_sleep: { available: false },
        }),
        backups: {
          configured: false,
          running: false,
          last_success_at: null,
          warning: false,
          next_run_at: null,
          runs: [],
        },
        github: false,
      });
    case 'fresh': {
      const first = deployment('notes', 'Initial import', 4 * MINUTE);
      const state = scenario(
        [
          {
            app: app('notes', {
              host_port: 20000,
              repo_full_name: 'octo/notes',
              service_memory_limits: {},
              backup_exclude_volumes: [],
              dump_hook_service: '',
              dump_hook_command: '',
              current_deployment: first,
              latest_deployment: first,
              created_at: ago(6 * MINUTE),
              updated_at: ago(4 * MINUTE),
            }),
          },
        ],
        {
          backups: backups({ last_success_at: null, warning: true, runs: [] }),
          snapshots: new Map([['notes', []]]),
          volumes: new Map([['notes', ['uploads']]]),
          // Nothing was configured for it yet either.
          env: new Map([['notes', []]]),
        },
      );
      state.deployments.set('notes', [first]);
      return state;
    }
    case 'undeployed': {
      const state = scenario([]);
      const fresh = app('notes', { host_port: 20000, created_at: ago(MINUTE) });
      state.apps.set(fresh.slug, fresh);
      state.deployments.set(fresh.slug, []);
      state.domains.set(fresh.slug, []);
      state.snapshots = new Map([[fresh.slug, []]]);
      return state;
    }
    case 'env-missing':
      return scenario(healthyApps(), {
        requirements: new Map([
          ['blog', { compose: composeRequirements(MISSING_EXTRA) }],
        ]),
      });
    case 'env-optional':
      return scenario(healthyApps(), {
        requirements: new Map([
          [
            'blog',
            {
              compose: composeRequirements(
                MISSING_EXTRA.filter((v) => !v.required),
              ),
            },
          ],
        ]),
      });
    case 'env-dotenv': {
      const state = scenario(healthyApps(), {
        requirements: new Map([
          [
            'blog',
            {
              dotenv: {
                status: 'ok',
                path: '.env.example',
                variables: DOTENV_VARIABLES,
              },
            },
          ],
        ]),
      });
      state.apps.get('blog').env_source = 'dotenv';
      return state;
    }
    case 'env-unavailable': {
      const state = scenario(healthyApps(), {
        requirements: new Map([
          [
            'shop',
            { compose: { status: 'unavailable', reason: 'repo_unavailable' } },
          ],
        ]),
      });
      state.apps.get('blog').env_source = 'dotenv';
      return state;
    }
    case 'promote-unpaired': {
      const state = scenario(healthyApps());
      state.apps.get('blog').promote_from = '';
      return state;
    }
    case 'env-pending': {
      // Saved: one value changed and one variable added since the release
      // was deployed; the release still has one that was deleted since.
      const saved = ENV_VARS.map((v) =>
        v.key === 'SECRET_KEY_BASE'
          ? { ...v, value: '1111111111111111111111111111111111111111' }
          : v,
      );
      saved.push({ key: 'MAIL_FROM', value: 'noreply@example.org' });
      return scenario(healthyApps(), {
        env: new Map([['blog', saved]]),
        deployedEnv: new Map([
          ['blog', [{ key: 'LEGACY_FLAG', value: '1' }, ...ENV_VARS]],
        ]),
      });
    }
    case 'memory-deploys': {
      const state = scenario(healthyApps());
      deploysAcrossTwoWeeks(state, 'blog');
      return state;
    }
    case 'blue-green': {
      const state = scenario(healthyApps());
      blueGreenHistory(state, 'blog');
      blueGreenProblems(state, 'blog-staging');
      // Deployed before koroku recorded the check.
      state.apps.get('shop').blue_green_problems = null;
      return state;
    }
    case 'blue-green-deploying': {
      const state = scenario(healthyApps());
      blueGreenInProgress(state, 'blog', 'drain', 'green', 'blue');
      blueGreenInProgress(state, 'shop-staging', 'switch', 'blue', 'green');
      return state;
    }
    case 'rollback': {
      const state = scenario(healthyApps());
      rolledBack(state, 'blog');
      rolledBack(state, 'blog-staging');
      return state;
    }
    default:
      return scenario(healthyApps());
  }
}

/**
 * Starts the kept release of deployment `id` again, as koroku does for
 * `rollback_to`; `null` when that is not possible.
 */
export function startRollback(state, slug, id) {
  const list = state.deployments.get(slug);
  const from = list.find((entry) => entry.id === id);
  if (!from) return { error: 'validation_failed' };
  if (!from.rollback_available) return { error: 'release_unavailable' };
  const source = list.find((entry) => entry.id === releaseOf(from));
  const started = rollbackOf(source, 0, {
    status: 'running',
    queued_at: ago(1000),
    started_at: ago(0),
    finished_at: null,
  });
  started.mockFinishAt = Date.now() + 8 * 1000;
  list.unshift(started);
  state.apps.get(slug).latest_deployment = started;
  return { deployment: started };
}

/**
 * How a promotion to each production app turns out (ARCHITECTURE 9.7):
 * blog reuses every image of blog-staging; shop bakes ASSET_HOST into its
 * assets, so web is built again with production's value.
 */
const PROMOTIONS = {
  blog: {
    commits: [
      'Try the new image pipeline',
      'Cache rendered pages for anonymous visitors',
    ],
    env: { only_in_source: [], only_in_target: ['SENTRY_DSN'] },
    images: [
      { service: 'web', reuse: true, reason: '', differing_args: [] },
      { service: 'worker', reuse: true, reason: '', differing_args: [] },
    ],
  },
  shop: {
    // Production is too far behind to count within the limit.
    commits: null,
    env: {
      only_in_source: ['DEBUG_TOOLBAR'],
      only_in_target: ['SENTRY_DSN', 'STRIPE_LIVE_KEY'],
    },
    images: [
      {
        service: 'web',
        reuse: false,
        reason: 'args_differ',
        differing_args: ['ASSET_HOST'],
      },
      { service: 'worker', reuse: true, reason: '', differing_args: [] },
    ],
  },
};

/** `GET /api/apps/{slug}/promotion` for one app. */
export function promotionPreview(state, target) {
  const empty = {
    status: 'ready',
    reason: '',
    source_app: target.promote_from ?? '',
    source_deployment: null,
    current_deployment: null,
    commits: { status: 'unknown', list: [] },
    env: { only_in_source: [], only_in_target: [] },
    images: { status: 'unavailable', services: [] },
  };
  if (!target.promote_from) return { ...empty, status: 'not_paired' };
  const source = state.apps.get(target.promote_from);
  if (target.environment !== 'production') {
    return { ...empty, status: 'unavailable', reason: 'not_production' };
  }
  if (!source) return { ...empty, status: 'not_paired', source_app: '' };
  if (!source.current_deployment) {
    return { ...empty, status: 'unavailable', reason: 'no_source_release' };
  }
  const plan = PROMOTIONS[target.slug] ?? PROMOTIONS.blog;
  const same =
    target.current_deployment?.commit_sha ===
    source.current_deployment.commit_sha;
  const list = same
    ? []
    : (plan.commits ?? []).map((message, index) => ({
        sha:
          index === 0 ? source.current_deployment.commit_sha : sha(700 + index),
        message,
        author: 'octo',
        date: ago((index * 7 + 1) * HOUR),
      }));
  return {
    ...empty,
    source_deployment: source.current_deployment,
    current_deployment: target.current_deployment,
    commits: { status: same || plan.commits ? 'ok' : 'unknown', list },
    env: plan.env,
    images: { status: 'checked', services: plan.images },
  };
}

/** Starts a deployment that the mock finishes by itself a little later. */
export function startDeployment(
  state,
  slug,
  { ref, sha: wanted, promote_from_deployment: promotedId },
) {
  const target = state.apps.get(slug);
  if (promotedId !== undefined) {
    const source = state.apps.get(target.promote_from)?.current_deployment;
    if (!source || source.id !== promotedId) return null;
    const plan = PROMOTIONS[slug] ?? PROMOTIONS.blog;
    const started = deployment(slug, source.commit_message, 0, {
      ref: '',
      commit_sha: source.commit_sha,
      status: 'running',
      queued_at: ago(1000),
      started_at: ago(0),
      finished_at: null,
      promoted_from: {
        deployment_id: source.id,
        app_slug: target.promote_from,
      },
    });
    started.mockImages = plan.images;
    started.mockFinishAt = Date.now() + 12 * 1000;
    state.deployments.get(slug).unshift(started);
    target.latest_deployment = started;
    return started;
  }
  const started = deployment(slug, 'Deploy from the mock console', 0, {
    ref: ref || target.default_branch,
    status: 'running',
    queued_at: ago(1000),
    started_at: ago(0),
    finished_at: null,
  });
  if (target.blue_green) {
    // The other colour starts next to the one serving (ARCHITECTURE 7.2).
    started.web_color = target.web_color === 'green' ? 'blue' : 'green';
  }
  if (wanted) started.commit_sha = wanted;
  started.mockFinishAt = Date.now() + 12 * 1000;
  state.deployments.get(slug).unshift(started);
  target.latest_deployment = started;
  return started;
}

/**
 * Keeps what the running release reserves before settings that change it
 * are saved: the release keeps it until the next deployment (ARCHITECTURE
 * 6.2).
 */
export function snapshotReservations(target) {
  if (target.reserved) return;
  const reserved = {};
  for (const svc of services(target)) {
    reserved[svc.name] = svc.memory_reservation_bytes ?? 0;
  }
  target.reserved = reserved;
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
        if (target && entry.web_color) {
          entry.switched_at = ago(3 * 1000);
          target.web_color = entry.web_color;
          delete target.mockColors;
        }
        // A rollback runs the release it started again.
        const release = list.find((older) => older.id === releaseOf(entry));
        if (target) target.current_deployment = release ?? entry;
        // A new release reserves what the settings say.
        if (target && entry.source_deployment_id === null)
          delete target.reserved;
        if (entry.source_deployment_id === null) {
          // The release now runs with what is saved.
          state.deployedEnv ??= new Map();
          state.deployedEnv.set(slug, [...(state.env?.get(slug) ?? ENV_VARS)]);
        }
        markKept(state, slug);
      }
    }
  }
}

function parseSize(value) {
  const match = /^([0-9]+)([kmg]?)b?$/.exec(value);
  if (!match) return 0;
  const unit = { '': 1, k: 1024, m: MB, g: 1024 * MB }[match[2]];
  return Number(match[1]) * unit;
}

/**
 * What a production release reserves for a service, like the override
 * (ARCHITECTURE 6.2): the app setting, else half the limit; null for none.
 */
function reservationOf(target, name, running) {
  if (target.environment !== 'production') return null;
  // A limit in the settings applies from the same deployment.
  const limit =
    parseSize(
      target.service_memory_limits?.[name] || target.memory_limit || '',
    ) || running;
  const value =
    target.service_memory_reservations?.[name] || target.memory_reservation;
  if (value === '0') return null;
  const bytes = value
    ? Math.min(parseSize(value), limit)
    : Math.floor(limit / 2 / MB) * MB;
  return bytes >= 6 * MB ? bytes : null;
}

/**
 * `memory_reservation` of `GET /api/system`: what the running releases of
 * the apps that are not stopped reserve, against the host's memory.
 */
export function memoryReservation(state) {
  const apps = [];
  for (const target of state.apps.values()) {
    if (target.desired_state === 'stopped' || !target.current_deployment) {
      continue;
    }
    const reserved = {};
    const next = {};
    for (const svc of services(target)) {
      if (svc.memory_reservation_bytes) {
        reserved[svc.name] = svc.memory_reservation_bytes;
      }
      if (svc.next_memory_reservation_bytes) {
        next[svc.name] = svc.next_memory_reservation_bytes;
      }
    }
    const sum = (map) => Object.values(map).reduce((a, b) => a + b, 0);
    const total = sum(reserved);
    // A release from before the memory shapes were recorded: the next
    // reservations are unknown.
    const unknown = Boolean(target.shapes_unrecorded);
    if (total > 0 || (!unknown && sum(next) > 0)) {
      apps.push({
        slug: target.slug,
        environment: target.environment,
        total_bytes: total,
        services: reserved,
        next_total_bytes: unknown ? null : sum(next),
        next_services: unknown ? null : next,
      });
    }
  }
  apps.sort(
    (a, b) => b.total_bytes - a.total_bytes || a.slug.localeCompare(b.slug),
  );
  const total = apps.reduce((sum, a) => sum + a.total_bytes, 0);
  const margin = 1024 * MB;
  const host = state.system.memory?.total_bytes ?? 0;
  return {
    total_bytes: total,
    next_total_bytes: apps.reduce(
      (sum, a) => sum + (a.next_total_bytes ?? a.total_bytes),
      0,
    ),
    margin_bytes: margin,
    warning: host > 0 && total + margin > host,
    apps,
  };
}

export function services(target) {
  if (target.desired_state === 'stopped' || !target.current_deployment) {
    return [];
  }
  if (target.sleeping_since) {
    // Asleep: the containers are kept, stopped (ARCHITECTURE 8.4).
    return ['web', 'db', 'worker'].map((name) => ({
      name,
      state: 'exited',
      status: 'Exited (0) 2 hours ago',
      health: '',
      memory_bytes: null,
      memory_limit_bytes: null,
      cpu_percent: null,
    }));
  }
  // Staging sees little traffic and holds little data.
  const scale = target.environment === 'production' ? 1 : 0.35;
  // `reserved` is what the running release reserves once the settings
  // changed after it (snapshotReservations); the next deployment follows
  // the settings.
  const reserved = (list) =>
    list.map((svc) => {
      const next = reservationOf(target, svc.name, svc.memory_limit_bytes);
      return {
        ...svc,
        memory_reservation_bytes: target.reserved?.[svc.name] ?? next,
        next_memory_reservation_bytes: next ?? 0,
      };
    });
  // A blue/green app's public service runs as a colour; during a
  // deployment both colours run (ARCHITECTURE 7.2).
  const colors =
    target.mockColors ?? (target.web_color ? [target.web_color] : [null]);
  const webRows = colors.map((color, index) => ({
    name: 'web',
    state: 'running',
    status: index === 0 ? 'Up 3 hours (healthy)' : 'Up 40 seconds (healthy)',
    health: 'healthy',
    memory_bytes: (index === 0 ? 0.31 : 0.22) * GB * scale,
    memory_limit_bytes: 0.5 * GB,
    cpu_percent: index === 0 ? 4.2 : 11.8,
    ...(color ? { color } : {}),
  }));
  return reserved([
    ...webRows,
    {
      name: 'db',
      state: 'running',
      status: 'Up 3 hours (healthy)',
      health: 'healthy',
      memory_bytes: 0.42 * GB * scale,
      memory_limit_bytes: 1 * GB,
      cpu_percent: 0.8,
    },
    {
      name: 'worker',
      state: 'running',
      status: 'Up 3 hours',
      health: '',
      memory_bytes: 0.18 * GB * scale,
      memory_limit_bytes: 0.5 * GB,
      cpu_percent: 1.6,
    },
  ]);
}

/**
 * Services with something wrong: a failing health check close to the memory
 * limit, a worker that crashed and a one-shot job that ended normally.
 */
function troubledServices() {
  return [
    {
      name: 'web',
      state: 'running',
      status: 'Up 2 days (unhealthy)',
      health: 'unhealthy',
      memory_bytes: 0.47 * GB,
      memory_limit_bytes: 0.5 * GB,
      cpu_percent: 87.5,
    },
    {
      name: 'db',
      state: 'running',
      status: 'Up 2 days (healthy)',
      health: 'healthy',
      memory_bytes: 0.42 * GB,
      memory_limit_bytes: 1 * GB,
      cpu_percent: 0.9,
    },
    {
      name: 'worker',
      state: 'exited',
      status: 'Exited (137) 4 minutes ago',
      health: '',
      memory_bytes: null,
      memory_limit_bytes: null,
      cpu_percent: null,
    },
    {
      name: 'migrate',
      state: 'exited',
      status: 'Exited (0) 2 days ago',
      health: '',
      memory_bytes: null,
      memory_limit_bytes: null,
      cpu_percent: null,
    },
  ];
}

export const ENV_VARS = [
  { key: 'DATABASE_URL', value: 'postgres://app:not-a-real-password@db/app' },
  { key: 'RAILS_ENV', value: 'production' },
  { key: 'SECRET_KEY_BASE', value: '0000000000000000000000000000000000000000' },
];

/**
 * What a Compose file asks for (GET /api/apps/{slug}/env/requirements):
 * the saved ENV_VARS, plus `extra`.
 */
export function composeRequirements(extra = []) {
  return {
    status: 'ok',
    path: 'compose.yaml',
    variables: [
      {
        key: 'DATABASE_URL',
        required: true,
        default: null,
        description: '',
      },
      {
        key: 'RAILS_ENV',
        required: false,
        default: 'production',
        description: '',
      },
      {
        key: 'SECRET_KEY_BASE',
        required: true,
        default: null,
        description: 'openssl rand -hex 64 で作る',
      },
      ...extra,
    ],
  };
}

/** Variables a newer commit of blog started to use. */
export const MISSING_EXTRA = [
  {
    key: 'SMTP_HOST',
    required: true,
    default: null,
    description: '送信メールのサーバー',
  },
  { key: 'SMTP_PASSWORD', required: true, default: null, description: '' },
  {
    key: 'MAIL_FROM',
    required: false,
    default: 'noreply@example.com',
    description: '',
  },
  { key: 'RAILS_MAX_THREADS', required: false, default: '5', description: '' },
];

/** The keys of a `.env.example`; RAILS_ENV is saved but not listed. */
export const DOTENV_VARIABLES = [
  {
    key: 'DATABASE_URL',
    required: true,
    default: null,
    description: 'PostgreSQL の接続先',
  },
  {
    key: 'SECRET_KEY_BASE',
    required: true,
    default: null,
    description: 'openssl rand -hex 64 で作る',
  },
  {
    key: 'STRIPE_API_KEY',
    required: true,
    default: null,
    description: '決済に使う Stripe のシークレットキー',
  },
];

/** Starts a backup that the mock finishes by itself a little later. */
export function startBackup(state) {
  const run = {
    id: Date.now() % 100000,
    trigger: 'manual',
    status: 'running',
    started_at: ago(0),
    finished_at: null,
    error: '',
    mockFinishAt: Date.now() + 8 * 1000,
  };
  state.backups.runs.unshift(run);
  state.backups.running = true;
  return run;
}

/** Finishes a backup started through the mock once its time is up. */
export function settleBackups(state) {
  for (const run of state.backups.runs ?? []) {
    if (!run.mockFinishAt || run.mockFinishAt > Date.now()) continue;
    delete run.mockFinishAt;
    run.status = 'succeeded';
    run.finished_at = new Date().toISOString();
    state.backups.running = false;
    state.backups.last_success_at = run.finished_at;
    state.backups.warning = false;
    // The run leaves a snapshot behind for every app that had none.
    for (const list of state.snapshots?.values() ?? []) {
      if (list.length === 0) list.push(...snapshots().slice(0, 2));
    }
  }
}

/** A made-up CPU use (per core, as docker stats shows it) for a container. */
const CPU_PERCENT = { web: 3.6, db: 1.2, worker: 1.8 };

/**
 * Memory and CPU of the running containers, by app, as the services of each
 * app report them (so the figures match the overview tab), plus Caddy's
 * certificate helper and a registry that koroku does not manage.
 */
export function containerResources(state, servicesOf) {
  const groups = [];
  for (const target of state.apps.values()) {
    const running = servicesOf(target).filter(
      (service) => service.state === 'running',
    );
    if (running.length === 0) continue;
    // The shop is the busy one; staging is mostly idle.
    const quiet =
      target.environment !== 'production'
        ? 0.1
        : target.slug === 'shop'
          ? 2.4
          : 1;
    const containers = running.map((service) => ({
      name: `koroku-${target.slug}-${service.name}-1`,
      project: `koroku-${target.slug}`,
      service: service.name,
      memory_bytes: Math.round(service.memory_bytes),
      memory_limit_bytes: service.memory_limit_bytes,
      cpu_percent:
        Math.round((CPU_PERCENT[service.name] ?? 0.2) * quiet * 100) / 100,
    }));
    containers.sort((a, b) => b.memory_bytes - a.memory_bytes);
    groups.push(group(target.slug, containers));
  }
  groups.sort((a, b) => b.memory_bytes - a.memory_bytes);
  if (!state.noContainers) {
    groups.push(
      group(null, [
        {
          name: 'registry',
          project: '',
          service: '',
          memory_bytes: Math.round(0.02 * GB),
          memory_limit_bytes: null,
          cpu_percent: 0.01,
        },
      ]),
    );
  }
  return { cpu_count: 2, memory_total_bytes: 4 * GB, groups };
}

function group(app, containers) {
  const limits = containers.map((c) => c.memory_limit_bytes);
  return {
    app,
    memory_bytes: containers.reduce((sum, c) => sum + c.memory_bytes, 0),
    memory_limit_bytes: limits.every((limit) => typeof limit === 'number')
      ? limits.reduce((sum, limit) => sum + limit, 0)
      : null,
    cpu_percent:
      Math.round(containers.reduce((sum, c) => sum + c.cpu_percent, 0) * 100) /
      100,
    containers,
  };
}

/** Docker disk usage; after a prune nothing is left to reclaim. */
export function dockerUsage(state) {
  if (!state.pruned) return DOCKER_USAGE;
  return {
    items: DOCKER_USAGE.items.map((item) => ({
      ...item,
      total_bytes: item.total_bytes - item.reclaimable_bytes,
      reclaimable_bytes: 0,
      count: item.type === 'build_cache' ? 0 : item.count,
    })),
  };
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

/**
 * The named volumes Docker has for a deployed app, unless the scenario says
 * otherwise (`state.volumes`, by slug): the database (excluded from backups
 * in these fixtures), the uploads (backed up every night) and a cache that
 * a recent deployment added, which no backup has seen yet.
 */
const VOLUMES = ['cache', 'pgdata', 'uploads'];

/**
 * An app's volumes as `GET /api/apps/{slug}/volumes` lists them: what
 * exists, plus the names in the exclude list that match nothing. An app
 * that was never deployed has no volumes yet.
 */
export function appVolumes(state, app) {
  const existing = app.current_deployment
    ? (state.volumes?.get(app.slug) ?? VOLUMES)
    : [];
  const excluded = app.backup_exclude_volumes ?? [];
  return [...new Set([...existing, ...excluded])].sort().map((name) => ({
    name,
    excluded: excluded.includes(name),
    exists: existing.includes(name),
  }));
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

/**
 * What `GET /api/repos/{owner}/{repo}/env-presets` suggests for each mock
 * repository (ARCHITECTURE 9.8); `secret` marks a generated value, made
 * fresh on every request like koroku does.
 */
export const ENV_PRESETS = {
  'octo/blog': {
    compose_path: 'compose.yaml',
    frameworks: ['rails'],
    presets: [
      {
        key: 'RAILS_ENV',
        value: 'production',
        framework: 'rails',
        file: 'Gemfile',
        match: 'rails',
      },
      {
        key: 'SECRET_KEY_BASE',
        secret: 'hex64',
        framework: 'rails',
        file: 'Gemfile',
        match: 'rails',
      },
      {
        key: 'RAILS_LOG_TO_STDOUT',
        value: 'true',
        framework: 'rails',
        file: 'config/environments/production.rb',
        match: 'RAILS_LOG_TO_STDOUT',
      },
      {
        key: 'RAILS_SERVE_STATIC_FILES',
        value: 'true',
        framework: 'rails',
        file: 'config/environments/production.rb',
        match: 'RAILS_SERVE_STATIC_FILES',
      },
    ],
    skipped: [{ key: 'RACK_ENV', service: 'web' }],
  },
  'octo/shop': {
    compose_path: 'compose.yaml',
    frameworks: ['laravel'],
    presets: [
      {
        key: 'APP_ENV',
        value: 'production',
        framework: 'laravel',
        file: 'composer.json',
        match: 'laravel/framework',
      },
      {
        key: 'APP_DEBUG',
        value: 'false',
        framework: 'laravel',
        file: 'composer.json',
        match: 'laravel/framework',
      },
      {
        key: 'APP_KEY',
        secret: 'laravel',
        framework: 'laravel',
        file: 'composer.json',
        match: 'laravel/framework',
      },
    ],
    skipped: [],
  },
  'octo/wiki': {
    compose_path: 'compose.yaml',
    frameworks: [],
    presets: [],
    skipped: [],
  },
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
