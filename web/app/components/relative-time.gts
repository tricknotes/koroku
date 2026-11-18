import Component from '@glimmer/component';
import { service } from '@ember/service';
import type ClockService from 'paas-console/services/clock';
import { formatDateTimeJst, relativeTime } from 'paas-console/utils/format';

interface RelativeTimeSignature {
  Element: HTMLTimeElement;
  Args: { time: string | null | undefined };
}

/**
 * `<time>` showing "3 分前", with the absolute time (JST) on hover. It
 * follows the clock, so it stays right on a page that is left open. Renders
 * nothing without a time.
 */
export default class RelativeTime extends Component<RelativeTimeSignature> {
  @service declare clock: ClockService;

  get text(): string {
    return relativeTime(this.args.time, this.clock.now);
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
