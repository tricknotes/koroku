import {
  SHA_A,
  SHA_B,
  makeApp,
  makeAppBackupStatus,
  makeBackups,
  makeDeployment,
  makeSystem,
  makeVolume,
  type Fakes,
} from './fakes';
import { makeComposeView, makeContainerView } from './compose';
import type {
  App,
  BackupOverview,
  Deployment,
  Domain,
  EnvVar,
  Snapshot,
} from 'koroku-console/utils/types';

const GB = 1024 ** 3;

/** What the fake backend holds; tests change it between requests. */
export interface Console {
  app: App;
  deployments: Deployment[];
  domains: Domain[];
  backups: BackupOverview;
  snapshots: Snapshot[];
  env: EnvVar[];
}

/** One screen of the console, by the URL that restores it. */
export interface Screen {
  name: string;
  url: string;
  /** The tab of the app detail it shows, if any. */
  tab?: string;
}

/** Every screen behind the login, with the app `blog` of `serveConsole`. */
export const SCREENS: Screen[] = [
  { name: 'app list', url: '/' },
  { name: 'create app', url: '/apps/new' },
  { name: 'overview', url: '/apps/blog', tab: 'index' },
  { name: 'deploy', url: '/apps/blog/deploy', tab: 'deploy' },
  {
    name: 'review apps',
    url: '/apps/blog/deploy?section=review-apps',
    tab: 'deploy',
  },
  { name: 'logs', url: '/apps/blog/logs', tab: 'logs' },
  { name: 'log search', url: '/apps/blog/logs?period=1h', tab: 'logs' },
  { name: 'log archives', url: '/apps/blog/logs?period=archive', tab: 'logs' },
  { name: 'resources', url: '/apps/blog/resources', tab: 'resources' },
  {
    name: 'compose',
    url: '/apps/blog/resources?section=compose',
    tab: 'resources',
  },
  { name: 'tasks', url: '/apps/blog/tasks', tab: 'tasks' },
  { name: 'environment variables', url: '/apps/blog/env', tab: 'env' },
  { name: 'backups', url: '/apps/blog/backups', tab: 'backups' },
  { name: 'settings', url: '/apps/blog/settings', tab: 'settings' },
  {
    name: 'domains',
    url: '/apps/blog/settings?section=domains',
    tab: 'settings',
  },
  { name: 'system', url: '/system' },
  { name: 'members', url: '/members' },
];

/**
 * Answers every read of the console for one host with the app `blog`, so
 * any screen can be opened with something on it. The app is running, with a
 * primary domain and one that still waits for its DNS record. Mutations are
 * left to the test.
 */
