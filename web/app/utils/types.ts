// Shapes of the koroku HTTP API (docs/API.md).

export interface SessionUser {
  id: number;
  login: string;
  avatar_url: string;
  /**
   * The owner (`allowed_user_id`) may do everything; members only what
   * their per-app roles allow (ARCHITECTURE 10.7). Absent on older
   * backends, which only let the owner in.
   */
  owner?: boolean;
}

/** A member's role on one app, from the least to the most powerful. */
export type MemberRole = 'view' | 'deploy' | 'manage';

/** What the requesting user may do on an app (`App.role`). */
export type AppRole = MemberRole | 'owner';

/** Who started an operation; `null` when nobody did (or older records). */
export interface Actor {
  id: number;
  login: string;
}

/** A user named next to a session or token the owner sees. */
export interface UserRef {
  id: number;
  login: string;
}

/** `GET /api/members`: someone the owner let in, with roles per app. */
export interface Member {
  github_user_id: number;
  login: string;
  avatar_url: string;
  added_at: string;
  roles: { app: string; role: MemberRole }[];
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
  /** Whose token it is; the owner sees everyone's. */
  user?: UserRef;
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
  /** Whose session it is; the owner sees everyone's. */
  user?: UserRef;
}

/**
 * The server records `succeeded` and `not_allowed`. Histories written by
 * older versions may also hold the two-factor outcomes.
 */
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

/** `GET /api/system/github`: the GitHub App and where it is installed. */
export interface GitHubAppStatus {
  configured: boolean;
  app: {
    slug: string;
    html_url: string;
    /** Settings page; "Advanced" there makes it installable by any account. */
    settings_url: string;
    install_url: string;
  } | null;
  installations: {
    account: string;
    /** `all` or `selected`. */
    repository_selection: string;
    /** koroku lists and fetches this installation's repositories. */
    used: boolean;
  }[];
  installation_accounts: string[];
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
  /**
   * Where the GitHub App delivers pushes for automatic deployment
   * (ARCHITECTURE 5.2); absent on older backends.
   */
  webhook?: { url: string; secret_configured: boolean };
  /**
   * Whether apps can sleep while idle on this host: Caddy writes per-app
   * access logs (ARCHITECTURE 8.4). Absent on older backends.
   */
  idle_sleep?: { available: boolean };
  /**
   * What production apps reserve (ARCHITECTURE 6.2); `null` for members,
   * absent on older backends.
   */
  memory_reservation?: MemoryReservationSummary | null;
}

export interface MemoryReservationSummary {
  total_bytes: number;
  /** Left free for the host, builds and staging apps. */
  margin_bytes: number;
  /** The reservations plus the margin exceed the host's memory. */
  warning: boolean;
  apps: {
    slug: string;
    environment: Environment;
    total_bytes: number;
    services: Record<string, number>;
  }[];
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

/**
 * An automatic deployment received while a command ran on the app, queued
 * once the command ends (ARCHITECTURE 5.2).
 */
export interface PendingAutoDeploy {
  ref: string;
  commit_sha: string;
  commit_message: string;
  pusher: string;
  /** The workflow the push waited for, or "". */
  workflow: string;
  received_at: string;
}

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
  /** The staging release a promotion deployed (ARCHITECTURE 9.7). */
  promoted_from?: { deployment_id: number; app_slug: string } | null;
  /** Who asked for it (ARCHITECTURE 10.7). */
  actor?: Actor | null;
  /** What started it (ARCHITECTURE 5.2); absent on older backends. */
  trigger?: DeploymentTrigger;
  /** The GitHub login that pushed, for a push-triggered deployment. */
  pusher?: string;
}

/** `manual`: the console or the API (rollbacks and promotions too). */
export type DeploymentTrigger = 'manual' | 'push';

/** A named command kept for one-click runs (ARCHITECTURE 9.6). */
export interface Task {
  name: string;
  service: string;
  command: string;
  /** A five-field cron expression; '' (or absent) runs only by hand. */
  schedule?: string;
  /** Read-only: the next scheduled time. */
  next_run_at?: string | null;
  /** Read-only: the latest scheduled time that was not run, and why. */
  last_skipped_at?: string | null;
  last_skip_reason?: TaskSkipReason | '';
  last_skip_detail?: string;
}

/** Why a scheduled time was not run (ARCHITECTURE 9.6). */
export type TaskSkipReason =
  'stopped' | 'not_deployed' | 'overlap' | 'busy' | 'error';

/** The registered tasks, with the time zone their schedules are read in. */
export interface TaskList {
  tasks: Task[];
  time_zone: string;
}

/** Runs are never queued: they start at once or are refused. */
export type TaskRunStatus = 'running' | 'succeeded' | 'failed' | 'canceled';

