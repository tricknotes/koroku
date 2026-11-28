import type { TOC } from '@ember/component/template-only';
import { on } from '@ember/modifier';
import FormField, {
  type FieldControl,
} from 'koroku-console/components/form-field';
import CopyButton from 'koroku-console/components/copy-button';
import {
  COMPOSE_PROMPT,
  INLINE_COMPOSE_FILE,
  PLUGIN_SETUP_COMMANDS,
} from 'koroku-console/utils/compose-source';
import { inputValue } from 'koroku-console/utils/dom';

const PLACEHOLDER = `# 例
services:
  web:
    build: .
    ports:
      - "3000"
    environment:
      DATABASE_URL: \${DATABASE_URL:?}
`;

const VAR_SYNTAX = '${VAR}';

/** The notes under the text area, and the error when there is one. */
function describedBy(field: FieldControl): string {
  return [`${field.id}-notes`, field.describedBy ?? ''].join(' ').trim();
}

function update(onInput: (value: string) => void): (event: Event) => void {
  return (event: Event) => onInput(inputValue(event));
}

/**
 * The Compose file written in koroku (ARCHITECTURE 6.3): a plain text
 * area, what a writer has to know about where the file ends up, and the
 * prompt that has Claude Code write the file with the prepare-app Skill.
 */
const ComposeEditor: TOC<{
  Element: HTMLDivElement;
  Args: {
    value: string;
    error?: string;
    /** Shown but not editable: only the owner changes the file. */
    readonly?: boolean;
    onInput: (value: string) => void;
  };
}> = <template>
  <FormField
    class="compose-editor"
    @label="Compose の内容"
    @error={{@error}}
    data-test-field-group="compose_content"
    ...attributes
    as |field|
  >
    <ul class="compose-editor-notes" id="{{field.id}}-notes">
      <li>
        リポジトリの直下に
        <span class="mono">{{INLINE_COMPOSE_FILE}}</span>
        として置かれます。<code>build: .</code>
        などの相対パスは、リポジトリの直下から数えます。
      </li>
      <li>
        パスワードなどの値は直接書かず、環境変数に保存して
        <code>{{VAR_SYNTAX}}</code>
        で参照してください。
      </li>
      <li>保存した内容は、次のデプロイから使われます。</li>
    </ul>
    <textarea
      id={{field.id}}
      class="mono compose-editor-text"
      rows="14"
      wrap="off"
      spellcheck="false"
      autocomplete="off"
      autocapitalize="off"
      placeholder={{PLACEHOLDER}}
      readonly={{@readonly}}
      value={{@value}}
      aria-describedby={{describedBy field}}
      aria-invalid={{field.invalid}}
      data-test-field="compose_content"
      {{on "input" (update @onInput)}}
    ></textarea>
  </FormField>
  {{#unless @readonly}}
    <details class="disclosure compose-assist" data-test-compose-assist>
      <summary>Claude Code に Compose を書かせる</summary>
      <p>
        手元の Claude Code に koroku の Skill を使わせると、リポジトリを調べて
        Compose を作り、手元の Docker
        でデプロイをリハーサルしてから、ここに貼る内容を出します。
      </p>
      <ol class="compose-assist-steps">
        <li>
          デプロイするリポジトリを手元に取得し、そのディレクトリで Claude Code
          を開く。
        </li>
        <li>
          koroku のプラグインを入れる（初回だけ。1 行ずつ実行する）。
          {{#each PLUGIN_SETUP_COMMANDS as |command|}}
            <div class="command">
              <pre
                class="code-block"
                data-test-compose-assist-setup
              >{{command}}</pre>
              <CopyButton @text={{command}} @label="コマンド" />
            </div>
          {{/each}}
        </li>
        <li>
          次のプロンプトを送る。
          <div class="command">
            <pre
              class="code-block"
              data-test-compose-assist-prompt
            >{{COMPOSE_PROMPT}}</pre>
            <CopyButton @text={{COMPOSE_PROMPT}} @label="プロンプト" />
          </div>
        </li>
        <li>
          Claude が出した Compose
          をこの欄に貼り、公開するサービス、コンテナ側のポート、ヘルスチェックのパス、環境変数を、報告のとおりに入れる。
        </li>
      </ol>
    </details>
  {{/unless}}
</template>;

export default ComposeEditor;
