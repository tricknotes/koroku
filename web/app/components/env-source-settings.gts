import Component from '@glimmer/component';
import { on } from '@ember/modifier';
import Button from 'koroku-console/components/button';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import Icon from 'koroku-console/components/icon';
import RoleNote from 'koroku-console/components/role-note';
import focusSection from 'koroku-console/modifiers/focus-section';
import { inputValue, preventDefault, setValue } from 'koroku-console/utils/dom';
import { can } from 'koroku-console/utils/roles';
import type SettingsEditor from 'koroku-console/utils/settings-editor';
import { ENV_SOURCE_FIELDS } from 'koroku-console/utils/settings-editor';
import type { EnvSource } from 'koroku-console/utils/types';

const KNOWN_FIELDS = new Set<string>(ENV_SOURCE_FIELDS);

const VAR_PLAIN = '${VAR}';
const VAR_REQUIRED = '${VAR:?説明}';
const VAR_OPTIONAL = '${VAR:-既定値}';

function isSource(current: EnvSource, value: EnvSource): boolean {
  return current === value;
}

interface Signature {
  Element: HTMLElement;
  Args: {
    editor: SettingsEditor;
    /** Led here by a link (`?section=env-requirements`). */
    focus: boolean;
  };
}

/**
 * Where the requirements check reads the variables the repository needs
 * (ARCHITECTURE 9.4), on the environment tab next to its results. Saving
 * checks again at once.
 */
export default class EnvSourceSettings extends Component<Signature> {
  get editor(): SettingsEditor {
    return this.args.editor;
  }

  get readOnly(): boolean {
    return !can(this.editor.app, 'manage');
  }

  get otherErrors(): string[] {
    return this.editor.otherErrors(KNOWN_FIELDS);
  }

  setSource = (event: Event) => {
    this.editor.form.envSource = inputValue(event) as EnvSource;
    this.editor.touch();
  };

  save = async () => {
    await this.editor.save();
  };

  <template>
    <section
      class="pane env-source"
      id="env-requirements"
      tabindex="-1"
      aria-labelledby="env-source-title"
      data-test-env-source-group
      {{focusSection @focus}}
      ...attributes
    >
      <header class="pane-header">
        <h2 class="pane-title" id="env-source-title">必要な環境変数の照合</h2>
      </header>
      <p class="pane-description">
        リポジトリが必要とする環境変数を読み、この画面とデプロイの前に、未設定のものを知らせます。デプロイは止めません。照合は、既定のブランチの最新のコミットで行います。
      </p>
      {{#if this.readOnly}}
        <RoleNote @needed="manage" @what="照合元の変更" />
      {{/if}}

      <form
        class="env-source-form"
        novalidate
        aria-label="照合元"
        data-test-env-source-form
        {{on "submit" (preventDefault this.save)}}
        {{on "input" @editor.touch}}
      >
        <fieldset class="role-fieldset" disabled={{this.readOnly}}>
          <fieldset class="option-group">
            <legend class="field-label">照合元</legend>
            <label class="option">
              <input
                type="radio"
                name="env_source"
                value="compose"
                checked={{isSource @editor.form.envSource "compose"}}
                data-test-env-source-option="compose"
                {{on "change" this.setSource}}
              />
              <span class="option-body">
                <span class="option-label">Compose ファイルの
                  <code>{{VAR_PLAIN}}</code>（推奨）</span>
                <span class="option-hint">
                  <code>{{VAR_REQUIRED}}</code>
                  と既定値のない参照は必須、<code>{{VAR_OPTIONAL}}</code>
                  は任意として扱います。アプリのコードだけが読む変数は対象外です。
                </span>
              </span>
            </label>
            <label class="option">
              <input
                type="radio"
                name="env_source"
                value="dotenv"
                checked={{isSource @editor.form.envSource "dotenv"}}
                data-test-env-source-option="dotenv"
                {{on "change" this.setSource}}
              />
              <span class="option-body">
                <span class="option-label">.env.example</span>
                <span class="option-hint">
                  書かれたキーをすべて必須として扱い、直前のコメントを説明として表示します。
                </span>
              </span>
            </label>
            <label class="option">
              <input
                type="radio"
                name="env_source"
                value="off"
                checked={{isSource @editor.form.envSource "off"}}
                data-test-env-source-option="off"
                {{on "change" this.setSource}}
              />
              <span class="option-body">
                <span class="option-label">照合しない</span>
              </span>
            </label>
          </fieldset>
          {{#if (isSource @editor.form.envSource "dotenv")}}
            <FormField
              class="env-source-path"
              @label=".env.example のパス"
              @hint="空ならリポジトリ直下の .env.example を読みます。"
              @error={{@editor.errors.env_example_path}}
              data-test-field-group="env_example_path"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                class="mono"
                spellcheck="false"
                autocomplete="off"
                placeholder=".env.example"
                value={{@editor.form.envExamplePath}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="env_example_path"
                {{on "input" (setValue @editor.form "envExamplePath")}}
              />
            </FormField>
          {{/if}}
          {{#if @editor.errors.env_source}}
            <p class="field-error">{{@editor.errors.env_source}}</p>
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
                data-test-env-source-save
              >保存</Button>
              <p class="form-status" aria-live="polite">
                {{#if @editor.saved}}
                  <span class="form-saved" data-test-env-source-saved>
                    <Icon @name="check" />
                    保存しました。照合し直しました。
                  </span>
                {{else if @editor.nothingToSave}}
                  <span data-test-env-source-unchanged>変更はありません。</span>
                {{/if}}
              </p>
            </div>
          {{/unless}}
        </fieldset>
      </form>
    </section>
  </template>
}
