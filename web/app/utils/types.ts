// Shapes of the koroku HTTP API (docs/API.md).

export interface SessionUser {
  id: number;
  login: string;
  avatar_url: string;
}

export interface SessionResponse {
  authenticated: boolean;
  user?: SessionUser;
  /**
   * Until when dangerous operations work without logging in again. `null`
   * (or absent) when the server does not ask for it (development mode).
   */
  reauth_until?: string | null;
}

/** A read-only API token (`GET /api/tokens`, ARCHITECTURE 10.5). */
export interface ApiToken {
  id: string;
  name: string;
  created_at: string;
  expires_at: string;
  /** Rounded to the minute; `null` until first used. */
  last_used_at: string | null;
  last_used_ip: string;
}

/** Answer to `POST /api/tokens`: the only time `value` is ever sent. */
export interface CreatedApiToken {
  token: ApiToken;
  value: string;
}

/** The expiries the server accepts, in days. */
export type TokenLifetime = 7 | 30 | 90;

/** A browser logged in as the user (`GET /api/sessions`). */
export interface LoginSession {
  id: string;
  created_at: string;
  last_seen_at: string;
  expires_at: string;
  ip: string;
  user_agent: string;
  /** The session of the browser that asked. */
  current: boolean;
}

export type LoginOutcome =
  'succeeded' | 'not_allowed' | 'two_factor_required' | 'two_factor_unknown';

/** One login attempt by a known GitHub user (`GET /api/login-events`). */
export interface LoginEvent {
  at: string;
  outcome: LoginOutcome;
  github_user_id: number;
  login: string;
  ip: string;
  user_agent: string;
}

export interface SystemInfo {
  version: string;
  base_domain: string;
  disk: {
    total_bytes: number;
    used_bytes: number;
    used_percent: number;
  } | null;
  memory: { total_bytes: number; available_bytes: number } | null;
  docker_version: string;
  // Added for the UI redesign (ARCHITECTURE 9.1); absent on older backends.
  /** Deployments are refused above this disk usage. */
  disk_limit_percent?: number;
  log_archive?: {
    configured: boolean;
    last_success_at: string | null;
    last_error: string;
    /** Next scheduled archive run; `null` when not configured. */
    next_run_at?: string | null;
  };
  caddy?: {
    enabled: boolean;
    last_loaded_at: string | null;
    last_error: string;
  };
  /** Host swap; `null` on a host without swap, absent on older backends. */
  swap?: { total_bytes: number; used_bytes: number } | null;
}

/** One line of `docker system df` (`GET /api/system/docker`). */
export interface DockerUsageItem {
  type: 'images' | 'containers' | 'volumes' | 'build_cache';
  total_bytes: number;
  /** What `POST /api/system/prune` would free (an estimate on the low side). */
  reclaimable_bytes: number;
  count: number;
}

/** One running container (`GET /api/system/containers`). */
export interface ContainerUsage {
  name: string;
  /** Compose labels; empty when the container is not from Compose. */
  project: string;
  service: string;
  memory_bytes: number;
  /** `null` when the container has no memory limit. */
  memory_limit_bytes: number | null;
  /** As `docker stats` shows it: one core is 100%, up to cores × 100. */
  cpu_percent: number;
}

/** The running containers of one app, or (`app: null`) of no app. */
export interface ContainerGroup {
  app: string | null;
  memory_bytes: number;
  /** Sum of the limits; `null` when any container has none. */
  memory_limit_bytes: number | null;
  cpu_percent: number;
  containers: ContainerUsage[];
}

/** `GET /api/system/containers`. */
export interface ContainerResources {
  cpu_count: number;
  memory_total_bytes: number;
  groups: ContainerGroup[];
}

export type DeploymentStatus =
  'queued' | 'running' | 'succeeded' | 'failed' | 'canceled';

export interface Deployment {
  id: number;
  app_slug: string;
  ref: string;
  commit_sha: string;
  commit_message: string;
  status: DeploymentStatus;
  error: string;
  queued_at: string | null;
  started_at: string | null;
  finished_at: string | null;
  // Added with the instant rollback (ARCHITECTURE 7.1); absent on older
  // backends.
  /** On a rollback: the deployment whose release it started again. */
  source_deployment_id?: number | null;
  /** The release can be started again without building it. */
  rollback_available?: boolean;
}

export type Environment = 'production' | 'staging';

export interface App {
  slug: string;
  repo_full_name: string;
  repo_url: string;
  default_branch: string;
  compose_path: string;
  web_service: string;
  web_port: number;
  host_port: number;
  healthcheck_path: string;
  desired_state: 'running' | 'stopped';
  environment: Environment;
  relax_policy: boolean;
  memory_limit: string;
  service_memory_limits: Record<string, string> | null;
  url: string;
  /** URL of the primary domain, or `url` when there is none. */
  primary_url?: string;
  /** Number of custom domains still waiting for DNS verification. */
  pending_domains?: number;
  current_deployment: Deployment | null;
  latest_deployment: Deployment | null;
  created_at: string;
  updated_at: string;
  // Added by the backend in M5; absent before that.
  backup_exclude_volumes?: string[] | null;
  dump_hook_service?: string;
  dump_hook_command?: string;
  backup_before_deploy?: boolean;
  /**
   * The load hook reads a dump from stdin; the post-load command runs after
   * a copy (ARCHITECTURE 10.5). Absent from older backends.
   */
  load_hook_service?: string;
  load_hook_command?: string;
  post_load_service?: string;
  post_load_command?: string;
  /** Where the variables the repository needs are read (ARCHITECTURE 9.4). */
  env_source?: EnvSource;
  /** The `.env.example` path in `dotenv` mode; '' for the default. */
  env_example_path?: string;
}

