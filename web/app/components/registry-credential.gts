import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import type Owner from '@ember/owner';
import AsyncView from './async-view';
import Button from './button';
import ConfirmDialog from './confirm-dialog';
import ErrorMessage from './error-message';
import FormField from './form-field';
import Icon from './icon';
import RelativeTime from './relative-time';
import type ApiService from 'koroku-console/services/api';
import type SessionService from 'koroku-console/services/session';
import { inputValue } from 'koroku-console/utils/dom';
import {
  ApiError,
  errorMessage,
  fieldErrors,
} from 'koroku-console/utils/errors';
import Loadable from 'koroku-console/utils/loadable';
import type { RegistryCredential } from 'koroku-console/utils/types';

const DEFAULT_REGISTRY = 'ghcr.io';

const FIELD_MESSAGES: Record<string, string> = {
  registry:
    'レジストリのホスト名（必要ならポート）を入力してください。例: ghcr.io。https:// やパスは付けません。',
  username: 'ユーザー名を入力してください（コロンと空白は使えません）。',
  token: 'トークンを入力してください（空白は含められません）。',
};

interface RegistryCredentialSignature {
  Element: HTMLElement;
  Args: {
    slug: string;
    /** The user may change it (manage or owner). */
    canEdit: boolean;
  };
}

/**
 * The app's container registry login (ARCHITECTURE 6.1). koroku writes it
 * into a Docker config of this app alone for builds, pulls and starts.
 * The token is write-only: the API only says whether one is saved.
 */
export default class RegistryCredentialPanel extends Component<RegistryCredentialSignature> {
  @service declare api: ApiService;
  @service declare session: SessionService;

  credential = new Loadable<RegistryCredential>(() =>
    this.api.getRegistryCredential(this.args.slug),
  );

  @tracked editing = false;
  @tracked registry = DEFAULT_REGISTRY;
  @tracked username = '';
  @tracked token = '';
  @tracked saving = false;
  @tracked saved = false;
  @tracked formError: unknown = null;

  @tracked confirmingRemove = false;
  @tracked removing = false;
  @tracked removeError: unknown = null;

  constructor(owner: Owner, args: RegistryCredentialSignature['Args']) {
    super(owner, args);
    void this.credential.load();
  }

