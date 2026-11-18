import { registerDestructor } from '@ember/destroyable';
import type Owner from '@ember/owner';
import ApiService from 'koroku-console/services/api';
import BrowserService from 'koroku-console/services/browser';
import EventSourceService, {
  type Stream,
  type StreamHandlers,
} from 'koroku-console/services/event-source';
import PollerService from 'koroku-console/services/poller';
import StorageService from 'koroku-console/services/storage';
import { ApiError } from 'koroku-console/utils/errors';
import type {
  App,
  AppVolume,
  BackupOverview,
  Deployment,
  SystemInfo,
} from 'koroku-console/utils/types';

export interface FakeCall {
  method: string;
  path: string;
  query: Record<string, string>;
  body: unknown;
}

type Handler = (call: FakeCall) => unknown;

/** A request the fake API has received and not answered yet. */
export interface HeldRequest {
  release(response?: unknown): void;
  refuse(status: number, code: string, details?: Record<string, unknown>): void;
}

/**
 * Stands in for the `api` service. Only the transport is replaced, so the
 * real service still builds every path, query and body. Endpoints without a
 * handler answer like a backend that does not have them: `404 not_found`.
 */
export class FakeApi extends ApiService {
  calls: FakeCall[] = [];
  private handlers = new Map<string, Handler>();
  /** Ends the requests that are still held, one function per `hold()`. */
  private abandon: (() => void)[] = [];

  /** Registers a response (or a function producing one) for an endpoint. */
  on(method: string, path: string, response: unknown): this {
    const handler: Handler =
      typeof response === 'function' ? (response as Handler) : () => response;
    this.handlers.set(`${method} ${path}`, handler);
    return this;
  }

  /** Makes an endpoint fail with the API's error shape. */
  fail(
    method: string,
    path: string,
    status: number,
    code: string,
    details: Record<string, unknown> = {},
  ): this {
    return this.on(method, path, () => {
      throw apiError(status, code, details);
    });
  }

  /**
   * Keeps the answer of an endpoint back, so a test can look at the screen
   * while the request runs. The request stays open until `release` (the
   * response) or `refuse` (an API error) is called. A test that stops
   * before either (a failed step) does not leave it hanging: what is still
   * held when the test ends is refused, so the next test starts settled.
   */
  hold(method: string, path: string): HeldRequest {
    let settle: (answer: () => unknown) => void = () => {};
    const answered = new Promise<() => unknown>((resolve) => {
      settle = resolve;
    });
    const refuse = (
      status: number,
      code: string,
      details: Record<string, unknown> = {},
    ) =>
      settle(() => {
        throw apiError(status, code, details);
      });
    this.on(method, path, () => answered.then((answer) => answer()));
    // Settling twice does nothing, so this is harmless after a release.
    this.abandon.push(() => refuse(0, 'network_error'));
    return {
      release: (response?: unknown) => settle(() => response),
      refuse,
    };
  }

  willDestroy(): void {
    super.willDestroy();
    for (const end of this.abandon) end();
  }

  /**
   * Makes every read that has a handler fail (the backend is in trouble),
   * except the given paths.
   */
  failReads(status: number, code: string, except: string[] = []): this {
    for (const key of [...this.handlers.keys()]) {
      const [method = '', path = ''] = key.split(' ');
      if (method === 'GET' && !except.includes(path)) {
        this.fail(method, path, status, code);
      }
    }
    return this;
  }

  callsTo(method: string, path: string): FakeCall[] {
    return this.calls.filter((c) => c.method === method && c.path === path);
  }

  override transport(
    method: string,
    url: string,
    body?: unknown,
  ): Promise<unknown> {
    const [path = '', search = ''] = url.split('?');
    const call: FakeCall = {
      method,
      path,
      query: Object.fromEntries(new URLSearchParams(search)),
      body,
    };
    this.calls.push(call);

    const handler = this.handlers.get(`${method} ${path}`);
    if (!handler) {
      return Promise.reject(apiError(404, 'not_found'));
    }
    // A copy, as a real response would be: the app never shares objects
    // with the fixture.
    const copy = (response: unknown) =>
      response === undefined ? undefined : structuredClone(response);
    try {
      const response = handler(call);
      // A handler may answer later (a promise), to hold a request open.
      return response instanceof Promise
        ? response.then(copy)
        : Promise.resolve(copy(response));
    } catch (error) {
      return Promise.reject(
        error instanceof Error ? error : new Error(String(error)),
      );
    }
  }
}

export function apiError(
  status: number,
  code: string,
  details: Record<string, unknown> = {},
): ApiError {
  return new ApiError(status, code, `${code} (from the fake API)`, details);
}

export class FakeStream implements Stream {
  closed = false;

  constructor(
    readonly url: string,
    private readonly handlers: StreamHandlers,
  ) {}

  close(): void {
    this.closed = true;
  }

  /** One unnamed SSE message. */
  emit(data: string): void {
    this.handlers.message(data);
  }

  /** The server's `event: end`. */
  end(data: string): void {
    this.handlers.end?.(data);
  }

  /** The connection was (re)established. */
  open(): void {
    this.handlers.open?.();
  }

  error(closed: boolean): void {
    this.handlers.error?.(closed);
  }
}

export class FakeEventSource extends EventSourceService {
  streams: FakeStream[] = [];

  override open(url: string, handlers: StreamHandlers): FakeStream {
    const stream = new FakeStream(url, handlers);
    this.streams.push(stream);
    return stream;
  }