export function serveConsole(
  fakes: Fakes,
  overrides: Partial<App> = {},
): Console {
  const running = makeDeployment({ id: 42 });
  const state: Console = {
    app: makeApp({
      current_deployment: running,
      latest_deployment: running,
      primary_url: 'https://www.example.org',
      pending_domains: 1,
      backup_exclude_volumes: [],
      dump_hook_service: 'db',
      dump_hook_command: 'pg_dump -U postgres app',
      backup_before_deploy: false,
      ...overrides,
    }),
    deployments: [
      running,
      makeDeployment({
        id: 41,
        commit_sha: SHA_B,
        commit_message: 'Add search',
        queued_at: '2026-11-17T03:00:00Z',
        started_at: '2026-11-17T03:00:01Z',
        finished_at: '2026-11-17T03:01:10Z',
      }),
    ],
    domains: [
      {
        id: 2,
        hostname: 'www.example.org',
        status: 'active',
        is_primary: true,
        force_active: false,
        last_checked_at: '2026-11-18T03:00:00Z',
        last_error: '',
        records: [
          {
            type: 'CNAME',
            name: 'www.example.org',
            value: 'blog.dev.example.com',
          },
        ],
      },
      {
        id: 3,
        hostname: 'example.net',
        status: 'pending',
        is_primary: false,
        force_active: false,
        last_checked_at: '2026-11-18T03:05:00Z',
        last_error: 'lookup example.net: no such host',
        records: [{ type: 'A', name: 'example.net', value: '203.0.113.10' }],
      },
    ],
    backups: makeBackups({
      runs: [
        {
          id: 12,
          trigger: 'scheduled',
          status: 'succeeded',
          started_at: '2026-11-17T18:00:00Z',
          finished_at: '2026-11-17T18:00:40Z',
          error: '',
        },
      ],
    }),
    snapshots: [
      {
        id: '4bba301e',
        time: '2026-11-17T18:00:12Z',
        kind: 'volume',
        target: 'uploads',
        size_bytes: 214748364,
      },
      {
        id: '77aa00ff',
        time: '2026-11-17T18:00:30Z',
        kind: 'dump',
        target: 'db',
        size_bytes: null,
      },
    ],
    env: [
      { key: 'DATABASE_URL', value: 'postgres://app:hunter2@db/app' },
      { key: 'RAILS_ENV', value: 'production' },
    ],
  };
  const { slug } = state.app;
  const app = `/api/apps/${slug}`;

  fakes.api
    .on('GET', '/api/apps', () => ({ apps: [state.app] }))
    .on('GET', '/api/members', {
      members: [
        {
          github_user_id: 2,
          login: 'mia',
          avatar_url: '',
          added_at: '2026-11-10T03:00:00Z',
          roles: [{ app: slug, role: 'deploy' }],
        },
      ],
    })
    .on('GET', '/api/system', () =>
      makeSystem({
        swap: { total_bytes: 2 * GB, used_bytes: 0.5 * GB },
        log_archive: {
          configured: true,
          last_success_at: '2026-11-18T03:10:05Z',
          last_error: '',
          next_run_at: '2026-11-18T07:10:00Z',
        },
      }),
    )
    .on('GET', '/api/system/containers', {
      cpu_count: 2,
      memory_total_bytes: 4 * GB,
      groups: [
        {
          app: slug,
          memory_bytes: 300 * 1024 ** 2,
          memory_limit_bytes: 512 * 1024 ** 2,
          cpu_percent: 1.5,
          containers: [
            {
              name: `koroku-${slug}-web-1`,
              project: `koroku-${slug}`,
              service: 'web',
              memory_bytes: 300 * 1024 ** 2,
              memory_limit_bytes: 512 * 1024 ** 2,
              cpu_percent: 1.5,
            },
          ],
        },
      ],
    })
    .on('GET', '/api/system/docker', {
      items: [
        {
          type: 'images',
          total_bytes: 5 * GB,
          reclaimable_bytes: GB,
          count: 12,
        },
        {
          type: 'containers',
          total_bytes: 1048576,
          reclaimable_bytes: 0,
          count: 6,
        },
        {
          type: 'volumes',
          total_bytes: 2 * GB,
          reclaimable_bytes: 0,
          count: 4,
        },
        {
          type: 'build_cache',
          total_bytes: 3 * GB,
          reclaimable_bytes: 3 * GB,
          count: 58,
        },
      ],
    })
    .on('GET', '/api/backups', () => state.backups)
    .on('GET', app, () => state.app)
    .on('GET', `${app}/backups/status`, makeAppBackupStatus())
    .on('GET', `${app}/compose`, makeComposeView())
    .on('GET', `${app}/containers`, makeContainerView())
    .on('GET', `${app}/services`, {
      services: [
        {
          name: 'db',
          state: 'running',
          status: 'Up 2 hours (healthy)',
          health: 'healthy',
          memory_bytes: 180 * 1024 ** 2,
          memory_limit_bytes: GB,
          cpu_percent: 0.26,
        },
        {
          name: 'web',
          state: 'running',
          status: 'Up 2 hours',
          health: '',
          memory_bytes: 300 * 1024 ** 2,
          memory_limit_bytes: 512 * 1024 ** 2,
          cpu_percent: 6.04,
        },
      ],
    })
    .on('GET', `${app}/memory`, {
      range: '24h',
      step_seconds: 300,
      from: '2026-11-17T03:00:00Z',
      to: '2026-11-18T03:00:00Z',
      services: [
        {
          name: 'web',
          limit_bytes: 512 * 1024 ** 2,
          peak_bytes: 470 * 1024 ** 2,
          restarts: 1,
          points: [
            {
              at: '2026-11-18T02:50:00Z',
              max_bytes: 470 * 1024 ** 2,
              avg_bytes: 400 * 1024 ** 2,
              limit_bytes: 512 * 1024 ** 2,
            },
            {
              at: '2026-11-18T02:55:00Z',
              max_bytes: 300 * 1024 ** 2,
              avg_bytes: 290 * 1024 ** 2,
              limit_bytes: 512 * 1024 ** 2,
            },
          ],
          suggestion: {
            basis_since: '2026-11-10T03:00:00Z',
            basis_hours: 192,
            peak_bytes: 470 * 1024 ** 2,
            p95_bytes: 320 * 1024 ** 2,
            ready: true,
            limit_bytes: 768 * 1024 ** 2,
            limit: '768m',
            flag: 'near_limit',
          },
        },
      ],
    })
    .on('GET', `${app}/deployments`, () => ({
      deployments: state.deployments,
    }))
    .on('GET', `${app}/env`, () => ({ vars: state.env }))
    .on('GET', `${app}/registry-credential`, {
      configured: true,
      registry: 'ghcr.io',
      username: 'octo-bot',
      updated_at: '2026-11-16T03:00:00Z',
    })
    .on('GET', `${app}/tasks`, {
      tasks: [
        {
          name: 'マイグレーションの状態',
          service: 'web',
          command: 'bin/rails db:migrate:status',
        },
      ],
    })
    .on('GET', `${app}/task-runs`, {
      runs: [
        {
          id: 7,
          app_slug: slug,
          task_name: 'マイグレーションの状態',
          service: 'web',
          command: 'bin/rails db:migrate:status',
          deployment_id: 42,
          status: 'succeeded',
          exit_code: 0,
          error: '',
          started_at: '2026-11-18T03:30:00Z',
          finished_at: '2026-11-18T03:30:04Z',
        },
      ],
    })
    .on('GET', `${app}/domains`, () => ({
      default_hostname: `${slug}.dev.example.com`,
      domains: state.domains,
    }))
    .on('GET', `${app}/review-apps`, { review_apps: [] })
    .on('GET', `${app}/backups`, () => ({ snapshots: state.snapshots }))
    .on('GET', `${app}/volumes`, {
      volumes: [makeVolume('pgdata'), makeVolume('uploads')],
    })
    .on('GET', `${app}/logs/history`, {
      lines: [
        {
          service: 'web',
          time: '2026-11-18T03:00:00.123Z',
          line: 'GET /up 200',
        },
        {
          service: 'db',
          time: '2026-11-18T03:00:01.456Z',
          line: 'checkpoint complete',
        },
      ],
      truncated: false,
    })
    .on('GET', `${app}/logs/archives`, {
      archives: [
        {
          key: `logs/${slug}/web/2026/11/01/04.log.gz`,
          service: 'web',
          hour: '2026-11-01T04:00:00+09:00',
          size: 12345,
        },
      ],
    })
    .on('GET', '/api/repos', {
      repos: [
        {
          id: 1,
          full_name: 'octo/blog',
          private: true,
          default_branch: 'main',
        },
      ],
    })
    .on('GET', '/api/repos/octo/blog/branches', {
      branches: [
        { name: 'main', sha: SHA_A },
        { name: 'feature/search', sha: SHA_B },
      ],
    })
    .on('GET', '/api/repos/octo/blog/commits', {
      commits: [
        {
          sha: SHA_A,
          message: 'Fix login',
          author: 'octo',
          date: '2026-11-18T03:00:00Z',
        },
        {
          sha: SHA_B,
          message: 'Add search',
          author: 'octo',
          date: '2026-11-17T03:00:00Z',
        },
      ],
    })
    .on('GET', '/api/repos/octo/blog/compose', {
      files: ['compose.yaml'],
      path: 'compose.yaml',
      services: [
        { name: 'db', ports: [5432] },
        { name: 'web', ports: [3000] },
      ],
    });

  return state;
}

/**
 * Puts the console into its busiest state, the one in which every screen
 * has something to follow: a deployment in progress, a domain waiting for
 * DNS (already there) and a backup that is running.
 */
export function makeBusy(state: Console): void {
  const deploying = makeDeployment({
    id: 43,
    status: 'running',
    finished_at: null,
  });
  state.deployments = [deploying, ...state.deployments];
  state.app = { ...state.app, latest_deployment: deploying };
  state.backups = {
    ...state.backups,
    running: true,
    runs: [
      {
        id: 13,
        trigger: 'manual',
        status: 'running',
        started_at: '2026-11-18T03:20:00Z',
        finished_at: null,
        error: '',
      },
      ...(state.backups.runs ?? []),
    ],
  };
}
