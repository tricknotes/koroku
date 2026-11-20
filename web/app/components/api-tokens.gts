import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn, uniqueId } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from './async-view';
import Banner from './banner';
import Button from './button';
import ConfirmDialog from './confirm-dialog';
import CopyButton from './copy-button';
import EmptyState from './empty-state';
import ErrorMessage from './error-message';
import FormField from './form-field';
import RelativeTime from './relative-time';
import RowList from './row-list';
import TimeUntil from './time-until';
import showModal from 'koroku-console/modifiers/show-modal';
import type ApiService from 'koroku-console/services/api';
import type SessionService from 'koroku-console/services/session';
import {
  ApiError,
  errorMessage,
  fieldErrors,
} from 'koroku-console/utils/errors';
import { inputValue } from 'koroku-console/utils/dom';
import Loadable from 'koroku-console/utils/loadable';
import type {
  ApiToken,
  CreatedApiToken,
  TokenLifetime,
} from 'koroku-console/utils/types';

const LIFETIMES: { days: TokenLifetime; label: string }[] = [
  { days: 7, label: '7 日' },
  { days: 30, label: '30 日' },
  { days: 90, label: '90 日（最長）' },
];

const FIELD_MESSAGES: Record<string, string> = {
  name: '名前を 1〜100 文字で入力してください。',
  expires_in_days: '有効期限は 7 日、30 日、90 日から選んでください。',
};

function isLifetime(days: TokenLifetime, chosen: TokenLifetime): boolean {
  return days === chosen;
}

/**
 * Read-only API tokens that let Claude Code on the developer's machine read
 * deployments and logs (ARCHITECTURE 10.4): list, create (shown once),
 * revoke.
 */
export default class ApiTokens extends Component {
  @service declare api: ApiService;
  @service declare session: SessionService;

  tokens = new Loadable<ApiToken[]>(() => this.api.listTokens());

  @tracked creating = false;
  @tracked name = '';
  @tracked lifetime: TokenLifetime = 30;
  @tracked saving = false;
  @tracked formError: unknown = null;
  /** The token just created; its value is never shown again. */
  @tracked created: CreatedApiToken | null = null;

