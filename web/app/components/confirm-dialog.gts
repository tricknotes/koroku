import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { on } from '@ember/modifier';
import { uniqueId } from '@ember/helper';
import showModal from 'paas-console/modifiers/show-modal';
import { inputValue } from 'paas-console/utils/dom';

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

  setTyped = (event: Event) => {
    this.typed = inputValue(event);
  };

  submit = (event: Event) => {
    event.preventDefault();
    if (this.canConfirm) void this.args.onConfirm();
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
            <label class="field" for="{{id}}-confirm">
              <span class="field-label">確認のため
                <code>{{@requireText}}</code>
                と入力してください</span>
              <input
                id="{{id}}-confirm"
                type="text"
                autocomplete="off"
                spellcheck="false"
                value={{this.typed}}
                data-test-dialog-input
                {{on "input" this.setTyped}}
              />
            </label>
          {{/if}}

          <div class="dialog-actions">
            <button
              type="button"
              class="button"
              disabled={{@busy}}
              data-test-dialog-cancel
              {{on "click" this.cancel}}
            >キャンセル</button>
            <button
              type="submit"
              class="button {{if @danger 'button-danger' 'button-primary'}}"
              disabled={{if this.canConfirm false true}}
              data-test-dialog-confirm
            >{{if @busy "実行中…" @confirmLabel}}</button>
          </div>
        </form>
      </dialog>
    {{/let}}
  </template>
}
