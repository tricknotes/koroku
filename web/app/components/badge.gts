import type { TOC } from '@ember/component/template-only';
import type { Tone } from 'paas-console/utils/status';

/** Small coloured label for a state. The text carries the meaning. */
const Badge: TOC<{
  Element: HTMLSpanElement;
  Args: { tone?: Tone };
  Blocks: { default: [] };
}> = <template>
  <span class="badge badge-{{if @tone @tone 'muted'}}" ...attributes>
    {{yield}}
  </span>
</template>;

export default Badge;
