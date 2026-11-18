import type { TOC } from '@ember/component/template-only';
import Icon, { type IconName } from './icon';

export type BannerTone = 'info' | 'warning' | 'error';

const ICONS: Record<BannerTone, IconName> = {
  info: 'info',
  warning: 'warning',
  error: 'error',
};

function icon(tone: BannerTone | undefined): IconName {
  return ICONS[tone ?? 'info'];
}

/** Errors interrupt a screen reader; the other tones are announced politely. */
function role(tone: BannerTone | undefined): 'alert' | 'status' {
  return tone === 'error' ? 'alert' : 'status';
}

/**
 * A message across the content area: something to know (`info`), something
 * that needs attention (`warning`) or something that went wrong (`error`).
 * The `actions` block holds what the reader can do about it, e.g. a retry.
 */
const Banner: TOC<{
  Element: HTMLDivElement;
  Args: { tone?: BannerTone; title?: string };
  Blocks: { default: []; actions: [] };
}> = <template>
  <div
    class="banner banner-{{if @tone @tone 'info'}}"
    role={{role @tone}}
    ...attributes
  >
    <Icon @name={{icon @tone}} />
    <div class="banner-body">
      {{#if @title}}
        <p class="banner-title">{{@title}}</p>
      {{/if}}
      {{yield}}
    </div>
    {{#if (has-block "actions")}}
      <div class="banner-actions">{{yield to="actions"}}</div>
    {{/if}}
  </div>
</template>;

export default Banner;
