import Service from '@ember/service';

export interface StreamHandlers {
  /** One unnamed SSE message; `data` is the raw `data:` payload. */
  message: (data: string) => void;
  /** The server's `event: end`. The caller is expected to `close()`. */
  end?: (data: string) => void;
  open?: () => void;
  /**
   * The connection failed. `closed` is true when the browser gave up (for
   * example a non-200 response); otherwise it reconnects on its own.
   */
  error?: (closed: boolean) => void;
}

export interface Stream {
  close(): void;
}

/**
 * Thin wrapper around `EventSource` so components do not depend on the
 * global and tests can register a fake that emits events by hand.
 */
export default class EventSourceService extends Service {
  open(url: string, handlers: StreamHandlers): Stream {
    const source = new EventSource(url);
    source.onmessage = (event: MessageEvent<string>) => {
      handlers.message(event.data);
    };
    source.addEventListener('end', (event: MessageEvent<string>) => {
      handlers.end?.(event.data);
    });
    source.onopen = () => handlers.open?.();
    source.onerror = () => {
      handlers.error?.(source.readyState === EventSource.CLOSED);
    };
    return { close: () => source.close() };
  }
}

declare module '@ember/service' {
  interface Registry {
    'event-source': EventSourceService;
  }
}
