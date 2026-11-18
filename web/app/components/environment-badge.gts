import type { TOC } from '@ember/component/template-only';
import { ENVIRONMENT_LABELS } from 'koroku-console/utils/status';
import type { Environment } from 'koroku-console/utils/types';

function label(environment: Environment): string {
  return ENVIRONMENT_LABELS[environment] ?? environment;
}

/**
 * Production / staging label of an app. Production is a solid, high-contrast
 * badge wherever it appears, so the two are never mixed up; staging is a
 * quiet outline.
 */
const EnvironmentBadge: TOC<{
  Element: HTMLSpanElement;
  Args: { environment: Environment };
}> = <template>
  <span class="badge badge-env-{{@environment}}" ...attributes>
    {{label @environment}}
  </span>
</template>;

export default EnvironmentBadge;
