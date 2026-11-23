import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { on } from '@ember/modifier';
import { guidFor } from '@ember/object/internals';
import onDismiss, {
  type DismissReason,
} from 'koroku-console/modifiers/on-dismiss';
import Icon from './icon';

interface ActionMenuSignature {
  Element: HTMLDivElement;
  Args: {
    /** The trigger's accessible name ("その他の操作"). */
    label: string;
  };
  Blocks: {
    /** The items: `<button class="menu-item">`; call `close` after acting. */
    default: [close: () => void];
  };
}

/**
 * A "…" button that discloses a short list of actions. Like the user menu,
 * it closes on Escape (focus returns to the button), a press outside, or
 * once an item has run.
 */
export default class ActionMenu extends Component<ActionMenuSignature> {
  @tracked open = false;

  panelId = `menu-${guidFor(this)}`;
  trigger: HTMLButtonElement | null = null;

  toggle = (event: Event) => {
    this.trigger = event.currentTarget as HTMLButtonElement;
    this.open = !this.open;
  };

  close = () => {
    this.open = false;
  };

  dismiss = (reason: DismissReason) => {
    if (!this.open) return;
    this.open = false;
    if (reason === 'escape') this.trigger?.focus();
  };

  <template>
    <div class="menu action-menu" {{onDismiss this.dismiss}} ...attributes>
      <button
        type="button"
        class="button action-menu-trigger"
        aria-expanded={{if this.open "true" "false"}}
        aria-controls={{this.panelId}}
        aria-label={{@label}}
        title={{@label}}
        data-test-action-menu
        {{on "click" this.toggle}}
      ><Icon @name="more" /></button>

      {{#if this.open}}
        <div class="menu-panel" id={{this.panelId}} data-test-action-menu-panel>
          {{yield this.close}}
        </div>
      {{/if}}
    </div>
  </template>
}
