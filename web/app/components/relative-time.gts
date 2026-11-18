import type { TOC } from '@ember/component/template-only';
import { formatDateTime, relativeTime } from 'paas-console/utils/format';

/** `<time>` showing "3 分前" with the absolute time as its tooltip. */
const RelativeTime: TOC<{
  Args: { time: string | null | undefined };
}> = <template>
  {{#if @time}}
    <time datetime={{@time}} title={{formatDateTime @time}}>
      {{relativeTime @time}}
    </time>
  {{/if}}
</template>;

export default RelativeTime;
