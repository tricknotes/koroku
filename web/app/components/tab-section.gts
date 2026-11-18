import type { TOC } from '@ember/component/template-only';
import { uniqueId } from '@ember/helper';

/**
 * One titled part of an app detail tab: a pane (styles/panes.css) with its
 * heading wired up as the section's name. The `actions` block sits at the
 * end of the title line: the section's own buttons, or the link to the tab
 * that holds the full picture. A `<p class="pane-description">` may open the
 * body.
 */
const TabSection: TOC<{
  Element: HTMLElement;
  Args: { title: string };
  Blocks: { default: []; actions: [] };
}> = <template>
  {{#let (uniqueId) as |id|}}
    <section class="pane" aria-labelledby={{id}} ...attributes>
      <header class="pane-header">
        <h2 class="pane-title" id={{id}}>{{@title}}</h2>
        {{#if (has-block "actions")}}
          <div class="tab-section-actions">{{yield to="actions"}}</div>
        {{/if}}
      </header>
      {{yield}}
    </section>
  {{/let}}
</template>;

export default TabSection;
