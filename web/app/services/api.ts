import Service, { service } from '@ember/service';
import { waitForPromise } from '@ember/test-waiters';
import { ApiError } from 'koroku-console/utils/errors';
import type SessionService from './session';
import type {
  ActivityPage,
  ApiToken,
  App,
  AppPatch,
  AppVolume,
  AppBackupStatus,
  BackupOverview,
  BackupRun,
  Branch,
  Commit,
  ComposeDetection,
  CreatedApiToken,
  ContainerResources,
  CreatedReviewApp,
  GitHubAppStatus,
  PullRequest,
  CopySourceKind,
  DbCopy,
  Deployment,
  DockerUsageItem,
  Domain,
  DomainList,
  EnvList,
  RegistryCredential,
  RegistryCredentialInput,
  EnvPresets,
  EnvRequirements,
  EnvPending,
  Promotion,
  EnvVar,
  LogArchive,
  LoginEvent,
  LoginSession,
  LogHistory,
  Member,
  MemberRole,
  MemoryHistory,
  MemoryRange,
  ComposeView,
  ContainerView,
  NewApp,
  Repo,
  ServiceStatus,
  SessionResponse,
  Snapshot,
  SystemInfo,
  Task,
  TaskList,
  TaskRun,
  TokenLifetime,
} from 'koroku-console/utils/types';

export type Query = Record<
  string,
  string | number | boolean | null | undefined
>;

export interface RequestOptions {
  query?: Query;
  body?: unknown;
}

/** Path plus query string; empty, null and undefined values are left out. */
export function buildUrl(path: string, query?: Query): string {
  if (!query) return path;
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value === undefined || value === null || value === '') continue;
    params.set(key, String(value));
  }
  const search = params.toString();
  return search ? `${path}?${search}` : path;
}

const seg = encodeURIComponent;

/** The task list of a response; older servers send no time zone. */
function taskList(response: Partial<TaskList> | null): TaskList {
  return {
    tasks: response?.tasks ?? [],
    time_zone: response?.time_zone ?? 'Asia/Tokyo',
  };
}

