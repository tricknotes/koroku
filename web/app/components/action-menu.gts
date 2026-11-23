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
    /**
     * The items: `<button class="menu-item">` with their own click
     * handler. The menu closes once the click has reached the item.
     */
    default: [];
  };
}

/**
 * A "…" button that discloses a short list of actions. Like the user menu,
 * it closes on Escape (focus returns to the button), a press outside, or
 * once an item has run.
 *
 * The panel closes the menu from its own click listener, which the click
 * reaches after the item's handler. Closing from a second listener on the
 * item would not do: after each listener of a real click the browser runs
 * the microtasks, the render removes the panel, and the item's later
 * listeners are dropped with it.
 */
export default class ActionMenu extends Component<ActionMenuSignature> {
  @tracked open = false;

  panelId = `menu-${guidFor(this)}`;
  trigger: HTMLButtonElement | null = null;

  toggle = (event: Event) => {
    this.trigger = event.currentTarget as HTMLButtonElement;
    this.open = !this.open;
  };

  closeAfterItem = (event: MouseEvent) => {
    // A disabled item fires no click, so any click on an item has run it.
    if ((event.target as Element | null)?.closest('.menu-item')) {
      this.open = false;
    }
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
        {{! template-lint-disable no-invalid-interactive }}
        <div
          class="menu-panel"
          id={{this.panelId}}
          data-test-action-menu-panel
          {{on "click" this.closeAfterItem}}
        >
          {{yield}}
        </div>
      {{/if}}
    </div>
  </template>
}