  @tracked revoking: ApiToken | null = null;
  @tracked revokeBusy = false;
  @tracked actionError: unknown = null;

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.tokens.load();
  }

  get consoleOrigin(): string {
    return window.location.origin;
  }

  get shellSnippet(): string {
    const value = this.created?.value ?? '';
    return `export KOROKU_URL=${this.consoleOrigin}\nexport KOROKU_TOKEN=${value}`;
  }

  get fieldErrors(): Record<string, string> {
    const fields = fieldErrors(this.formError);
    const out: Record<string, string> = {};
    for (const name of Object.keys(fields)) {
      out[name] = FIELD_MESSAGES[name] ?? fields[name] ?? '';
    }
    return out;
  }

  /** An error that is not about one field. */
  get formMessage(): string | null {
    if (!this.formError) return null;
    if (Object.keys(this.fieldErrors).length > 0) return null;
    return errorMessage(this.formError);
  }

  openCreate = () => {
    this.actionError = null;
    // Creating needs a recent login; ask before the form, not after it.
    if (!this.session.recentlyAuthenticated) {
      this.session.promptReauth();
      return;
    }
    this.name = '';
    this.lifetime = 30;
    this.formError = null;
    this.created = null;
    this.creating = true;
  };

  setName = (event: Event) => {
    this.name = inputValue(event);
  };

  setLifetime = (event: Event) => {
    this.lifetime = Number(inputValue(event)) as TokenLifetime;
  };

  submit = async (event: Event) => {
    event.preventDefault();
    if (this.saving) return;
    this.saving = true;
    this.formError = null;
    try {
      this.created = await this.api.createToken(
        this.name.trim(),
        this.lifetime,
      );
      await this.tokens.load({ quiet: true });
    } catch (error) {
      if (error instanceof ApiError && error.code === 'reauth_required') {
        // The re-login dialog takes over; this one would sit underneath.
        this.creating = false;
      } else {
        this.formError = error;
      }
    } finally {
      this.saving = false;
    }
  };

  closeCreate = () => {
    if (this.saving) return;
    this.creating = false;
    this.created = null;
  };

  onDialogCancel = (event: Event) => {
    event.preventDefault();
    this.closeCreate();
  };

  requestRevoke = (token: ApiToken) => {
    this.actionError = null;
    this.revoking = token;
  };

  cancelRevoke = () => {
    this.revoking = null;
  };

  revoke = async () => {
    const token = this.revoking;
    if (!token) return;
    this.revokeBusy = true;
    try {
      await this.api.deleteToken(token.id);
      await this.tokens.load({ quiet: true });
    } catch (error) {
      this.actionError = error;
    } finally {
      this.revokeBusy = false;
      this.revoking = null;
    }
  };

  <template>
    <section class="pane" aria-labelledby="tokens-title" id="api-tokens">
      <div class="pane-header">
        <h2 class="pane-title" id="tokens-title">API トークン</h2>
        {{#if this.tokens.value.length}}
          <Button data-test-token-create {{on "click" this.openCreate}}>
            トークンを作成…
          </Button>
        {{/if}}
      </div>
      <p class="security-note">
        手元の Claude Code
        などから、デプロイの結果とログを読むための読み取り専用のトークンです。デプロイや設定の変更、環境変数の値の表示には使えません。
      </p>

      <div class="pane-notices" aria-live="polite">
        <ErrorMessage @error={{this.actionError}} />
      </div>

      <AsyncView
        @data={{this.tokens}}
        @errorTitle="API トークンの一覧を取得できませんでした"
      >
        <:default as |tokens|>
          {{#if tokens.length}}
            <RowList class="token-rows" @label="API トークン" data-test-tokens>
              <:header>
                <span>名前</span>
                <span>作成</span>
                <span>最終利用</span>
                <span>有効期限</span>
                <span></span>
              </:header>
              <:rows as |Row|>
                {{#each tokens key="id" as |token|}}
                  <Row data-test-token={{token.id}}>
                    <span class="token-name" data-test-token-name>
                      {{token.name}}
                    </span>
                    <span class="session-time">
                      <span class="cell-label">作成</span>
                      <RelativeTime @time={{token.created_at}} />
                    </span>
                    <span
                      class="session-time token-last-used"
                      data-test-token-last-used
                    >
                      <span class="cell-label">最終利用</span>
                      {{#if token.last_used_at}}
                        <RelativeTime @time={{token.last_used_at}} />
                        {{#if token.last_used_ip}}
                          <span
                            class="mono session-ip"
                          >{{token.last_used_ip}}</span>
                        {{/if}}
                      {{else}}
                        未使用
                      {{/if}}
                    </span>
                    <span class="session-time">
                      <span class="cell-label">有効期限</span>
                      <TimeUntil @time={{token.expires_at}} />
                    </span>
                    <span class="session-action">
                      <Button
                        @size="small"
                        aria-label="{{token.name}} を失効"
                        data-test-token-revoke
                        {{on "click" (fn this.requestRevoke token)}}
                      >失効</Button>
                    </span>
                  </Row>
                {{/each}}
              </:rows>
            </RowList>
          {{else}}
            <EmptyState
              @title="API トークンはありません"
              @description="Claude Code にデプロイの失敗の原因を調べさせるときに作成します。"
              data-test-no-tokens
            >
              <:action>
                <Button
                  @variant="primary"
                  data-test-token-create
                  {{on "click" this.openCreate}}
                >トークンを作成</Button>
              </:action>
            </EmptyState>
          {{/if}}
        </:default>
      </AsyncView>
    </section>

    {{#if this.creating}}
      {{#let (uniqueId) as |id|}}
        <dialog
          class="dialog token-dialog"
          aria-labelledby="{{id}}-title"
          data-test-dialog
          data-test-token-dialog
          {{showModal}}
          {{on "cancel" this.onDialogCancel}}
        >
          {{#if this.created}}
            <h2
              class="dialog-title"
              id="{{id}}-title"
            >トークンを作成しました</h2>
            <div class="dialog-body">
              <Banner @tone="warning" data-test-token-once>
                この値は今しか表示されません。閉じる前にコピーして、手元のシェルの環境変数に設定してください。リポジトリにはコミットしないでください。
              </Banner>
              <div class="token-value">
                <code
                  class="mono"
                  data-test-token-value
                >{{this.created.value}}</code>
                <CopyButton
                  @text={{this.created.value}}
                  @label="トークン"
                  data-test-token-copy
                />
              </div>
              <p class="token-howto">
                シェルの設定（<code>~/.zshrc</code>
                など）か、direnv の
                <code>.envrc</code>（git の管理外）に書きます。
              </p>
              <div class="token-snippet">
                <pre
                  class="mono"
                  data-test-token-snippet
                >{{this.shellSnippet}}</pre>
                <CopyButton @text={{this.shellSnippet}} @label="設定例" />
              </div>
            </div>
            <div class="dialog-actions">
              <Button
                @variant="primary"
                data-test-dialog-close
                {{on "click" this.closeCreate}}
              >コピーしたので閉じる</Button>
            </div>
          {{else}}
            <form {{on "submit" this.submit}}>
              <h2 class="dialog-title" id="{{id}}-title">API トークンを作成</h2>
              <div class="dialog-body">
                <p
                >読み取り専用です。アプリ、デプロイとそのログ、実行ログ、必要な環境変数の照合（キーだけ）、ホストの状態を読めます。</p>
                <div class="token-fields">
                  <FormField
                    @label="名前"
                    @hint="どこで使うかのメモ。例: ノート PC の Claude Code"
                    @error={{this.fieldErrors.name}}
                    as |field|
                  >
                    <input
                      id={{field.id}}
                      type="text"
                      maxlength="100"
                      autocomplete="off"
                      required
                      value={{this.name}}
                      aria-describedby={{field.describedBy}}
                      aria-invalid={{field.invalid}}
                      data-test-token-name-input
                      {{on "input" this.setName}}
                    />
                  </FormField>
                  <FormField
                    @label="有効期限"
                    @error={{this.fieldErrors.expires_in_days}}
                    as |field|
                  >
                    <select
                      id={{field.id}}
                      aria-describedby={{field.describedBy}}
                      aria-invalid={{field.invalid}}
                      data-test-token-lifetime
                      {{on "change" this.setLifetime}}
                    >
                      {{#each LIFETIMES as |choice|}}
                        <option
                          value={{choice.days}}
                          selected={{isLifetime choice.days this.lifetime}}
                        >{{choice.label}}</option>
                      {{/each}}
                    </select>
                  </FormField>
                </div>
                {{#if this.formMessage}}
                  <p class="field-error" role="alert" data-test-token-error>
                    {{this.formMessage}}
                  </p>
                {{/if}}
              </div>
              <div class="dialog-actions">
                <Button
                  @disabled={{this.saving}}
                  data-test-dialog-cancel
                  {{on "click" this.closeCreate}}
                >キャンセル</Button>
                <Button
                  @type="submit"
                  @variant="primary"
                  @busy={{this.saving}}
                  @busyLabel="作成中…"
                  data-test-dialog-confirm
                >作成</Button>
              </div>
            </form>
          {{/if}}
        </dialog>
      {{/let}}
    {{/if}}

    {{#if this.revoking}}
      <ConfirmDialog
        @title="トークンを失効させますか？"
        @confirmLabel="失効させる"
        @danger={{true}}
        @busy={{this.revokeBusy}}
        @onConfirm={{this.revoke}}
        @onCancel={{this.cancelRevoke}}
      >
        <p><strong>{{this.revoking.name}}</strong>
          はすぐに使えなくなります。このトークンを設定した Claude Code
          などは、新しいトークンを作るまで koroku を読めません。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
