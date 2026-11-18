import Component from '@glimmer/component';
import { service } from '@ember/service';
import type ClockService from 'paas-console/services/clock';
import { formatDateTimeJst } from 'paas-console/utils/format';
import { timeUntil } from 'paas-console/utils/schedule';

interface TimeUntilSignature {
  Element: HTMLTimeElement;
  Args: { time: string | null | undefined };
}

/**
 * `<time>` for a moment that lies ahead ("3 時間後"), with the absolute time
 * (JST) on hover: the counterpart of `<RelativeTime>` for schedules. It
 * follows the clock. Renders nothing without a time.
 */
export default class TimeUntil extends Component<TimeUntilSignature> {
  @service declare clock: ClockService;

  get text(): string {
    return timeUntil(this.args.time, this.clock.now);
  }

  <template>
    {{#if @time}}
      <time
        datetime={{@time}}
        title={{formatDateTimeJst @time}}
        ...attributes
      >{{this.text}}</time>
    {{/if}}
  </template>
}
