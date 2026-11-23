import Component from '@glimmer/component';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq } from '@ember/helper';
import type Owner from '@ember/owner';
import Button from 'koroku-console/components/button';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import RoleNote from 'koroku-console/components/role-note';
import SettingsConfirm from 'koroku-console/components/settings-confirm';
import focusSection from 'koroku-console/modifiers/focus-section';
import type ApiService from 'koroku-console/services/api';
import {
  inputValue,
  preventDefault,
  setChecked,
} from 'koroku-console/utils/dom';
import Loadable from 'koroku-console/utils/loadable';
import { can } from 'koroku-console/utils/roles';
import type SettingsEditor from 'koroku-console/utils/settings-editor';
import { IDLE_SLEEP_FIELDS } from 'koroku-console/utils/settings-editor';
import {
  IDLE_MINUTES_CHOICES,
  minutesLabel,
} from 'koroku-console/utils/settings-form';
import type { SystemInfo } from 'koroku-console/utils/types';

const KNOWN_FIELDS = new Set<string>(IDLE_SLEEP_FIELDS);

interface Signature {
  Element: HTMLElement;
  Args: {
    editor: SettingsEditor;
    /** Led here by a link (`?section=idle-sleep`): in view and focused. */
    focus: boolean;
  };
}

/**
 * 「アクセスがない間の休止」 on the resources tab (ARCHITECTURE 8.4, 9.1):
 * a setting that frees the host's memory, next to the limits and
 * reservations. Its own form and save.
 */
export default class IdleSleepSettings extends Component<Signature> {
  @service declare api: ApiService;

  /** Whether the host can count requests (`idle_sleep.available`). */
  system = new Loadable<SystemInfo>(() => this.api.getSystem());

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.system.load();
  }

  get editor(): SettingsEditor {
    return this.args.editor;
  }

  get readOnly(): boolean {
    return !can(this.editor.app, 'manage');
  }

  /**
   * Requests cannot be counted without Caddy's per-app access logs; the
   * setting can then only be turned off.
   */
  get unavailable(): boolean {
    return this.system.value?.idle_sleep?.available === false;
  }

  get switchDisabled(): boolean {
    return this.unavailable && !this.editor.form.idleSleep;
  }

  /** The saved idle time, offered even when it is not one of the choices. */
  get choices(): number[] {
    const minutes = this.editor.form.idleSleepMinutes;
    const choices = [...IDLE_MINUTES_CHOICES];
    if (!choices.includes(minutes)) {
      choices.push(minutes);
      choices.sort((a, b) => a - b);
    }
    return choices;
  }

  get otherErrors(): string[] {
    return this.editor.otherErrors(KNOWN_FIELDS);
  }

  setMinutes = (event: Event) => {
    this.editor.form.idleSleepMinutes = Number(inputValue(event));
    this.editor.touch();
  };

  save = async () => {
    await this.editor.save();
  };

  <template>
    <section
      class="pane idle-sleep-settings"
      id="idle-sleep"
      tabindex="-1"
      aria-labelledby="idle-sleep-title"
      data-test-idle-sleep-group
      {{focusSection @focus}}
      ...attributes
    >
      <header class="pane-header">
        <h2 class="pane-title" id="idle-sleep-title">アクセスがない間の休止</h2>
      </header>
      <p class="pane-description">
        アクセスがなければコンテナを止めてメモリを空け、次のアクセスで起動します。
      </p>
      {{#if this.readOnly}}
        <RoleNote @needed="manage" @what="休止の設定の変更" />
      {{/if}}

      <form
        class="idle-sleep-form"
        novalidate
        aria-label="アクセスがない間の休止"
        data-test-idle-sleep-form
        {{on "submit" (preventDefault this.save)}}
        {{on "input" @editor.touch}}
      >
        <fieldset class="role-fieldset" disabled={{this.readOnly}}>
          <label class="option">
            <input
              type="checkbox"
              checked={{@editor.form.idleSleep}}
              disabled={{this.switchDisabled}}
              aria-describedby="idle-sleep-hint"
              data-test-field="idle_sleep"
              {{on "change" (setChecked @editor.form "idleSleep")}}
            />
            <span class="option-body">
              <span class="option-label">アクセスがない間は止める</span>
              <span class="option-hint" id="idle-sleep-hint">
                最初のアクセスは起動を待ちます（Rails なら 5〜15
                秒）。死活監視などの定期的なアクセスがあると休止しません。
              </span>
            </span>
          </label>
          {{#if this.unavailable}}
            <p
              class="field-hint idle-sleep-unavailable"
              data-test-idle-sleep-unavailable
            >
              このホストでは使えません。アクセスを数えるために、Caddy
              のアプリ別のアクセスログ（<code>[caddy] access_log_dir</code>）が要ります。
            </p>
          {{/if}}
          {{#if @editor.form.idleSleep}}
            <FormField
              class="idle-sleep-minutes"
              @label="止めるまでの時間"
              @error={{@editor.errors.idle_sleep_minutes}}
              data-test-field-group="idle_sleep_minutes"
              as |field|
            >
              <select
                id={{field.id}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="idle_sleep_minutes"
                {{on "change" this.setMinutes}}
              >
                {{#each this.choices as |minutes|}}
                  <option
                    value={{minutes}}
                    selected={{eq minutes @editor.form.idleSleepMinutes}}
                  >{{minutesLabel minutes}}</option>
                {{/each}}
              </select>
            </FormField>
          {{/if}}
          {{#if @editor.errors.idle_sleep}}
            <p class="field-error" data-test-field-error>
              {{@editor.errors.idle_sleep}}
            </p>
          {{/if}}

          <div aria-live="polite">
            <ErrorMessage @error={{@editor.error}} />
            {{#if this.otherErrors.length}}
              <ul class="form-problems" data-test-other-errors>
                {{#each this.otherErrors as |line|}}
                  <li>{{line}}</li>
                {{/each}}
              </ul>
            {{/if}}
          </div>

          {{#unless this.readOnly}}
            <div class="form-footer section-footer">
              <Button
                @type="submit"
                @variant="primary"
                @busy={{@editor.saving}}
                @busyLabel="保存中…"
                data-test-idle-sleep-save
              >保存</Button>
              <p class="form-status" aria-live="polite">
                {{#if @editor.saved}}
                  <span class="form-saved" data-test-idle-sleep-saved>
                    <Icon @name="check" />
                    保存しました。
                  </span>
                {{else if @editor.nothingToSave}}
                  <span data-test-idle-sleep-unchanged>変更はありません。</span>
                {{/if}}
              </p>
            </div>
          {{/unless}}
        </fieldset>
      </form>
    </section>

    <SettingsConfirm @editor={{@editor}} />
  </template>
}
