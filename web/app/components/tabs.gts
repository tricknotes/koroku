import Component from '@glimmer/component';
import type { TOC } from '@ember/component/template-only';
import { hash } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type RouterService from '@ember/routing/router-service';
import { service } from '@ember/service';

function models(model: string | number | undefined): (string | number)[] {
  return model === undefined ? [] : [model];
}

interface TabSignature {
  Element: HTMLAnchorElement;
  Args: { route: string; model?: string | number };
  Blocks: { default: [] };
}

/** One tab: a link to a route, so the URL restores the selected tab. */
class Tab extends Component<TabSignature> {
  @service declare router: RouterService;

  /** `<LinkTo>` adds the `active` class; assistive technology needs this. */
  get current(): 'page' | undefined {
    const { route, model } = this.args;
    return this.router.isActive(route, ...models(model)) ? 'page' : undefined;
  }

  <template>
    <LinkTo
      @route={{@route}}
      @models={{models @model}}
      class="tab"
      aria-current={{this.current}}
      ...attributes
    >{{yield}}</LinkTo>
  </template>
}

/**
 * Horizontal tabs for the sections of one record. They are plain navigation
 * links (the active one is marked with `aria-current="page"`), not an ARIA
 * tablist: each tab has its own URL.
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
