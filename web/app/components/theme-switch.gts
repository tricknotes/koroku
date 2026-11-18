import Component from '@glimmer/component';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import { guidFor } from '@ember/object/internals';
import type ThemeService from 'koroku-console/services/theme';
import type { ThemePreference } from 'koroku-console/services/theme';
import Icon, { type IconName } from './icon';

interface Option {
  value: ThemePreference;
  label: string;
  icon: IconName;
}

const OPTIONS: Option[] = [
  { value: 'system', label: 'OS の設定に合わせる', icon: 'system' },
  { value: 'light', label: 'ライト', icon: 'sun' },
  { value: 'dark', label: 'ダーク', icon: 'moon' },
];

function same(a: ThemePreference, b: ThemePreference): boolean {
  return a === b;
}

/**
 * System / light / dark, as three radio buttons drawn as one segmented
 * control. The arrow keys move between them like any radio group.
 */
export default class ThemeSwitch extends Component<{
  Element: HTMLFieldSetElement;
}> {
  @service declare theme: ThemeService;

  name = `theme-${guidFor(this)}`;

  select = (value: ThemePreference) => {
    this.theme.select(value);
  };

  <template>
    <fieldset class="segmented" data-test-theme-switch ...attributes>
      <legend class="visually-hidden">テーマ</legend>
      {{#each OPTIONS as |option|}}
        <label class="segmented-option" title="テーマ: {{option.label}}">
          <input
            type="radio"
            class="visually-hidden"
            name={{this.name}}
            value={{option.value}}
            checked={{same this.theme.preference option.value}}
            data-test-theme={{option.value}}
            {{on "change" (fn this.select option.value)}}
          />
          <Icon @name={{option.icon}} />
          <span class="visually-hidden">{{option.label}}</span>
        </label>
      {{/each}}
    </fieldset>
  </template>
}
