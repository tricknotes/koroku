import type {
  ComposeView,
  ContainerInfo,
  ContainerView,
} from 'koroku-console/utils/types';

const MB = 1024 ** 2;

export const OVERRIDE = `services:
  "web":
    ports: !override
      - "127.0.0.1:20000:3000"
    oom_score_adj: -400
`;

export const CONFIG = `name: koroku-blog
services:
  web:
    environment:
      DATABASE_URL: "[MASKED]"
    oom_score_adj: -400
`;

/** The Compose view of blog's current release 42, with 41 kept too. */
export function makeComposeView(
  overrides: Partial<ComposeView> = {},
): ComposeView {
  return {
    deployment_id: 42,
    current_deployment_id: 42,
    releases: [
      {
        deployment_id: 42,
        commit_sha: 'a'.repeat(40),
        commit_message: 'Fix the header',
        finished_at: '2026-11-18T02:59:00Z',
        current: true,
      },
      {
        deployment_id: 41,
        commit_sha: 'b'.repeat(40),
        commit_message: 'Add search',
        finished_at: '2026-11-17T03:01:10Z',
        current: false,
      },
    ],
    compose_file: 'compose.yaml',
    files: ['compose.yaml', 'koroku.override.yaml'],
    override: OVERRIDE,
    config: CONFIG,
    config_error: '',
    ...overrides,
  };
}

export function makeContainer(
  overrides: Partial<ContainerInfo> = {},
): ContainerInfo {
  return {
    name: 'koroku-blog-web-1',
    service: 'web',
    image: 'koroku-blog-web:r42',
    image_id: 'sha256:' + 'c'.repeat(64),
    image_digest: null,
    created_at: '2026-11-18T02:58:30Z',
    deployment_id: 42,
    matched_by: 'image',
    current: true,
    state: 'running',
    health: 'healthy',
    exit_code: 0,
    started_at: '2026-11-18T02:58:31Z',
    finished_at: null,
    restart_count: 0,
    oom_killed: false,
    memory_limit_bytes: 512 * MB,
    memory_reservation_bytes: 256 * MB,
    oom_score_adj: -400,
    expected: {
      memory_limit_bytes: 512 * MB,
      memory_reservation_bytes: 256 * MB,
      oom_score_adj: -400,
    },
    ports: [
      { host_ip: '127.0.0.1', host_port: '20000', container_port: '3000/tcp' },
    ],
    mismatches: [],
    ...overrides,
  };
}

export function makeContainerView(
  containers: ContainerInfo[] = [makeContainer()],
  overrides: Partial<ContainerView> = {},
): ContainerView {
  return {
    current_deployment_id: 42,
    compared: true,
    comparison_error: '',
    containers,
    ...overrides,
  };
}