/** One run of a command in a service of an app. */
export interface TaskRun {
  id: number;
  app_slug: string;
  /** The registered task it came from; '' for a command typed on the spot. */
  task_name: string;
  service: string;
  command: string;
  /** The release it ran in. */
  deployment_id: number | null;
  status: TaskRunStatus;
  /** `null` until it ends, and when it was canceled or timed out. */
  exit_code: number | null;
  error: string;
  started_at: string;
  finished_at: string | null;
  /** Who started it; nobody for a scheduled run. */
  actor?: Actor | null;
  /** What started it. */
  trigger?: 'manual' | 'scheduled';
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
  /** Production only (ARCHITECTURE 6.2): '' = half the limit, '0' = none. */
  memory_reservation?: string;
  service_memory_reservations?: Record<string, string> | null;
  url: string;
  /** URL of the primary domain, or `url` when there is none. */
  primary_url?: string;
  /** Number of custom domains still waiting for DNS verification. */
  pending_domains?: number;
  current_deployment: Deployment | null;
  latest_deployment: Deployment | null;
  created_at: string;
  updated_at: string;
  /**
   * What the user may do on this app (ARCHITECTURE 10.7). Absent on older
   * backends, where the only user is the owner.
   */
  role?: AppRole;
  // Added by the backend in M5; absent before that.
  backup_exclude_volumes?: string[] | null;
  dump_hook_service?: string;
  dump_hook_command?: string;
  backup_before_deploy?: boolean;
  /**
   * The load hook reads a dump from stdin; the post-load command runs after
   * a copy (ARCHITECTURE 10.6). Absent from older backends.
   */
  load_hook_service?: string;
  load_hook_command?: string;
  post_load_service?: string;
  post_load_command?: string;
  /** Where the variables the repository needs are read (ARCHITECTURE 9.4). */
  env_source?: EnvSource;
  /** The `.env.example` path in `dotenv` mode; '' for the default. */
  env_example_path?: string;
  /** Only the logged-in console user can open the app (ARCHITECTURE 8.3). */
  access_protection?: boolean;
  /** Path prefixes that skip the access protection, e.g. `/webhooks/`. */
  protection_exempt_paths?: string[] | null;
  /** The staging app a production app promotes from; '' when none. */
  promote_from?: string;
  /** Deploy pushes to `auto_deploy_branch` (ARCHITECTURE 5.2). */
  auto_deploy?: boolean;
  /** The branch whose pushes deploy; '' follows `default_branch`. */
  auto_deploy_branch?: string;
  /**
   * The workflow file whose successful run deploys instead of the push,
   * e.g. `.github/workflows/image.yml`; '' deploys on push.
   */
  auto_deploy_workflow?: string;
  /** Stop the app after `idle_sleep_minutes` without a request (ARCHITECTURE 8.4). */
  idle_sleep?: boolean;
  idle_sleep_minutes?: number;
  /** When the app went to sleep while idle; null while it is awake. */
  sleeping_since?: string | null;
  /** Set on a review app of a pull request (ARCHITECTURE 9.9). */
  review?: ReviewInfo | null;
  /** The owner lets review apps be created from this app (ARCHITECTURE 9.9). */
  review_apps_allowed?: boolean;
  /** How many review apps the app has; null on a review app. */
  review_apps?: ReviewSummary | null;
}

export interface ReviewSummary {
  count: number;
  /** Review apps whose latest deployment or copy of the data failed. */
  failed: number;
  /** Review apps with a deployment queued or running. */
  deploying: number;
}

export type PullRequestState = 'open' | 'closed' | 'merged';

/**
 * The copy of the parent's data into a review app: '' not asked, then
 * waiting for the first successful deployment, started (see the database
 * copies), skipped or failed (why in `data_copy_detail`).
 */
export type ReviewDataCopy = '' | 'waiting' | 'started' | 'skipped' | 'failed';

export interface ReviewInfo {
  parent: string;
  pr_number: number;
  pr_title: string;
  pr_url: string;
  pr_state: PullRequestState;
  head_branch: string;
  data_copy: ReviewDataCopy;
  data_copy_detail: string;
  /** Status of the newest copy from the parent; '' when none. */
  data_copy_status?: '' | 'running' | 'succeeded' | 'failed';
}

/** An open pull request of a review parent's repository. */
export interface PullRequest {
  number: number;
  title: string;
  head_branch: string;
  head_sha: string;
  author: string;
  url: string;
  updated_at: string;
  from_fork: boolean;
  /** Slug of the pull request's review app, or null. */
  review_app: string | null;
}