/** `/api/repos/{owner}/{repo}` from `owner/repo`. */
export function repoPath(fullName: string): string {
  return `/api/repos/${fullName.split('/').map(seg).join('/')}`;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * `details` of an error response as a record. Most errors send an object;
 * `policy_violation` sends the list of findings, kept under `violations`.
 */
function errorDetails(details: unknown): Record<string, unknown> {
  if (isRecord(details)) return details;
  if (Array.isArray(details)) return { violations: details };
  return {};
}

/** Builds an `ApiError` from an error response body (JSON or not). */
export function parseErrorBody(status: number, text: string): ApiError {
  try {
    const parsed: unknown = JSON.parse(text);
    if (isRecord(parsed) && isRecord(parsed['error'])) {
      const error = parsed['error'];
      return new ApiError(
        status,
        typeof error['code'] === 'string' ? error['code'] : 'unknown',
        typeof error['message'] === 'string' ? error['message'] : '',
        errorDetails(error['details']),
      );
    }
  } catch {
    // Not JSON: fall through to the generic error below.
  }
  return new ApiError(status, 'unknown', `HTTP ${status}`);
}

/**
 * The only place that talks to the koroku HTTP API. Every call goes through
 * `transport`, so tests register a subclass that overrides just that method.
 */
export default class ApiService extends Service {
  @service declare session: SessionService;

  /** Performs one HTTP exchange. Resolves with the parsed JSON body. */
  async transport(
    method: string,
    url: string,
    body?: unknown,
  ): Promise<unknown> {
    const headers: Record<string, string> = { Accept: 'application/json' };
    const init: RequestInit = { method, headers, credentials: 'same-origin' };
    if (method !== 'GET') {
      // The API takes exactly one JSON object as the body of a mutation.
      headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(body ?? {});
    }

    let response: Response;
    try {
      response = await fetch(url, init);
    } catch {
      throw new ApiError(0, 'network_error', '');
    }
    if (!response.ok) {
      throw parseErrorBody(response.status, await response.text());
    }
    if (response.status === 204) return undefined;
    const text = await response.text();
    return text ? (JSON.parse(text) as unknown) : undefined;
  }

  async request<T>(
    method: string,
    path: string,
    options: RequestOptions = {},
  ): Promise<T> {
    try {
      const result = await waitForPromise(
        this.transport(method, buildUrl(path, options.query), options.body),
      );
      return result as T;
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        this.session.invalidate();
      }
      if (error instanceof ApiError && error.code === 'reauth_required') {
        this.session.promptReauth();
      }
      throw error;
    }
  }

  // Session

  getSession(): Promise<SessionResponse> {
    return this.request('GET', '/api/session');
  }

  logout(): Promise<void> {
    return this.request('POST', '/auth/logout');
  }

  async listSessions(): Promise<LoginSession[]> {
    const { sessions } = await this.request<{
      sessions: LoginSession[] | null;
    }>('GET', '/api/sessions');
    return sessions ?? [];
  }

  deleteSession(id: string): Promise<void> {
    return this.request('DELETE', `/api/sessions/${seg(id)}`);
  }

  revokeOtherSessions(): Promise<{ revoked: number }> {
    return this.request('POST', '/api/sessions/revoke-others');
  }

  async listLoginEvents(): Promise<LoginEvent[]> {
    const { events } = await this.request<{ events: LoginEvent[] | null }>(
      'GET',
      '/api/login-events',
    );
    return events ?? [];
  }

  // API tokens (read-only, for tools such as Claude Code)

  async listTokens(): Promise<ApiToken[]> {
    const { tokens } = await this.request<{ tokens: ApiToken[] | null }>(
      'GET',
      '/api/tokens',
    );
    return tokens ?? [];
  }

  createToken(
    name: string,
    expiresInDays: TokenLifetime,
  ): Promise<CreatedApiToken> {
    return this.request('POST', '/api/tokens', {
      body: { name, expires_in_days: expiresInDays },
    });
  }

  deleteToken(id: string): Promise<void> {
    return this.request('DELETE', `/api/tokens/${seg(id)}`);
  }

  // Members and their roles (the owner only, ARCHITECTURE 10.7)

  async listMembers(): Promise<Member[]> {
    const { members } = await this.request<{ members: Member[] | null }>(
      'GET',
      '/api/members',
    );
    return (members ?? []).map((m) => ({ ...m, roles: m.roles ?? [] }));
  }

  /** Resolves a GitHub login and adds the user without roles. */
  addMember(login: string): Promise<Member> {
    return this.request('POST', '/api/members', { body: { login } });
  }

  removeMember(userId: number): Promise<void> {
    return this.request('DELETE', `/api/members/${userId}`);
  }

  /** Sets the member's role on the app; `null` takes it away. */
  setMemberRole(
    userId: number,
    slug: string,
    role: MemberRole | null,
  ): Promise<Member> {
    const path = `/api/members/${userId}/roles/${seg(slug)}`;
    return role
      ? this.request('PUT', path, { body: { role } })
      : this.request('DELETE', path);
  }

  // System

  getSystem(): Promise<SystemInfo> {
    return this.request('GET', '/api/system');
  }

  async getDockerUsage(): Promise<DockerUsageItem[]> {
    const { items } = await this.request<{ items: DockerUsageItem[] | null }>(
      'GET',
      '/api/system/docker',
    );
    return items ?? [];
  }

  async getGitHubApp(): Promise<GitHubAppStatus> {
    const status = await this.request<GitHubAppStatus>(
      'GET',
      '/api/system/github',
    );
    return {
      ...status,
      installations: status.installations ?? [],
      installation_accounts: status.installation_accounts ?? [],
    };
  }

  async getContainerResources(): Promise<ContainerResources> {
    const resources = await this.request<ContainerResources>(
      'GET',
      '/api/system/containers',
    );
    return { ...resources, groups: resources.groups ?? [] };
  }

  prune(body: { images: boolean; builder: boolean }): Promise<{
    output: string;
    /** What Docker reported it freed; absent from older backends. */
    reclaimed_bytes?: number;
  }> {
    return this.request('POST', '/api/system/prune', { body });
  }

  // Apps

  async listApps(): Promise<App[]> {
    const { apps } = await this.request<{ apps: App[] | null }>(
      'GET',
      '/api/apps',
    );
    return apps ?? [];
  }

  createApp(body: NewApp): Promise<App> {
    return this.request('POST', '/api/apps', { body });
  }

  getApp(slug: string): Promise<App> {
    return this.request('GET', `/api/apps/${seg(slug)}`);
  }

  updateApp(slug: string, body: AppPatch): Promise<App> {
    return this.request('PATCH', `/api/apps/${seg(slug)}`, { body });
  }

  deleteApp(slug: string, options: { volumes: boolean }): Promise<void> {
    return this.request('DELETE', `/api/apps/${seg(slug)}`, {
      query: { volumes: options.volumes ? true : undefined },
    });
  }

  controlApp(slug: string, action: 'start' | 'stop' | 'restart'): Promise<App> {
    return this.request('POST', `/api/apps/${seg(slug)}/${action}`);
  }

  // Review apps (ARCHITECTURE 9.9)

  async listReviewApps(slug: string): Promise<App[]> {
    const { review_apps } = await this.request<{ review_apps: App[] | null }>(
      'GET',
      `/api/apps/${seg(slug)}/review-apps`,
    );
    return review_apps ?? [];
  }

  async listPullRequests(slug: string): Promise<PullRequest[]> {
    const { pull_requests } = await this.request<{
      pull_requests: PullRequest[] | null;
    }>('GET', `/api/apps/${seg(slug)}/pull-requests`);
    return pull_requests ?? [];
  }

  createReviewApp(
    slug: string,
    body: { pr_number: number; copy_data: boolean },
  ): Promise<CreatedReviewApp> {
    return this.request('POST', `/api/apps/${seg(slug)}/review-apps`, {
      body,
    });
  }

  /** Wakes a sleeping app and resolves once it answers (ARCHITECTURE 8.4). */
  wakeApp(slug: string): Promise<App> {
    return this.request('POST', `/api/apps/${seg(slug)}/wake`);
  }

  async listServices(slug: string): Promise<ServiceStatus[]> {
    const { services } = await this.request<{
      services: ServiceStatus[] | null;
    }>('GET', `/api/apps/${seg(slug)}/services`);
    return services ?? [];
  }

  /** Memory use per service and the limit suggestion (ARCHITECTURE 9.5). */
  async getMemoryHistory(
    slug: string,
    range: MemoryRange,
  ): Promise<MemoryHistory> {
    const history = await this.request<MemoryHistory>(
      'GET',
      `/api/apps/${seg(slug)}/memory`,
      { query: { range } },
    );
    return { ...history, services: history.services ?? [] };
  }

  /** A release's override and merged configuration (ARCHITECTURE 9.10). */
  async getComposeView(
    slug: string,
    deploymentId?: number,
  ): Promise<ComposeView> {
    const view = await this.request<ComposeView>(
      'GET',
      `/api/apps/${seg(slug)}/compose`,
      deploymentId ? { query: { deployment: String(deploymentId) } } : {},
    );
    return { ...view, releases: view.releases ?? [] };
  }

  /** The app's containers compared with its current release. */
  async getContainers(slug: string): Promise<ContainerView> {
    const view = await this.request<ContainerView>(
      'GET',
      `/api/apps/${seg(slug)}/containers`,
    );
    return { ...view, containers: view.containers ?? [] };
  }

  // Environment variables

  /** Values come back as `null` without a recent login; they become ''. */
  async getEnv(slug: string): Promise<EnvList> {
    return envList(
      await this.request<EnvListJson>('GET', `/api/apps/${seg(slug)}/env`),
    );
  }

  /** Replaces every variable (the `.env` editor). */
  async putEnv(slug: string, vars: EnvVar[]): Promise<EnvList> {
    return envList(
      await this.request<EnvListJson>('PUT', `/api/apps/${seg(slug)}/env`, {
        body: { vars: vars.map(({ key, value }) => ({ key, value })) },
      }),
    );
  }

  /**
   * Adds or changes one variable; `change.key` renames it. Without
   * `change.value` a renamed variable keeps its value.
   */
  async putEnvVar(
    slug: string,
    key: string,
    change: { key?: string; value?: string },
  ): Promise<EnvList> {
    return envList(
      await this.request<EnvListJson>(
        'PUT',
        `/api/apps/${seg(slug)}/env/${seg(key)}`,
        { body: change },
      ),
    );
  }

  async deleteEnvVar(slug: string, key: string): Promise<EnvList> {
    return envList(
      await this.request<EnvListJson>(
        'DELETE',
        `/api/apps/${seg(slug)}/env/${seg(key)}`,
      ),
    );
  }

  // Container registry credential

  getRegistryCredential(slug: string): Promise<RegistryCredential> {
    return this.request<RegistryCredential>(
      'GET',
      `/api/apps/${seg(slug)}/registry-credential`,
    );
  }

  putRegistryCredential(
    slug: string,
    input: RegistryCredentialInput,
  ): Promise<RegistryCredential> {
    return this.request<RegistryCredential>(
      'PUT',
      `/api/apps/${seg(slug)}/registry-credential`,
      { body: input },
    );
  }

  deleteRegistryCredential(slug: string): Promise<RegistryCredential> {
    return this.request<RegistryCredential>(
      'DELETE',
      `/api/apps/${seg(slug)}/registry-credential`,
    );
  }

  /**
   * What the repository needs at `target` (a branch or a full SHA; the
   * app's default branch when omitted), compared with the saved keys.
   */
  async getEnvRequirements(
    slug: string,
    target: { ref?: string; sha?: string } = {},
  ): Promise<EnvRequirements> {
    const query: Record<string, string> = {};
    if (target.sha) query['sha'] = target.sha;
    else if (target.ref) query['ref'] = target.ref;
    const json = await this.request<EnvRequirements>(
      'GET',
      `/api/apps/${seg(slug)}/env/requirements`,
      { query },
    );
    return {
      ...json,
      variables: json.variables ?? [],
      unlisted: json.unlisted ?? [],
    };
  }

  // Deployments

  async listDeployments(slug: string, limit = 50): Promise<Deployment[]> {
    const { deployments } = await this.request<{
      deployments: Deployment[] | null;
    }>('GET', `/api/apps/${seg(slug)}/deployments`, { query: { limit } });
    return deployments ?? [];
  }

  /** What promoting the paired staging release would do (ARCHITECTURE 9.7). */
  async getPromotion(slug: string): Promise<Promotion> {
    const json = await this.request<Promotion>(
      'GET',
      `/api/apps/${seg(slug)}/promotion`,
    );
    return {
      ...json,
      commits: {
        status: json.commits?.status ?? 'unknown',
        list: json.commits?.list ?? [],
      },
      env: {
        only_in_source: json.env?.only_in_source ?? [],
        only_in_target: json.env?.only_in_target ?? [],
      },
      images: {
        status: json.images?.status ?? 'unavailable',
        services: (json.images?.services ?? []).map((s) => ({
          ...s,
          differing_args: s.differing_args ?? [],
        })),
      },
    };
  }

  createDeployment(
    slug: string,
    body: { ref?: string; sha?: string; promote_from_deployment?: number },
  ): Promise<Deployment> {
    return this.request('POST', `/api/apps/${seg(slug)}/deployments`, { body });
  }

  /**
   * Starts the kept release of a deployment again, without building it
   * (ARCHITECTURE 7.1). The rollback is a deployment of its own.
   */
  rollBack(slug: string, deploymentId: number): Promise<Deployment> {
    return this.request('POST', `/api/apps/${seg(slug)}/deployments`, {
      body: { rollback_to: deploymentId },
    });
  }

  getDeployment(id: number): Promise<Deployment> {
    return this.request('GET', `/api/deployments/${id}`);
  }

  cancelDeployment(id: number): Promise<Deployment> {
    return this.request('POST', `/api/deployments/${id}/cancel`);
  }

  deploymentLogsUrl(id: number): string {
    return `/api/deployments/${id}/logs`;
  }

  // One-off commands and registered tasks (ARCHITECTURE 9.6)

  async listTasks(slug: string): Promise<TaskList> {
    const response = await this.request<Partial<TaskList> | null>(
      'GET',
      `/api/apps/${seg(slug)}/tasks`,
    );
    return taskList(response);
  }

  /** Replaces every registered task. */
  async putTasks(slug: string, tasks: Task[]): Promise<TaskList> {
    const response = await this.request<Partial<TaskList> | null>(
      'PUT',
      `/api/apps/${seg(slug)}/tasks`,
      {
        body: {
          tasks: tasks.map(({ name, service, command, schedule }) => ({
            name,
            service,
            command,
            schedule: schedule ?? '',
          })),
        },
      },
    );
    return taskList(response);
  }

  async listTaskRuns(slug: string, limit = 50): Promise<TaskRun[]> {
    const { runs } = await this.request<{ runs: TaskRun[] | null }>(
      'GET',
      `/api/apps/${seg(slug)}/task-runs`,
      { query: { limit } },
    );
    return runs ?? [];
  }

  /** Runs a registered task (`{task}`) or a command (`{service, command}`). */
  createTaskRun(
    slug: string,
    body: { task: string } | { service: string; command: string },
  ): Promise<TaskRun> {
    return this.request('POST', `/api/apps/${seg(slug)}/task-runs`, { body });
  }

  cancelTaskRun(id: number): Promise<TaskRun> {
    return this.request('POST', `/api/task-runs/${id}/cancel`);
  }

  taskRunLogsUrl(id: number): string {
    return `/api/task-runs/${id}/logs`;
  }

  // GitHub repositories

  async listRepos(): Promise<Repo[]> {
    const { repos } = await this.request<{ repos: Repo[] | null }>(
      'GET',
      '/api/repos',
    );
    return repos ?? [];
  }

  async listBranches(fullName: string): Promise<Branch[]> {
    const { branches } = await this.request<{ branches: Branch[] | null }>(
      'GET',
      `${repoPath(fullName)}/branches`,
    );
    return branches ?? [];
  }

  async listCommits(fullName: string, ref: string): Promise<Commit[]> {
    const { commits } = await this.request<{ commits: Commit[] | null }>(
      'GET',
      `${repoPath(fullName)}/commits`,
      { query: { ref } },
    );
    return commits ?? [];
  }

  detectCompose(
    fullName: string,
    query: { ref?: string; path?: string },
  ): Promise<ComposeDetection> {
    return this.request('GET', `${repoPath(fullName)}/compose`, { query });
  }

  /** Variables to prefill on the create screen; secrets come generated. */
  async getEnvPresets(
    fullName: string,
    query: { ref?: string; path?: string },
  ): Promise<EnvPresets> {
    const presets = await this.request<EnvPresets>(
      'GET',
      `${repoPath(fullName)}/env-presets`,
      { query },
    );
    return {
      ...presets,
      frameworks: presets.frameworks ?? [],
      presets: presets.presets ?? [],
      skipped: presets.skipped ?? [],
    };
  }

  // Custom domains

  listDomains(slug: string): Promise<DomainList> {
    return this.request('GET', `/api/apps/${seg(slug)}/domains`);
  }

  addDomain(slug: string, hostname: string): Promise<Domain> {
    return this.request('POST', `/api/apps/${seg(slug)}/domains`, {
      body: { hostname },
    });
  }

  verifyDomain(slug: string, id: number): Promise<Domain> {
    return this.request('POST', `/api/apps/${seg(slug)}/domains/${id}/verify`);
  }

  updateDomain(
    slug: string,
    id: number,
    body: { is_primary?: boolean; force_active?: boolean },
  ): Promise<Domain> {
    return this.request('PATCH', `/api/apps/${seg(slug)}/domains/${id}`, {
      body,
    });
  }

  deleteDomain(slug: string, id: number): Promise<void> {
    return this.request('DELETE', `/api/apps/${seg(slug)}/domains/${id}`);
  }

  // Runtime logs

  liveLogsUrl(
    slug: string,
    query: { service?: string; tail?: number },
  ): string {
    return buildUrl(`/api/apps/${seg(slug)}/logs`, query);
  }

  searchLogs(
    slug: string,
    query: {
      service?: string;
      from?: string;
      to?: string;
      q?: string;
      limit?: number;
    },
  ): Promise<LogHistory> {
    return this.request('GET', `/api/apps/${seg(slug)}/logs/history`, {
      query,
    });
  }

  async listLogArchives(slug: string, service?: string): Promise<LogArchive[]> {
    const { archives } = await this.request<{ archives: LogArchive[] | null }>(
      'GET',
      `/api/apps/${seg(slug)}/logs/archives`,
      { query: { service } },
    );
    return archives ?? [];
  }

  async logArchiveUrl(slug: string, key: string): Promise<string> {
    const { url } = await this.request<{ url: string }>(
      'GET',
      `/api/apps/${seg(slug)}/logs/archives/url`,
      { query: { key } },
    );
    return url;
  }

  // Backups

  getBackups(): Promise<BackupOverview> {
    return this.request('GET', '/api/backups');
  }

  /** How the backups of one app are doing, without other apps' errors. */
  getAppBackupStatus(slug: string): Promise<AppBackupStatus> {
    return this.request('GET', `/api/apps/${seg(slug)}/backups/status`);
  }

  runBackup(): Promise<BackupRun> {
    return this.request('POST', '/api/backups');
  }

  async listSnapshots(slug: string): Promise<Snapshot[]> {
    const { snapshots } = await this.request<{ snapshots: Snapshot[] | null }>(
      'GET',
      `/api/apps/${seg(slug)}/backups`,
    );
    return snapshots ?? [];
  }

  /** The app's named volumes, each with whether it is left out of backups. */
  async listVolumes(slug: string): Promise<AppVolume[]> {
    const { volumes } = await this.request<{ volumes: AppVolume[] | null }>(
      'GET',
      `/api/apps/${seg(slug)}/volumes`,
    );
    return volumes ?? [];
  }

  restoreSnapshot(
    slug: string,
    snapshotId: string,
  ): Promise<{ restored: boolean }> {
    return this.request('POST', `/api/apps/${seg(slug)}/backups/restore`, {
      body: { snapshot_id: snapshotId },
    });
  }

  snapshotDownloadUrl(slug: string, snapshotId: string): string {
    return `/api/apps/${seg(slug)}/backups/${seg(snapshotId)}/download`;
  }

  // Activity

  /** What was done to the app, newest first (ARCHITECTURE 10.7). */
  async listActivity(
    slug: string,
    options: { limit?: number; before?: number | null } = {},
  ): Promise<ActivityPage> {
    const page = await this.request<Partial<ActivityPage>>(
      'GET',
      `/api/apps/${seg(slug)}/activity`,
      { query: { limit: options.limit, before: options.before } },
    );
    return {
      entries: page.entries ?? [],
      next_before: page.next_before ?? null,
    };
  }

  // Database copies

  async listDbCopies(slug: string): Promise<DbCopy[]> {
    const { copies } = await this.request<{ copies: DbCopy[] | null }>(
      'GET',
      `/api/apps/${seg(slug)}/db-copies`,
    );
    return copies ?? [];
  }

  /** Starts copying the source's database into the staging app `slug`. */
  startDbCopy(
    slug: string,
    body: { source: string; source_kind: CopySourceKind; confirm: string },
  ): Promise<DbCopy> {
    return this.request('POST', `/api/apps/${seg(slug)}/db-copies`, { body });
  }

  getDbCopy(id: number): Promise<DbCopy> {
    return this.request('GET', `/api/db-copies/${id}`);
  }

  dbCopyLogsUrl(id: number): string {
    return `/api/db-copies/${id}/logs`;
  }
}

declare module '@ember/service' {
  interface Registry {
    api: ApiService;
  }
}

interface EnvListJson {
  vars:
    { key: string; value: string | null; pending?: EnvPending | null }[] | null;
  removed?: string[] | null;
  compared?: boolean;
  values_hidden?: boolean;
  values_forbidden?: boolean;
}

function envList(json: EnvListJson): EnvList {
  return {
    vars: (json.vars ?? []).map((v) => ({
      key: v.key,
      value: v.value ?? '',
      pending: v.pending ?? null,
    })),
    removed: json.removed ?? [],
    compared: json.compared === true,
    valuesHidden: json.values_hidden === true,
    valuesForbidden: json.values_forbidden === true,
  };
}
