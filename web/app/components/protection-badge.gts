import type { TOC } from '@ember/component/template-only';
import Icon from './icon';

/**
 * Marks an app that only the logged-in console user can open (access
 * protection, ARCHITECTURE 8.3). Quiet like the staging badge; the lock and
 * the word carry the meaning.
 */
const ProtectionBadge: TOC<{ Element: HTMLSpanElement }> = <template>
  <span
    class="badge badge-protected"
    title="管理画面にログインしている人だけが開けます"
    ...attributes
  >
    <Icon @name="lock" />
    保護
  </span>
</template>;

export default ProtectionBadge;
