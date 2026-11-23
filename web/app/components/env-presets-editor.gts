import Component from '@glimmer/component';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import { tracked } from '@glimmer/tracking';
import { modifier } from 'ember-modifier';
import Badge from './badge';
import Banner from './banner';
import Button from './button';
import Icon from './icon';
import type SessionService from 'koroku-console/services/session';
import { inputValue } from 'koroku-console/utils/dom';
import {
  frameworkLabel,
  type EnvDraft,
  type EnvDraftRow,
} from 'koroku-console/utils/env-presets';
import type { EnvPresets } from 'koroku-console/utils/types';

const MASK = '••••••••';

/** Focuses the element when it is inserted (the key of a new row). */
const focusOnInsert = modifier<{
  Element: HTMLElement;
  Args: { Positional: [boolean] };
}>((element, [enabled]) => {
  if (enabled) element.focus();
});

/** The note under a row, and its error when there is one. */
function describedBy(id: string, error: string | undefined): string {
  return error ? `${id}-note ${id}-error` : `${id}-note`;
}

interface EnvPresetsEditorSignature {
  Args: {
    draft: EnvDraft;
    /** The last detection, null before the first one. */
    presets: EnvPresets | null;
    loading: boolean;
    /** The server's reason for refusing the variables (`details.env`). */
    error?: string;
  };
}

/**
 * The variables of the create form (ARCHITECTURE 9.8): suggestions from the
 * repository with their reason, generated secrets masked, each editable and
 * removable, and rows of the user's own.
 */
export default class EnvPresetsEditor extends Component<EnvPresetsEditorSignature> {
  @service declare session: SessionService;
  /** The row the user just added, whose key gets the focus. */
  @tracked addedId = '';

  get frameworks(): string {
    return (this.args.presets?.frameworks ?? []).map(frameworkLabel).join('、');
  }

  get skipped(): { key: string; service: string }[] {
    return this.args.presets?.skipped ?? [];
  }

  /** Saving variables needs a recent login, and logging in loses the form. */
  get needsReauth(): boolean {
    return (
      !this.session.recentlyAuthenticated && this.args.draft.vars.length > 0
    );
  }

  keyError = (row: EnvDraftRow): string | undefined =>
    this.args.draft.keyError(row);

  setKey = (row: EnvDraftRow, event: Event) => {
    this.args.draft.setKey(row, inputValue(event));
  };

  setValue = (row: EnvDraftRow, event: Event) => {
    this.args.draft.setValue(row, inputValue(event));
  };

  toggleReveal = (row: EnvDraftRow) => {
    row.revealed = !row.revealed;
  };

  remove = (row: EnvDraftRow) => {
    this.args.draft.remove(row);
  };

  add = () => {
    this.addedId = this.args.draft.add().id;
  };

  isAdded = (row: EnvDraftRow): boolean => row.id === this.addedId;

  reauthenticate = () => this.session.reauthenticate();

