import type { TOC } from '@ember/component/template-only';
import type { Tone } from 'paas-console/utils/status';

/**
 * Small label for a category or a count. The text carries the meaning; the
 * tone only supports it. For the state of an app, deployment or domain use
 * `<StatusIndicator>`, and `<EnvironmentBadge>` for production / staging.
 */
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
