import type { TOC } from '@ember/component/template-only';
import { fn } from '@ember/helper';
import { on } from '@ember/modifier';

function not(value: boolean): boolean {
  return !value;
}

/**
 * An on/off setting that takes effect at once (live tail, line wrapping).
 * The state is shown by the position of the knob, not by colour alone, and
 * announced through `role="switch"`.
 */
const ToggleSwitch: TOC<{
  Element: HTMLButtonElement;
  Args: { checked: boolean; onChange: (checked: boolean) => unknown };
  Blocks: { default: [] };
}> = <template>
  <button
    type="button"
    role="switch"
    class="switch"
    aria-checked={{if @checked "true" "false"}}
    {{on "click" (fn @onChange (not @checked))}}
    ...attributes
  >
    <span class="switch-track" aria-hidden="true">
      <span class="switch-knob"></span>
    </span>
    <span class="switch-label">{{yield}}</span>
  </button>
</template>;

export default ToggleSwitch;
