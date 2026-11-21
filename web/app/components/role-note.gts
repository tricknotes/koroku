import type { TOC } from '@ember/component/template-only';
import { roleLabel } from 'koroku-console/utils/roles';
import type { AppRole } from 'koroku-console/utils/types';

function sentence(needed: AppRole, what: string | undefined): string {
  const subject = what ?? 'この操作';
  if (needed === 'owner') return `${subject}はオーナーだけが変えられます。`;
  return `${subject}には「${roleLabel(needed)}」のロールが必要です。オーナーにロールの変更を頼んでください。`;
}

/**
 * Says why a part of the screen is read-only for a member: the role it
 * needs (ARCHITECTURE 10.7). The controls themselves are hidden or
 * disabled by the caller.
 */
const RoleNote: TOC<{
  Element: HTMLParagraphElement;
  Args: { needed: AppRole; what?: string };
}> = <template>
  <p class="role-note" data-test-role-note={{@needed}} ...attributes>
    {{sentence @needed @what}}
  </p>
</template>;

export default RoleNote;