  get last(): FakeStream {
    const stream = this.streams[this.streams.length - 1];
    if (!stream) throw new Error('no stream was opened');
    return stream;
  }
}

/** Polls only when a test calls `tick()`. */
export class FakePoller extends PollerService {
  private callbacks = new Set<() => unknown>();

  override every(
    owner: object,
    _interval: number,
    callback: () => unknown,
  ): void {
    this.callbacks.add(callback);
    registerDestructor(owner, () => this.callbacks.delete(callback));
  }

  async tick(): Promise<void> {
    for (const callback of [...this.callbacks]) {
      await callback();
    }
  }
}

export class FakeBrowser extends BrowserService {
  assigned: string[] = [];
  reloads = 0;
  query = '';
  copied: string[] = [];
  /** Makes `copy` fail, as when the browser refuses clipboard access. */
  clipboardBlocked = false;

  override get search(): string {
    return this.query;
  }

  override assign(url: string): void {
    this.assigned.push(url);
  }

  override reload(): void {
    this.reloads += 1;
  }

  override copy(text: string): Promise<void> {
    if (this.clipboardBlocked) {
      return Promise.reject(new Error('clipboard blocked'));
    }
    this.copied.push(text);
    return Promise.resolve();
  }
}

/** In-memory stand-in for `localStorage`. */
export class FakeStorage extends StorageService {
  items = new Map<string, string>();

  override get(key: string): string | null {
    return this.items.get(key) ?? null;
  }

  override set(key: string, value: string): void {
    this.items.set(key, value);
  }

  override remove(key: string): void {
    this.items.delete(key);
  }
}

export interface Fakes {
  api: FakeApi;
  eventSource: FakeEventSource;
  poller: FakePoller;
  browser: FakeBrowser;
  storage: FakeStorage;
}

/**
 * Replaces every service that reaches outside the app and logs in as
 * `octo`. Call from `beforeEach`, before the first `visit`.
 */
export function installFakes(owner: Owner): Fakes {
  owner.register('service:api', FakeApi);
  owner.register('service:event-source', FakeEventSource);
  owner.register('service:poller', FakePoller);
  owner.register('service:browser', FakeBrowser);
  owner.register('service:storage', FakeStorage);

  const fakes: Fakes = {
    api: owner.lookup('service:api') as FakeApi,
    eventSource: owner.lookup('service:event-source') as FakeEventSource,
    poller: owner.lookup('service:poller') as FakePoller,
    browser: owner.lookup('service:browser') as FakeBrowser,
    storage: owner.lookup('service:storage') as FakeStorage,
  };
  fakes.api.on('GET', '/api/session', {
    authenticated: true,
    user: { id: 1, login: 'octo', avatar_url: '' },
  });
  return fakes;
}

export const SHA_A = '9fceb02d0ae598e95dc970b74767f19372d61af8';
export const SHA_B = '1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4';

export function makeDeployment(
  overrides: Partial<Deployment> = {},
): Deployment {
  return {
    id: 42,
    app_slug: 'blog',
    ref: 'main',
    commit_sha: SHA_A,
    commit_message: 'Fix login',
    status: 'succeeded',
    error: '',
    queued_at: '2026-11-18T03:00:00Z',
    started_at: '2026-11-18T03:00:01Z',
    finished_at: '2026-11-18T03:01:10Z',
    ...overrides,
  };
}

export function makeApp(overrides: Partial<App> = {}): App {
  const slug = overrides.slug ?? 'blog';
  return {
    slug,
    repo_full_name: 'octo/blog',
    repo_url: '',
    default_branch: 'main',
    compose_path: '',
    web_service: 'web',
    web_port: 3000,
    host_port: 20000,
    healthcheck_path: '/',
    desired_state: 'running',
    environment: 'staging',
    relax_policy: false,
    memory_limit: '',
    service_memory_limits: {},
    url: `https://${slug}.dev.example.com`,
    primary_url: `https://${slug}.dev.example.com`,
    pending_domains: 0,
    current_deployment: null,
    latest_deployment: null,
    created_at: '2026-11-18T03:00:00Z',
    updated_at: '2026-11-18T03:00:00Z',
    ...overrides,
  };
}

export function makeSystem(overrides: Partial<SystemInfo> = {}): SystemInfo {
  return {
    version: 'v0.1.0',
    base_domain: 'dev.example.com',
    disk: {
      total_bytes: 40 * 1024 ** 3,
      used_bytes: 8.6 * 1024 ** 3,
      used_percent: 21.5,
    },
    memory: { total_bytes: 4 * 1024 ** 3, available_bytes: 2 * 1024 ** 3 },
    docker_version: '29.8.0',
    disk_limit_percent: 85,
    log_archive: {
      configured: true,
      last_success_at: '2026-11-18T03:10:05Z',
      last_error: '',
    },
    caddy: {
      enabled: true,
      last_loaded_at: '2026-11-18T03:00:02Z',
      last_error: '',
    },
    ...overrides,
  };
}

/** One entry of `GET /api/apps/{slug}/volumes`: backed up and existing. */
export function makeVolume(
  name: string,
  overrides: Partial<AppVolume> = {},
): AppVolume {
  return { name, excluded: false, exists: true, ...overrides };
}

export function makeBackups(
  overrides: Partial<BackupOverview> = {},
): BackupOverview {
  return {
    configured: true,
    running: false,
    last_success_at: '2026-11-17T18:00:40Z',
    warning: false,
    next_run_at: '2026-11-18T18:00:00Z',
    runs: [],
    ...overrides,
  };
}
