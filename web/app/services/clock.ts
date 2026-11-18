import Service, { service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import type Owner from '@ember/owner';
import type PollerService from './poller';

const TICK = 30 * 1000;

/**
 * The current time as tracked state, advanced twice a minute, so relative
 * times ("3 分前") keep moving on a page that stays open.
 */
export default class ClockService extends Service {
  @service declare poller: PollerService;

  @tracked now = Date.now();

  constructor(owner: Owner) {
    super(owner);
    this.poller.every(this, TICK, () => {
      this.now = Date.now();
    });
  }
}

declare module '@ember/service' {
  interface Registry {
    clock: ClockService;
  }
}
