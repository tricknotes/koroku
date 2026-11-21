import type { TOC } from '@ember/component/template-only';
import Badge from 'koroku-console/components/badge';
import { roleLabel } from 'koroku-console/utils/roles';
import type { AppRole } from 'koroku-console/utils/types';

function shown(role: AppRole | undefined): boolean {
  return role !== undefined && role !== 'owner';
}

/**
 * The member's role on an app (ARCHITECTURE 10.7). The owner may do
 * everything, so nothing is shown for it.
 */
const RoleBadge: TOC<{
  Element: HTMLSpanElement;
  Args: { role: AppRole | undefined };
}> = <template>
  {{#if (shown @role)}}
    <Badge
      title="このアプリでのあなたのロール"
      data-test-role-badge={{@role}}
      ...attributes
    >ロール: {{roleLabel @role}}</Badge>
  {{/if}}
</template>;

export default RoleBadge;
