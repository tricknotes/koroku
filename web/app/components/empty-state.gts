import type { TOC } from '@ember/component/template-only';

/**
 * What to show where a list has nothing yet: what is missing, why it
 * matters, and exactly one next step in the `action` block (one button or
 * link, not a menu of choices).
 */
const EmptyState: TOC<{
  Element: HTMLDivElement;
  Args: { title: string; description?: string };
  Blocks: { action: [] };
}> = <template>
  <div class="empty-state" ...attributes>
    <p class="empty-state-title">{{@title}}</p>
    {{#if @description}}
      <p class="empty-state-description">{{@description}}</p>
    {{/if}}
    <div class="empty-state-action">{{yield to="action"}}</div>
  </div>
</template>;

export default EmptyState;
