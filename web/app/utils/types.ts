// Shapes of the paasd HTTP API (docs/API.md).

export interface SessionUser {
  id: number;
  login: string;
  avatar_url: string;
}

export interface SessionResponse {
  authenticated: boolean;
  user?: SessionUser;
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
  reclaimable_bytes: number;
  count: number;
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
}

export interface EnvVar {
  key: string;
  value: string;
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

export interface Snapshot {
  id: string;
  time: string;
  kind: 'volume' | 'dump';
  target: string;
  /** What restic processed for it; `null` when it did not record a size. */
  size_bytes?: number | null;
}
