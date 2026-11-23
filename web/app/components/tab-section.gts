import type { TOC } from '@ember/component/template-only';
import { eq } from '@ember/helper';
import { uniqueId } from '@ember/helper';

/**
 * One titled part of an app detail tab: a pane (styles/panes.css) with its
 * heading wired up as the section's name. The `actions` block sits at the
 * end of the title line: the section's own buttons, or the link to the tab
 * that holds the full picture. A `<p class="pane-description">` may open the
 * body. `@level={{3}}` makes the heading an `<h3>`, for a part inside a
 * section of its own.
 */
const TabSection: TOC<{
  Element: HTMLElement;
  Args: { title: string; level?: 2 | 3 };
  Blocks: { default: []; actions: [] };
}> = <template>
  {{#let (uniqueId) as |id|}}
    <section class="pane" aria-labelledby={{id}} ...attributes>
      <header class="pane-header">
        {{#if (eq @level 3)}}
          <h3 class="pane-title" id={{id}}>{{@title}}</h3>
        {{else}}
          <h2 class="pane-title" id={{id}}>{{@title}}</h2>
        {{/if}}
        {{#if (has-block "actions")}}
          <div class="tab-section-actions">{{yield to="actions"}}</div>
        {{/if}}
      </header>
      {{yield}}
    </section>
  {{/let}}
</template>;

export default TabSection;