/** The answer to creating a review app. */
export interface CreatedReviewApp extends App {
  /** The hostname does not resolve to the host yet (no wildcard record). */
  dns_warning: boolean;
  /** The first deployment could not be queued. */
  deploy_error?: string;
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
  memory_reservation?: string;
  service_memory_reservations?: Record<string, string>;
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
  access_protection?: boolean;
  protection_exempt_paths?: string[];
  promote_from?: string;
  auto_deploy?: boolean;
  auto_deploy_branch?: string;
  auto_deploy_workflow?: string;
  idle_sleep?: boolean;
  idle_sleep_minutes?: number;
  /** Only the owner changes it (ARCHITECTURE 9.9). */
  review_apps_allowed?: boolean;
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
  /** Variables stored with the app (ARCHITECTURE 9.8). */
  env?: { key: string; value: string }[];
}

export interface ServiceStatus {
  name: string;
  state: string;
  status: string;
  health: string;
  /** Memory in use and its limit (0: unlimited); `null` when unknown. */
  memory_bytes?: number | null;
  memory_limit_bytes?: number | null;
  /** CPU as Docker shows it (one core is 100%); `null` when unknown. */
  cpu_percent?: number | null;
  /** What the running release reserves for it; `null` for none. */
  memory_reservation_bytes?: number | null;
}

export type MemoryRange = '24h' | '7d' | '14d';

/** One point of a service's memory history: a 5-minute bucket or several. */
export interface MemoryPoint {
  at: string;
  max_bytes: number;
  avg_bytes: number;
  limit_bytes: number | null;
}

export type MemoryFlag = 'near_limit' | 'generous';

/**
 * The limit suggestion, from the data recorded under the current limit in
 * the last 14 days (ARCHITECTURE 9.5).
 */
export interface MemorySuggestion {
  basis_since: string;
  basis_hours: number;
  peak_bytes: number;
  p95_bytes: number;
  /** At least 24 hours of data; `limit` is null until then. */
  ready: boolean;
  limit_bytes: number | null;
  /** The suggestion as a setting, e.g. `640m`. */
  limit: string | null;
  flag: MemoryFlag | null;
}

export interface MemoryServiceHistory {
  name: string;
  limit_bytes: number | null;
  peak_bytes: number;
  restarts: number;
  points: MemoryPoint[];
  suggestion: MemorySuggestion;
}

/** `GET /api/apps/{slug}/memory`. */
export interface MemoryHistory {
  range: MemoryRange;
  step_seconds: number;
  from: string;
  to: string;
  services: MemoryServiceHistory[];
}

/** How a saved variable differs from the running release. */
export type EnvPending = 'added' | 'changed';

export interface EnvVar {
  key: string;
  value: string;
  /** Saved but not deployed yet; `null` when the release has it as is. */
  pending?: EnvPending | null;
}

/**
 * `GET /api/apps/{slug}/registry-credential`: the app's container registry
 * login (ARCHITECTURE 6.1). The token is never sent back.
 */
export interface RegistryCredential {
  configured: boolean;
  /** The saved registry, or the suggested default when none is set. */
  registry: string;
  username: string;
  updated_at: string | null;
}

/** The body of `PUT /api/apps/{slug}/registry-credential`. */
export interface RegistryCredentialInput {
  registry: string;
  username: string;
  /** Empty keeps the saved token. */
  token: string;
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
  /** The user's role never shows values (below manage). */
  valuesForbidden: boolean;
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

/** One variable the create screen prefills (ARCHITECTURE 9.8). */
export interface EnvPreset {
  key: string;
  value: string;
  /** A secret koroku generated; masked until revealed. */
  generated: boolean;
  framework: string;
  /** Why: the file read and what in it matched ('' = the file exists). */
  file: string;
  match: string;
}

/** `GET /api/repos/{owner}/{repo}/env-presets`. */
export interface EnvPresets {
  status: 'ok' | 'unavailable';
  reason: string;
  compose_path: string;
  frameworks: string[];
  presets: EnvPreset[];
  /** Presets left out because the Compose file sets them itself. */
  skipped: { key: string; service: string }[];
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
  /**
   * Apps whose latest backup did not fully succeed (only those the user
   * has a role on). Older backends send none.
   */
  problems?: BackupProblem[];
  /** The system's latest backup failed (owner only). */
  system_failed?: boolean;
}

export type BackupTargetStatus = 'succeeded' | 'warning' | 'failed';

/** An app whose latest backup did not fully succeed (ARCHITECTURE 10.2). */
export interface BackupProblem {
  app: string;
  status: Exclude<BackupTargetStatus, 'succeeded'>;
  /** The run failed before backing anything up: every app is concerned. */
  whole_run: boolean;
}

/** What a run did to one volume or dump of an app. */
export interface BackupTargetOutcome {
  kind: 'system' | 'volume' | 'dump';
  target: string;
  status: BackupTargetStatus;
  /** The (masked) reason when it did not simply succeed. */
  error: string;
}

/** The latest run that concerns one app, reduced to that app's targets. */
export interface AppBackupRun {
  id: number;
  trigger: BackupRun['trigger'];
  started_at: string;
  finished_at: string | null;
  status: BackupTargetStatus;
  whole_run: boolean;
  /** The run's message when `whole_run`; empty for members. */
  error: string;
  targets: BackupTargetOutcome[];
}

/** `GET /api/apps/{slug}/backups/status`. */
export interface AppBackupStatus {
  configured: boolean;
  last_success_at: string | null;
  warning: boolean;
  last_run: AppBackupRun | null;
}

export type CopySourceKind = 'live' | 'snapshot';

/** One copy of a database into a staging app (ARCHITECTURE 10.6). */
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
  /** Who started it. */
  actor?: Actor | null;
}

