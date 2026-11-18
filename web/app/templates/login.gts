import Component from '@glimmer/component';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { pageTitle } from 'ember-page-title';
import Banner from 'paas-console/components/banner';
import Button, { buttonClass } from 'paas-console/components/button';
import Icon from 'paas-console/components/icon';
import type BrowserService from 'paas-console/services/browser';
import type SessionService from 'paas-console/services/session';
import { errorMessage, loginErrorMessage } from 'paas-console/utils/errors';

export default class LoginTemplate extends Component {
  @service declare session: SessionService;
  @service declare browser: BrowserService;

  get loginError(): string {
    return loginErrorMessage(this.session.loginError);
  }

  /** GitHub knew the account, but it is not the one this console allows. */
  get notAllowed(): boolean {
    return this.session.loginError === 'not_allowed';
  }

  get loadError(): string {
    return this.session.loadError ? errorMessage(this.session.loadError) : '';
  }

  reload = () => this.browser.reload();

  <template>
    {{pageTitle "ログイン"}}

    <section class="login" aria-labelledby="login-title">
      <Icon @name="logo" class="login-logo" />
      <h1 class="login-title" id="login-title">paasd にログイン</h1>

      {{#if this.loadError}}
        <Banner
          @tone="error"
          @title="ログイン状態を確認できませんでした"
          data-test-session-error
        >
          {{this.loadError}}
        </Banner>
        <Button class="login-button" {{on "click" this.reload}}>
          再読み込み
        </Button>
      {{else}}
        {{#if this.loginError}}
          <Banner @tone="error" data-test-login-error>
            {{this.loginError}}
          </Banner>
          {{#if this.notAllowed}}
            <p class="login-lead" data-test-login-hint>
              別のアカウントで入るには、GitHub
              でログアウトしてからやり直してください。
            </p>
          {{/if}}
        {{else}}
          <p class="login-lead">
            許可された GitHub アカウントだけが、このコンソールを使えます。
          </p>
        {{/if}}

        <a
          class="{{buttonClass 'primary'}} login-button"
          href="/auth/github/login"
          data-test-login-link
        >GitHub でログイン</a>
      {{/if}}
    </section>
  </template>
}
