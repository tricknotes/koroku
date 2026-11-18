import type { TOC } from '@ember/component/template-only';
import Icon from './icon';

export type ButtonVariant = 'default' | 'primary' | 'danger' | 'ghost';

/**
 * Class list of a button, for elements that only look like one (a link to
 * another page, for example): `<LinkTo class={{buttonClass "primary"}}>`.
 */
export function buttonClass(
  variant: ButtonVariant = 'default',
  size: 'medium' | 'small' = 'medium',
): string {
  const classes = ['button'];
  if (variant !== 'default') classes.push(`button-${variant}`);
  if (size === 'small') classes.push('button-small');
  return classes.join(' ');
}

function isDisabled(disabled?: boolean, busy?: boolean): boolean {
  return Boolean(disabled) || Boolean(busy);
}

/**
 * The button. `primary` is the one main action of a view, `danger` is for
 * actions that cannot be undone, `ghost` for a quiet action inside a row.
 * While `@busy` it is disabled and shows a spinner (and `@busyLabel`
 * instead of its content, when given).
 */
const Button: TOC<{
  Element: HTMLButtonElement;
  Args: {
    variant?: ButtonVariant;
    size?: 'medium' | 'small';
    /** `submit` inside a form; a plain `button` otherwise. */
    type?: 'button' | 'submit';
    disabled?: boolean;
    busy?: boolean;
    busyLabel?: string;
  };
  Blocks: { default: [] };
}> = <template>
  <button
    type={{if @type @type "button"}}
    class={{buttonClass @variant @size}}
    disabled={{isDisabled @disabled @busy}}
    aria-busy={{if @busy "true"}}
    ...attributes
  >
    {{#if @busy}}
      <Icon @name="spinner" />
      {{#if @busyLabel}}{{@busyLabel}}{{else}}{{yield}}{{/if}}
    {{else}}
      {{yield}}
    {{/if}}
  </button>
</template>;

export default Button;
