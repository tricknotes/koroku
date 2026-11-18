import { registerDestructor } from '@ember/destroyable';
import type Owner from '@ember/owner';
import ApiService from 'paas-console/services/api';
import BrowserService from 'paas-console/services/browser';
import EventSourceService, {
  type Stream,
  type StreamHandlers,
} from 'paas-console/services/event-source';
import PollerService from 'paas-console/services/poller';
import { ApiError } from 'paas-console/utils/errors';
import type { App, Deployment } from 'paas-console/utils/types';

export interface FakeCall {
  method: string;
  path: string;
  query: Record<string, string>;
  body: unknown;
}

type Handler = (call: FakeCall) => unknown;

/**
 * Stands in for the `api` service. Only the transport is replaced, so the
 * real service still builds every path, query and body. Endpoints without a
 * handler answer like a backend that does not have them: `404 not_found`.
 */
export class FakeApi extends ApiService {
  calls: FakeCall[] = [];
  private handlers = new Map<string, Handler>();

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
    try {
      const response = handler(call);
      // A copy, as a real response would be: the app never shares objects
      // with the fixture.
      return Promise.resolve(
        response === undefined ? undefined : structuredClone(response),
      );
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

  override get search(): string {
    return this.query;
  }

  override assign(url: string): void {
    this.assigned.push(url);
  }

  override reload(): void {
    this.reloads += 1;
  }
}

export interface Fakes {
  api: FakeApi;
  eventSource: FakeEventSource;
  poller: FakePoller;
  browser: FakeBrowser;
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

  const fakes: Fakes = {
    api: owner.lookup('service:api') as FakeApi,
    eventSource: owner.lookup('service:event-source') as FakeEventSource,
    poller: owner.lookup('service:poller') as FakePoller,
    browser: owner.lookup('service:browser') as FakeBrowser,
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
    current_deployment: null,
    latest_deployment: null,
    created_at: '2026-11-18T03:00:00Z',
    updated_at: '2026-11-18T03:00:00Z',
    ...overrides,
  };
}
