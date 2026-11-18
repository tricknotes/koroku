import type { TOC } from '@ember/component/template-only';
import { uniqueId } from '@ember/helper';

/**
 * One titled section of an app detail tab. The `actions` block sits at the
 * end of the title line: the section's own buttons, or the link to the tab
 * that holds the full picture.
 */
const TabSection: TOC<{
  Element: HTMLElement;
  Args: { title: string };
  Blocks: { default: []; actions: [] };
}> = <template>
  {{#let (uniqueId) as |id|}}
    <section class="tab-section" aria-labelledby={{id}} ...attributes>
      <header class="tab-section-header">
        <h2 class="tab-section-title" id={{id}}>{{@title}}</h2>
        {{#if (has-block "actions")}}
          <div class="tab-section-actions">{{yield to="actions"}}</div>
        {{/if}}
      </header>
      {{yield}}
    </section>
  {{/let}}
</template>;

export default TabSection;
