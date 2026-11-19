import Service, { service } from '@ember/service';
import { waitForPromise } from '@ember/test-waiters';
import { ApiError } from 'koroku-console/utils/errors';
import type SessionService from './session';
import type {
  App,
  AppPatch,
  AppVolume,
  BackupOverview,
  BackupRun,
  Branch,
  Commit,
  ComposeDetection,
  ContainerResources,
  Deployment,
  DockerUsageItem,
  Domain,
  DomainList,
  EnvList,
  EnvRequirements,
  EnvPending,
  EnvVar,
  LogArchive,
  LoginEvent,
  LoginSession,
  LogHistory,
  NewApp,
  Repo,
  ServiceStatus,
  SessionResponse,
  Snapshot,
  SystemInfo,
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

  async listServices(slug: string): Promise<ServiceStatus[]> {
    const { services } = await this.request<{
      services: ServiceStatus[] | null;
    }>('GET', `/api/apps/${seg(slug)}/services`);
    return services ?? [];
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

  createDeployment(
    slug: string,
    body: { ref?: string; sha?: string },
  ): Promise<Deployment> {
    return this.request('POST', `/api/apps/${seg(slug)}/deployments`, { body });
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
  };
}
