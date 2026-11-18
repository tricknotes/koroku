import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { registerDestructor } from '@ember/destroyable';
import type Owner from '@ember/owner';
import type BrowserService from 'koroku-console/services/browser';
import Icon from './icon';

const FEEDBACK = 2000;

interface CopyButtonSignature {
  Element: HTMLButtonElement;
  Args: {
    /** What goes on the clipboard. */
    text: string;
    /** Names the thing being copied, e.g. "CNAME の値". */
    label?: string;
  };
}

/**
 * Icon button that copies a value (a domain, a DNS record, a SHA). The
 * result is shown on the button for a moment and announced to screen
 * readers.
 */
export default class CopyButton extends Component<CopyButtonSignature> {
  @service declare browser: BrowserService;

  @tracked state: 'idle' | 'copied' | 'failed' = 'idle';

  private timer: ReturnType<typeof setTimeout> | undefined;

  constructor(owner: Owner, args: CopyButtonSignature['Args']) {
    super(owner, args);
    registerDestructor(this, () => clearTimeout(this.timer));
  }

  get label(): string {
    const what = this.args.label;
    return what ? `${what}をコピー` : 'コピー';
  }

  get announcement(): string {
    if (this.state === 'copied') return 'コピーしました';
    if (this.state === 'failed') return 'コピーできませんでした';
    return '';
  }

  copy = async () => {
    try {
      await this.browser.copy(this.args.text);
      this.state = 'copied';
    } catch {
      this.state = 'failed';
    }
    clearTimeout(this.timer);
    this.timer = setTimeout(() => {
      this.state = 'idle';
    }, FEEDBACK);
  };

  <template>
    <button
      type="button"
      class="button button-ghost button-icon copy-button
        {{if (isCopied this.state) 'is-copied'}}"
      aria-label={{this.label}}
      title={{this.label}}
      data-test-copy
      {{on "click" this.copy}}
      ...attributes
    >
      <Icon @name={{if (isCopied this.state) "check" "copy"}} />
    </button>
    <span class="visually-hidden" role="status">{{this.announcement}}</span>
  </template>
}

function isCopied(state: string): boolean {
  return state === 'copied';
}