  get configured(): boolean {
    return this.credential.value?.configured ?? false;
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

  get tokenHint(): string {
    return this.configured
      ? '変えないときは空のままにします。保存した値は二度と表示されません。'
      : '保存した値は暗号化して保管し、二度と表示されません。';
  }

  startEdit = () => {
    this.saved = false;
    this.removeError = null;
    // Saving needs a recent login; ask before the form, not after it.
    if (!this.session.recentlyAuthenticated) {
      this.session.promptReauth();
      return;
    }
    const current = this.credential.value;
    this.registry = current?.registry || DEFAULT_REGISTRY;
    this.username = current?.username ?? '';
    this.token = '';
    this.formError = null;
    this.editing = true;
  };

  cancelEdit = () => {
    if (this.saving) return;
    this.editing = false;
    this.formError = null;
  };

  setRegistry = (event: Event) => {
    this.registry = inputValue(event);
  };

  setUsername = (event: Event) => {
    this.username = inputValue(event);
  };

  setToken = (event: Event) => {
    this.token = inputValue(event);
  };

  save = async (event: Event) => {
    event.preventDefault();
    if (this.saving) return;
    this.saving = true;
    this.formError = null;
    try {
      const saved = await this.api.putRegistryCredential(this.args.slug, {
        registry: this.registry.trim(),
        username: this.username.trim(),
        token: this.token.trim(),
      });
      this.credential.set(saved);
      this.token = '';
      this.editing = false;
      this.saved = true;
    } catch (error) {
      if (error instanceof ApiError && error.code === 'reauth_required') {
        // The re-login dialog takes over.
        this.editing = false;
      } else {
        this.formError = error;
      }
    } finally {
      this.saving = false;
    }
  };

  requestRemove = () => {
    this.saved = false;
    this.removeError = null;
    if (!this.session.recentlyAuthenticated) {
      this.session.promptReauth();
      return;
    }
    this.confirmingRemove = true;
  };

  cancelRemove = () => {
    this.confirmingRemove = false;
  };

  remove = async () => {
    this.removing = true;
    try {
      this.credential.set(
        await this.api.deleteRegistryCredential(this.args.slug),
      );
    } catch (error) {
      if (!(error instanceof ApiError && error.code === 'reauth_required')) {
        this.removeError = error;
      }
    } finally {
      this.removing = false;
      this.confirmingRemove = false;
    }
  };

  <template>
    <section
      class="pane settings-registry"
      aria-labelledby="registry-title"
      data-test-registry
      ...attributes
    >
      <div class="pane-header">
        <h2 class="pane-title" id="registry-title">コンテナレジストリの認証</h2>
        {{#if @canEdit}}
          {{#if this.credential.loaded}}
            {{#unless this.editing}}
              <div class="registry-actions">
                {{#if this.configured}}
                  <Button
                    data-test-registry-edit
                    {{on "click" this.startEdit}}
                  >変更…</Button>
                  <Button
                    class="button-danger-outline"
                    data-test-registry-remove
                    {{on "click" this.requestRemove}}
                  >削除…</Button>
                {{else}}
                  <Button
                    data-test-registry-edit
                    {{on "click" this.startEdit}}
                  >設定する…</Button>
                {{/if}}
              </div>
            {{/unless}}
          {{/if}}
        {{/if}}
      </div>
      <p class="pane-description">
        private なイメージ（CI
        がビルドしてレジストリに置いたイメージや、Dockerfile の
        <code>FROM</code>
        に書いたベースイメージ）を取得するためのログイン情報です。このアプリのビルド・pull・起動にだけ使い、ほかのアプリやホストの
        <code>docker login</code>
        とは混ざりません。
      </p>

      <div class="pane-notices" aria-live="polite">
        <ErrorMessage @error={{this.removeError}} />
        {{#if this.saved}}
          <p class="form-saved" data-test-registry-saved>
            <Icon @name="check" />
            保存しました。次のデプロイや起動から使います。
          </p>
        {{/if}}
      </div>

      <AsyncView
        @data={{this.credential}}
        @errorTitle="コンテナレジストリの認証を取得できませんでした"
      >
        <:default as |credential|>
          {{#if this.editing}}
            <form
              class="registry-form"
              novalidate
              data-test-registry-form
              {{on "submit" this.save}}
            >
              <div class="registry-fields">
                <FormField
                  @label="レジストリ"
                  @hint="ホスト名（必要ならポート）。GitHub Container Registry は ghcr.io、Docker Hub は docker.io"
                  @error={{this.fieldErrors.registry}}
                  data-test-field-group="registry"
                  as |field|
                >
                  <input
                    id={{field.id}}
                    type="text"
                    class="mono"
                    autocomplete="off"
                    spellcheck="false"
                    required
                    value={{this.registry}}
                    aria-describedby={{field.describedBy}}
                    aria-invalid={{field.invalid}}
                    data-test-registry-field="registry"
                    {{on "input" this.setRegistry}}
                  />
                </FormField>
                <FormField
                  @label="ユーザー名"
                  @hint="GHCR なら、トークンを発行した GitHub アカウントの名前"
                  @error={{this.fieldErrors.username}}
                  data-test-field-group="username"
                  as |field|
                >
                  <input
                    id={{field.id}}
                    type="text"
                    autocomplete="off"
                    spellcheck="false"
                    required
                    value={{this.username}}
                    aria-describedby={{field.describedBy}}
                    aria-invalid={{field.invalid}}
                    data-test-registry-field="username"
                    {{on "input" this.setUsername}}
                  />
                </FormField>
                <FormField
                  @label="トークン"
                  @hint={{this.tokenHint}}
                  @error={{this.fieldErrors.token}}
                  data-test-field-group="token"
                  as |field|
                >
                  <input
                    id={{field.id}}
                    type="password"
                    class="mono"
                    autocomplete="new-password"
                    spellcheck="false"
                    placeholder={{if this.configured "（変更しない）"}}
                    value={{this.token}}
                    aria-describedby={{field.describedBy}}
                    data-test-registry-field="token"
                    {{on "input" this.setToken}}
                  />
                </FormField>
              </div>
              <p class="registry-advice" data-test-registry-advice>
                トークンは、読み取りだけができるものにしてください。GHCR なら
                <code>read:packages</code>
                だけを付けた classic な Personal access token
                か、読み取り専用のボットアカウントのトークンを勧めます。期限が切れたら、ここで新しい値に変えます。
              </p>
              {{#if this.formMessage}}
                <p class="field-error" role="alert" data-test-registry-error>
                  {{this.formMessage}}
                </p>
              {{/if}}
              <div class="registry-form-actions">
                <Button
                  @disabled={{this.saving}}
                  data-test-registry-cancel
                  {{on "click" this.cancelEdit}}
                >キャンセル</Button>
                <Button
                  @type="submit"
                  @variant="primary"
                  @busy={{this.saving}}
                  @busyLabel="保存中…"
                  data-test-registry-save
                >保存</Button>
              </div>
            </form>
          {{else if credential.configured}}
            <dl class="readout" data-test-registry-readout>
              <dt>レジストリ</dt>
              <dd
                class="mono"
                data-test-registry-host
              >{{credential.registry}}</dd>
              <dt>ユーザー名</dt>
              <dd data-test-registry-username>{{credential.username}}</dd>
              <dt>トークン</dt>
              <dd data-test-registry-token>
                設定済み
                <span class="readout-note">（値は表示しません）</span>
              </dd>
              <dt>更新</dt>
              <dd data-test-registry-updated>
                <RelativeTime @time={{credential.updated_at}} />
              </dd>
            </dl>
          {{else}}
            <p class="registry-empty" data-test-registry-empty>
              設定していません。public なイメージと、ホストの root で
              <code>docker login</code>
              したレジストリのイメージだけを取得できます。
            </p>
          {{/if}}
        </:default>
      </AsyncView>
    </section>

    {{#if this.confirmingRemove}}
      <ConfirmDialog
        @title="コンテナレジストリの認証を削除しますか？"
        @confirmLabel="削除する"
        @danger={{true}}
        @busy={{this.removing}}
        @onConfirm={{this.remove}}
        @onCancel={{this.cancelRemove}}
      >
        <p>次のデプロイや起動から、このアプリは private
          なイメージを取得できなくなります。動いているコンテナは止まりません。レジストリ側のトークンは失効しないので、要らなければ発行元で削除してください。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
