import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn, get } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from 'paas-console/components/async-view';
import ErrorMessage from 'paas-console/components/error-message';
import type ApiService from 'paas-console/services/api';
import type AppContext from 'paas-console/utils/app-context';
import { preventDefault, setValue } from 'paas-console/utils/dom';
import { fieldErrors } from 'paas-console/utils/errors';
import { isValidEnvKey } from 'paas-console/utils/format';
import Loadable from 'paas-console/utils/loadable';
import type { EnvVar } from 'paas-console/utils/types';

let nextRowId = 0;

class EnvRow {
  readonly id = `env-${nextRowId++}`;
  @tracked key: string;
  @tracked value: string;
  /** Values stay out of the DOM until the row is revealed. */
  @tracked revealed: boolean;

  constructor(key: string, value: string, revealed: boolean) {
    this.key = key;
    this.value = value;
    this.revealed = revealed;
  }

  get blank(): boolean {
    return this.key.trim() === '' && this.value === '';
  }

  get rows(): number {
    return Math.min(8, this.value.split('\n').length);
  }
}

function serialize(vars: EnvVar[]): string {
  return JSON.stringify(vars.map((v) => [v.key, v.value]));
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppEnvTemplate extends Component<Signature> {
  @service declare api: ApiService;

  vars = new Loadable<EnvVar[]>(() => this.api.getEnv(this.args.model.slug));

  @tracked rows: EnvRow[] = [];
  @tracked saving = false;
  @tracked saved = false;
  @tracked error: unknown = null;
  @tracked serverErrors: Record<string, string> = {};

  private original = serialize([]);

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.load();
  }

  private async load(): Promise<void> {
    await this.vars.load();
    if (this.vars.value) this.reset(this.vars.value);
  }

  private reset(vars: EnvVar[]): void {
    this.rows = vars.map((v) => new EnvRow(v.key, v.value, false));
    this.original = serialize(vars);
  }

  /** What would be sent: rows left completely empty are ignored. */
  get current(): EnvVar[] {
    return this.rows
      .filter((row) => !row.blank)
      .map((row) => ({ key: row.key.trim(), value: row.value }));
  }

  get dirty(): boolean {
    return serialize(this.current) !== this.original;
  }

  /** Row id → problem with its key (the API's rules, checked up front). */
  get keyErrors(): Record<string, string> {
    const errors: Record<string, string> = {};
    const seen = new Set<string>();
    for (const row of this.rows) {
      if (row.blank) continue;
      const key = row.key.trim();
      if (key === '') {
        errors[row.id] = 'キーを入力してください。';
      } else if (!isValidEnvKey(key)) {
        errors[row.id] =
          '英字かアンダースコアで始まる英数字とアンダースコアだけが使えます。';
      } else if (seen.has(key)) {
        errors[row.id] = 'キーが重複しています。';
      }
      seen.add(key);
    }
    return errors;
  }

  get invalid(): boolean {
    return Object.keys(this.keyErrors).length > 0;
  }

  get allRevealed(): boolean {
    return this.rows.length > 0 && this.rows.every((row) => row.revealed);
  }

  get serverErrorLines(): string[] {
    return Object.entries(this.serverErrors).map(
      ([name, reason]) => `${name}: ${reason}`,
    );
  }

  add = () => {
    this.rows = [...this.rows, new EnvRow('', '', true)];
    this.saved = false;
  };

  remove = (row: EnvRow) => {
    this.rows = this.rows.filter((r) => r !== row);
    this.saved = false;
  };

  toggle = (row: EnvRow) => {
    row.revealed = !row.revealed;
  };

  toggleAll = () => {
    const reveal = !this.allRevealed;
    for (const row of this.rows) row.revealed = reveal;
  };

  touch = () => {
    this.saved = false;
  };

  discard = () => {
    if (this.vars.value) this.reset(this.vars.value);
    this.error = null;
    this.serverErrors = {};
  };

  save = async () => {
    if (this.invalid) return;
    this.saving = true;
    this.saved = false;
    this.error = null;
    this.serverErrors = {};
    try {
      const saved = await this.api.putEnv(this.args.model.slug, this.current);
      this.vars.set(saved);
      this.reset(saved);
      this.saved = true;
    } catch (error) {
      this.error = error;
      this.serverErrors = fieldErrors(error);
    } finally {
      this.saving = false;
    }
  };

  <template>
    <section class="card" aria-labelledby="env-title">
      <div class="card-header">
        <h3 id="env-title">環境変数</h3>
      </div>
      <p
        class="notice"
        data-test-env-notice
      >変更は保存しただけでは反映されません。<strong
        >次のデプロイから</strong>反映されます。</p>

      <AsyncView @data={{this.vars}}>
        <form
          class="form"
          autocomplete="off"
          data-test-env-form
          {{on "submit" (preventDefault this.save)}}
          {{on "input" this.touch}}
        >
          {{#if this.rows.length}}
            <div class="toolbar">
              <button
                type="button"
                class="button button-small"
                aria-pressed={{if this.allRevealed "true" "false"}}
                data-test-env-toggle-all
                {{on "click" this.toggleAll}}
              >{{if this.allRevealed "すべて隠す" "すべて表示"}}</button>
            </div>

            <ul class="env-list">
              {{#each this.rows key="id" as |row|}}
                <li class="env-row" data-test-env-row>
                  <label class="field" for="{{row.id}}-key">
                    <span class="visually-hidden">キー</span>
                    <input
                      id="{{row.id}}-key"
                      type="text"
                      class="mono"
                      placeholder="KEY"
                      spellcheck="false"
                      autocapitalize="off"
                      autocomplete="off"
                      value={{row.key}}
                      aria-invalid={{if (get this.keyErrors row.id) "true"}}
                      data-test-env-key
                      {{on "input" (setValue row "key")}}
                    />
                    {{#if (get this.keyErrors row.id)}}
                      <span class="field-error" data-test-env-key-error>
                        {{get this.keyErrors row.id}}
                      </span>
                    {{/if}}
                  </label>

                  <div class="field env-value">
                    {{#if row.revealed}}
                      <label class="visually-hidden" for="{{row.id}}-value">
                        値
                      </label>
                      <textarea
                        id="{{row.id}}-value"
                        class="mono"
                        rows={{row.rows}}
                        spellcheck="false"
                        autocapitalize="off"
                        autocomplete="off"
                        value={{row.value}}
                        data-test-env-value
                        {{on "input" (setValue row "value")}}
                      ></textarea>
                    {{else}}
                      <span
                        class="masked"
                        aria-label="値は隠されています"
                        data-test-env-masked
                      >••••••••</span>
                    {{/if}}
                  </div>

                  <div class="button-row">
                    <button
                      type="button"
                      class="button button-small"
                      aria-pressed={{if row.revealed "true" "false"}}
                      data-test-env-toggle
                      {{on "click" (fn this.toggle row)}}
                    >{{if row.revealed "隠す" "表示・編集"}}</button>
                    <button
                      type="button"
                      class="button button-small button-danger"
                      data-test-env-remove
                      {{on "click" (fn this.remove row)}}
                    >削除</button>
                  </div>
                </li>
              {{/each}}
            </ul>
          {{else}}
            <p class="muted" data-test-env-empty>環境変数はまだありません。</p>
          {{/if}}

          <div class="form-actions">
            <button
              type="button"
              class="button"
              data-test-env-add
              {{on "click" this.add}}
            >変数を追加</button>
            <button
              type="submit"
              class="button button-primary"
              disabled={{if this.saving true this.invalid}}
              data-test-env-save
            >{{if this.saving "保存中…" "保存する"}}</button>
            {{#if this.dirty}}
              <button
                type="button"
                class="button"
                disabled={{this.saving}}
                data-test-env-discard
                {{on "click" this.discard}}
              >変更を破棄</button>
            {{/if}}
          </div>

          <div aria-live="polite">
            {{#if this.saved}}
              <p
                class="notice notice-ok"
                data-test-env-saved
              >保存しました。次のデプロイから反映されます。</p>
            {{else if this.dirty}}
              <p class="muted" data-test-env-dirty>未保存の変更があります。</p>
            {{/if}}
            <ErrorMessage @error={{this.error}} />
            {{#if this.serverErrorLines.length}}
              <ul class="field-error" data-test-env-server-errors>
                {{#each this.serverErrorLines as |line|}}
                  <li>{{line}}</li>
                {{/each}}
              </ul>
            {{/if}}
          </div>
        </form>
      </AsyncView>
    </section>
  </template>
}