export interface AppPatch {
  default_branch?: string;
  compose_path?: string;
  web_service?: string;
  web_port?: number;
  healthcheck_path?: string;
  environment?: Environment;
  relax_policy?: boolean;
  memory_limit?: string;
  service_memory_limits?: Record<string, string>;
  backup_exclude_volumes?: string[];
  dump_hook_service?: string;
  dump_hook_command?: string;
  backup_before_deploy?: boolean;
  load_hook_service?: string;
  load_hook_command?: string;
  post_load_service?: string;
  post_load_command?: string;
  env_source?: EnvSource;
  env_example_path?: string;
}

export interface NewApp {
  slug: string;
  web_port: number;
  repo_full_name?: string;
  repo_url?: string;
  default_branch?: string;
  compose_path?: string;
  web_service?: string;
  healthcheck_path?: string;
  environment?: Environment;
}

export interface ServiceStatus {
  name: string;
  state: string;
  status: string;
  health: string;
  /** Memory in use and its limit (0: unlimited); `null` when unknown. */
  memory_bytes?: number | null;
  memory_limit_bytes?: number | null;
}

/** How a saved variable differs from the running release. */
export type EnvPending = 'added' | 'changed';

export interface EnvVar {
  key: string;
  value: string;
  /** Saved but not deployed yet; `null` when the release has it as is. */
  pending?: EnvPending | null;
}

/** `GET /api/apps/{slug}/env`, with hidden values turned into ''. */
export interface EnvList {
  vars: EnvVar[];
  /** Deleted, but still in the running release. */
  removed: string[];
  /** There is a running release the saved variables were compared with. */
  compared: boolean;
  /** The session is too old to see values: only the keys are real. */
  valuesHidden: boolean;
}

/**
 * Where the console reads the variables a repository needs: the Compose
 * file's `${VAR}` references, a `.env.example` file, or nowhere.
 */
export type EnvSource = 'compose' | 'dotenv' | 'off';

/** Why the requirements could not be read. */
export type EnvRequirementsReason =
  | 'file_not_found'
  | 'repo_unavailable'
  | 'unsupported_repo'
  | 'not_configured'
  | 'ref_not_found'
  | 'too_large'
  | 'rate_limited'
  | 'github_error';

export interface EnvRequirement {
  key: string;
  /** Must be set (`${VAR:?}`, `${VAR}` without a default, any `.env.example` key). */
  required: boolean;
  /** The Compose default (`${VAR:-x}`), `null` when there is none. */
  default: string | null;
  description: string;
  /** A variable with this key is saved. */
  set: boolean;
}

/** `GET /api/apps/{slug}/env/requirements`. */
export interface EnvRequirements {
  source: EnvSource;
  status: 'ok' | 'off' | 'unavailable';
  reason: EnvRequirementsReason | '';
  ref: string;
  commit_sha: string;
  /** The file that was read (or looked for); '' when none was found. */
  path: string;
  variables: EnvRequirement[];
  /** Saved keys the repository does not mention. */
  unlisted: string[];
}

export interface Repo {
  id: number;
  full_name: string;
  private: boolean;
  default_branch: string;
}

export interface Branch {
  name: string;
  sha: string;
}

export interface Commit {
  sha: string;
  message: string;
  author: string;
  date: string;
}

export interface ComposeService {
  name: string;
  ports: number[] | null;
}

export interface ComposeDetection {
  files: string[] | null;
  path: string;
  services: ComposeService[] | null;
}

export interface DnsRecord {
  type: string;
  name: string;
  value: string;
}

export interface Domain {
  id: number;
  hostname: string;
  status: 'pending' | 'active';
  is_primary: boolean;
  force_active: boolean;
  last_checked_at: string | null;
  last_error: string;
  records: DnsRecord[] | null;
}

export interface DomainList {
  default_hostname: string;
  domains: Domain[] | null;
}

export interface LogLine {
  service: string;
  time: string;
  line: string;
}

export interface LogHistory {
  lines: LogLine[] | null;
  truncated: boolean;
}

export interface LogArchive {
  key: string;
  service: string;
  hour: string;
  size: number;
}

/** One named volume of an app (`GET /api/apps/{slug}/volumes`). */
export interface AppVolume {
  /** The name in the Compose file, as the exclude list and snapshots use it. */
  name: string;
  /** Whether the name is in `backup_exclude_volumes`. */
  excluded: boolean;
  /** `false` for an excluded name that matches no volume in Docker. */
  exists: boolean;
}

export interface BackupRun {
  id: number;
  trigger: 'scheduled' | 'manual' | 'pre_deploy';
  status: 'running' | 'succeeded' | 'failed';
  started_at: string | null;
  finished_at: string | null;
  error: string;
}

export interface BackupOverview {
  configured: boolean;
  running: boolean;
  last_success_at: string | null;
  warning: boolean;
  /** Next scheduled run; `null` when backups are not configured. */
  next_run_at?: string | null;
  runs: BackupRun[] | null;
}

export type CopySourceKind = 'live' | 'snapshot';

/** One copy of a database into a staging app (ARCHITECTURE 10.5). */
export interface DbCopy {
  id: number;
  target_app: string;
  /** The source's slug; kept after the source app is deleted. */
  source_app: string;
  source_kind: CopySourceKind;
  status: 'running' | 'succeeded' | 'failed';
  error: string;
  /** Size of the dump that was streamed; 0 while it runs. */
  bytes: number;
  /** The target's dump taken before loading; '' when none was taken. */
  backup_snapshot_id: string;
  started_at: string;
  finished_at: string | null;
}

export interface Snapshot {
  id: string;
  time: string;
  kind: 'volume' | 'dump';
  target: string;
  /** What restic processed for it; `null` when it did not record a size. */
  size_bytes?: number | null;
}
