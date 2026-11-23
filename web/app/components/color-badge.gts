import type { TOC } from '@ember/component/template-only';
import type { WebColor } from 'koroku-console/utils/types';

/**
 * The colour of a copy of a blue/green app's public service (ARCHITECTURE
 * 7.2), as a quiet label. The colours are names, not states: the badge
 * stays grey so they are not read as 稼働中 / デプロイ中.
 */
const ColorBadge: TOC<{
  Element: HTMLSpanElement;
  Args: { color: WebColor };
}> = <template>
  <span
    class="badge badge-muted badge-web-color"
    data-color={{@color}}
    ...attributes
  >{{@color}}</span>
</template>;

export default ColorBadge;