  <template>
    <div class="env-presets" data-test-env-presets>
      {{#if @loading}}
        <p class="new-app-progress" data-test-env-presets-loading>
          <Icon @name="spinner" />
          リポジトリを調べています…
        </p>
      {{else if this.frameworks}}
        <dl class="readout">
          <dt>検出したフレームワーク</dt>
          <dd data-test-env-presets-frameworks>{{this.frameworks}}</dd>
        </dl>
      {{/if}}

      {{#if @draft.rows.length}}
        <ul class="env-draft" aria-label="作成時に保存する環境変数">
          {{#each @draft.rows key="id" as |row|}}
            {{#let (this.keyError row) as |keyError|}}
              <li class="env-draft-row" data-test-env-draft-row={{row.key}}>
                <div class="env-draft-key">
                  <label class="visually-hidden" for="{{row.id}}-key">
                    キー
                  </label>
                  <input
                    id="{{row.id}}-key"
                    type="text"
                    class="mono"
                    placeholder="KEY"
                    spellcheck="false"
                    autocapitalize="off"
                    autocomplete="off"
                    value={{row.key}}
                    aria-invalid={{if keyError "true"}}
                    aria-describedby={{describedBy row.id keyError}}
                    data-test-env-draft-key
                    {{on "input" (fn this.setKey row)}}
                    {{focusOnInsert (this.isAdded row)}}
                  />
                </div>
                <div class="env-draft-value">
                  {{#if row.masked}}
                    <span
                      class="env-draft-masked"
                      aria-hidden="true"
                      data-test-env-draft-masked
                    >{{MASK}}</span>
                    <span class="visually-hidden">値は隠されています</span>
                  {{else}}
                    <label class="visually-hidden" for="{{row.id}}-value">
                      {{row.key}}
                      の値
                    </label>
                    <input
                      id="{{row.id}}-value"
                      type="text"
                      class="mono"
                      placeholder="値"
                      spellcheck="false"
                      autocapitalize="off"
                      autocomplete="off"
                      value={{row.value}}
                      data-test-env-draft-value
                      {{on "input" (fn this.setValue row)}}
                    />
                  {{/if}}
                </div>
                <div class="env-draft-actions">
                  {{#if row.generated}}
                    <Button
                      @variant="ghost"
                      @size="small"
                      aria-pressed={{if row.revealed "true" "false"}}
                      aria-label="{{row.key}} の値を{{if
                        row.revealed
                        '隠す'
                        '表示する'
                      }}"
                      data-test-env-draft-reveal
                      {{on "click" (fn this.toggleReveal row)}}
                    >{{if row.revealed "隠す" "表示"}}</Button>
                  {{/if}}
                  <Button
                    @variant="ghost"
                    @size="small"
                    aria-label="{{if row.key row.key '空の行'}} を外す"
                    data-test-env-draft-remove
                    {{on "click" (fn this.remove row)}}
                  >外す</Button>
                </div>
                <p class="env-draft-note" id="{{row.id}}-note">
                  {{#if row.generated}}
                    <Badge data-test-env-draft-generated>自動生成</Badge>
                  {{/if}}
                  {{#if row.reason}}
                    <span data-test-env-draft-reason>{{row.reason}}</span>
                  {{else}}
                    <span>手で追加した変数</span>
                  {{/if}}
                </p>
                {{#if keyError}}
                  <p
                    class="field-error env-draft-error"
                    id="{{row.id}}-error"
                    data-test-env-draft-error
                  >{{keyError}}</p>
                {{/if}}
              </li>
            {{/let}}
          {{/each}}
        </ul>
      {{else if @presets}}
        <p class="env-presets-none" data-test-env-presets-none>
          リポジトリから初期値にする変数は見つかりませんでした。
        </p>
      {{/if}}

      {{#if this.skipped.length}}
        <ul class="env-presets-skipped" data-test-env-presets-skipped>
          {{#each this.skipped as |item|}}
            <li>
              <span class="mono">{{item.key}}</span>
              は Compose ファイル（{{item.service}}
              サービス）が値を書いているため、入れていません。
            </li>
          {{/each}}
        </ul>
      {{/if}}

      <div class="env-presets-footer">
        <Button @size="small" data-test-env-draft-add {{on "click" this.add}}>
          変数を追加
        </Button>
      </div>

      {{#if @error}}
        <p class="field-error" data-test-env-presets-error>{{@error}}</p>
      {{/if}}

      {{#if this.needsReauth}}
        <Banner
          @tone="warning"
          @title="環境変数を一緒に保存するには再ログインが必要です"
          data-test-env-presets-reauth
        >
          <:default>
            再ログインすると入力が消えるので、先に再ログインするか、変数を外して作成し、あとで「環境変数」タブで足してください。
          </:default>
          <:actions>
            <Button
              @size="small"
              data-test-env-presets-reauth-button
              {{on "click" this.reauthenticate}}
            >GitHub で再ログイン</Button>
          </:actions>
        </Banner>
      {{/if}}
    </div>
  </template>
}
