import type { TOC } from '@ember/component/template-only';
import stickToBottom from 'paas-console/modifiers/stick-to-bottom';

/**
 * Scrolling block of log text. The whole log is one text node, which keeps
 * thousands of lines cheap to update.
 */
const LogView: TOC<{
  Element: HTMLPreElement;
  Args: { text: string; label: string; empty?: string };
}> = <template>
  {{! A focusable region so the log can be scrolled with the keyboard.
      Not role="log": a busy stream would be read out line by line. }}
  <pre
    class="log"
    tabindex="0"
    role="region"
    aria-label={{@label}}
    {{stickToBottom @text}}
    ...attributes
  >{{#if @text}}{{@text}}{{else}}<span class="muted">{{if
          @empty
          @empty
          "ログはまだありません。"
        }}</span>{{/if}}</pre>
</template>;

export default LogView;
