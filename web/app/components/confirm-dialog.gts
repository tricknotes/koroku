import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { on } from '@ember/modifier';
import { uniqueId } from '@ember/helper';
import showModal from 'koroku-console/modifiers/show-modal';
import { inputValue } from 'koroku-console/utils/dom';
import Button from './button';
import FormField from './form-field';

interface ConfirmDialogSignature {
  Args: {
    title: string;
    confirmLabel: string;
    /** Styles the confirm button as destructive. */
    danger?: boolean;
    /** The confirm button stays disabled until this text is typed. */
    requireText?: string;
    /** Disables both buttons while the confirmed action runs. */
    busy?: boolean;
    onConfirm: () => unknown;
    onCancel: () => unknown;
    /** An optional third choice between cancelling and confirming. */
    altLabel?: string;
    onAlt?: () => unknown;
  };
  Blocks: {
    default: [];
  };
}

/**
 * Modal confirmation built on the native `<dialog>`. It is open for as long
 * as it is rendered: wrap it in `{{#if}}`.
 */
export default class ConfirmDialog extends Component<ConfirmDialogSignature> {
  @tracked typed = '';

  get canConfirm(): boolean {
    if (this.args.busy) return false;
    const required = this.args.requireText;
    return !required || this.typed === required;
  }

  get cannotConfirm(): boolean {
    return !this.canConfirm;
  }

  setTyped = (event: Event) => {
    this.typed = inputValue(event);
  };

  submit = (event: Event) => {
    event.preventDefault();
    if (this.canConfirm) void this.args.onConfirm();
  };

  alt = () => {
    if (!this.args.busy) void this.args.onAlt?.();
  };

  cancel = (event: Event) => {
    // Escape fires `cancel`; the owner decides when the dialog goes away.
    event.preventDefault();
    if (!this.args.busy) void this.args.onCancel();
  };

  <template>
    {{#let (uniqueId) as |id|}}
      <dialog
        class="dialog"
        aria-labelledby="{{id}}-title"
        data-test-dialog
        {{showModal}}
        {{on "cancel" this.cancel}}
      >
        <form {{on "submit" this.submit}}>
          <h2 class="dialog-title" id="{{id}}-title">{{@title}}</h2>
          <div class="dialog-body">{{yield}}</div>

          {{#if @requireText}}
            <FormField>
              <:label>確認のため
                <code>{{@requireText}}</code>
                と入力してください</:label>
              <:default as |field|>
                <input
                  id={{field.id}}
                  type="text"
                  autocomplete="off"
                  spellcheck="false"
                  class="mono"
                  value={{this.typed}}
                  data-test-dialog-input
                  {{on "input" this.setTyped}}
                />
              </:default>
            </FormField>
          {{/if}}

          <div class="dialog-actions">
            <Button
              @disabled={{@busy}}
              data-test-dialog-cancel
              {{on "click" this.cancel}}
            >キャンセル</Button>
            {{#if @altLabel}}
              <Button
                @disabled={{@busy}}
                data-test-dialog-alt
                {{on "click" this.alt}}
              >{{@altLabel}}</Button>
            {{/if}}
            <Button
              @type="submit"
              @variant={{if @danger "danger" "primary"}}
              @disabled={{this.cannotConfirm}}
              @busy={{@busy}}
              @busyLabel="実行中…"
              data-test-dialog-confirm
            >{{@confirmLabel}}</Button>
          </div>
        </form>
      </dialog>
    {{/let}}
  </template>
}
