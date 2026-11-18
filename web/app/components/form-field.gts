import Component from '@glimmer/component';
import { guidFor } from '@ember/object/internals';

/** What the control inside a `<FormField>` needs to be wired to it. */
export interface FieldControl {
  id: string;
  /** For `aria-describedby`: the hint and/or the error, when present. */
  describedBy: string | undefined;
  /** For `aria-invalid`. */
  invalid: 'true' | undefined;
}

interface FormFieldSignature {
  Element: HTMLDivElement;
  Args: {
    /** Plain-text label; use the `label` block when it needs markup. */
    label?: string;
    /** One line on what the value is for or its expected format. */
    hint?: string;
    /** The reason the current value was refused; shown under the control. */
    error?: string | null;
  };
  Blocks: {
    default: [control: FieldControl];
    label: [];
  };
}

/**
 * A labelled form control with an optional hint and error. It yields the
 * attributes that tie the control to them:
 *
 *   <FormField @label="Slug" @hint="英小文字で始めます" as |field|>
 *     <input
 *       id={{field.id}}
 *       aria-describedby={{field.describedBy}}
 *       aria-invalid={{field.invalid}}
 *     />
 *   </FormField>
 */
export default class FormField extends Component<FormFieldSignature> {
  id = `field-${guidFor(this)}`;

  get control(): FieldControl {
    const described: string[] = [];
    if (this.args.hint) described.push(`${this.id}-hint`);
    if (this.args.error) described.push(`${this.id}-error`);
    return {
      id: this.id,
      describedBy: described.length ? described.join(' ') : undefined,
      invalid: this.args.error ? 'true' : undefined,
    };
  }

  <template>
    <div class="field" ...attributes>
      <label class="field-label" for={{this.id}}>
        {{#if (has-block "label")}}
          {{yield to="label"}}
        {{else}}
          {{@label}}
        {{/if}}
      </label>
      {{yield this.control}}
      {{#if @hint}}
        <span class="field-hint" id="{{this.id}}-hint">{{@hint}}</span>
      {{/if}}
      {{#if @error}}
        <p class="field-error" id="{{this.id}}-error" data-test-field-error>
          {{@error}}
        </p>
      {{/if}}
    </div>
  </template>
}
