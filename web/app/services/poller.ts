import Service from '@ember/service';
import { registerDestructor } from '@ember/destroyable';

/**
 * Repeats a callback for as long as its owner lives. A service so tests can
 * register a fake and tick by hand instead of waiting on real timers.
 */
export default class PollerService extends Service {
  every(owner: object, interval: number, callback: () => unknown): void {
    let stopped = false;
    let timer: ReturnType<typeof setTimeout> | undefined;

    const tick = async () => {
      if (stopped) return;
      try {
        await callback();
      } catch {
        // A failed refresh is retried on the next tick.
      }
      if (!stopped) timer = setTimeout(() => void tick(), interval);
    };

    timer = setTimeout(() => void tick(), interval);
    registerDestructor(owner, () => {
      stopped = true;
      clearTimeout(timer);
    });
  }
}

declare module '@ember/service' {
  interface Registry {
    poller: PollerService;
  }
}