export interface Snapshot {
  id: string;
  time: string;
  kind: 'volume' | 'dump';
  target: string;
  /** What restic processed for it; `null` when it did not record a size. */
  size_bytes?: number | null;
}

/** Why a promotion cannot happen (`GET /api/apps/{slug}/promotion`). */
export type PromotionReason =
  | 'not_production'
  | 'source_not_staging'
  | 'repository_mismatch'
  | 'no_source_release';

/** Why a built service is built again instead of reusing staging's image. */
export type ImageRebuildReason =
  | 'args_differ'
  | 'build_differs'
  | 'not_built_in_source'
  | 'image_missing'
  | 'source_missing';

export interface PromotionImage {
  service: string;
  reuse: boolean;
  reason: ImageRebuildReason | '';
  /** Keys of `build.args` whose values differ between the apps. */
  differing_args: string[];
}

/** What promoting the paired staging release would do (ARCHITECTURE 9.7). */
export interface Promotion {
  status: 'ready' | 'not_paired' | 'unavailable';
  reason: PromotionReason | '';
  source_app: string;
  source_deployment: Deployment | null;
  current_deployment: Deployment | null;
  commits: { status: 'ok' | 'unknown'; list: Commit[] };
  env: { only_in_source: string[]; only_in_target: string[] };
  images: { status: 'checked' | 'unavailable'; services: PromotionImage[] };
}

/**
 * One thing someone did to an app (`GET /api/apps/{slug}/activity`,
 * ARCHITECTURE 10.7). The shape of `details` depends on `action`; it never
 * holds secret values.
 */
export interface ActivityEntry {
  id: number;
  at: string;
  /** Who did it; `null` for koroku itself. */
  actor: Actor | null;
  action: string;
  details: Record<string, unknown>;
}

/** A page of activity, newest first. */
export interface ActivityPage {
  entries: ActivityEntry[];
  /** Pass as `before` for the next page; `null` on the last one. */
  next_before: number | null;
}

/** A kept release the Compose tab can show (ARCHITECTURE 9.10). */
export interface ComposeRelease {
  deployment_id: number;
  commit_sha: string;
  commit_message: string;
  finished_at: string | null;
  current: boolean;
}

/** The override and merged configuration of one release, values masked. */
export interface ComposeView {
  deployment_id: number;
  current_deployment_id: number | null;
  releases: ComposeRelease[];
  compose_file: string;
  files: string[];
  override: string;
  /** `null` when `docker compose config` failed; see `config_error`. */
  config: string | null;
  config_error: string;
}

export type ContainerMismatch =
  | 'outdated'
  | 'orphan'
  | 'memory_limit'
  | 'memory_reservation'
  | 'oom_score_adj'
  | 'oom_killed'
  | 'restarting'
  | 'restarted'
  | 'unhealthy'
  | 'exited';

export interface ContainerExpected {
  memory_limit_bytes: number;
  memory_reservation_bytes: number;
  oom_score_adj: number;
}

export interface PublishedPort {
  host_ip: string;
  host_port: string;
  container_port: string;
}

/** One container of the app as `docker container inspect` shows it. */
export interface ContainerInfo {
  name: string;
  service: string;
  image: string;
  image_id: string;
  image_digest: string | null;
  created_at: string;
  deployment_id: number | null;
  matched_by: '' | 'image' | 'created_at';
  /** From the current release's configuration; `null` when not compared. */
  current: boolean | null;
  state: string;
  health: string;
  exit_code: number;
  started_at: string | null;
  finished_at: string | null;
  restart_count: number;
  oom_killed: boolean;
  /** 0 means none, as Docker reports it. */
  memory_limit_bytes: number;
  memory_reservation_bytes: number;
  oom_score_adj: number;
  expected: ContainerExpected | null;
  ports: PublishedPort[];
  mismatches: ContainerMismatch[];
}

export interface ContainerView {
  current_deployment_id: number | null;
  compared: boolean;
  comparison_error: string;
  containers: ContainerInfo[];
}
