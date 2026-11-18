import type { TOC } from '@ember/component/template-only';
import { hash } from '@ember/helper';
import { LinkTo } from '@ember/routing';

function models(model: string | number | undefined): (string | number)[] {
  return model === undefined ? [] : [model];
}

/** One tab: a link to a route, so the URL restores the selected tab. */
const Tab: TOC<{
  Element: HTMLAnchorElement;
  Args: { route: string; model?: string | number };
  Blocks: { default: [] };
}> = <template>
  <LinkTo
    @route={{@route}}
    @models={{models @model}}
    class="tab"
    ...attributes
  >{{yield}}</LinkTo>
</template>;

/**
 * Horizontal tabs for the sections of one record. They are plain navigation
 * links (the active one gets `aria-current="page"` from the router), not an
 * ARIA tablist: each tab has its own URL.
 */
const Tabs: TOC<{
  Element: HTMLElement;
  Args: { label: string };
  Blocks: { default: [{ Tab: typeof Tab }] };
}> = <template>
  <nav class="tabs" aria-label={{@label}} ...attributes>
    {{yield (hash Tab=Tab)}}
  </nav>
</template>;

export default Tabs;
