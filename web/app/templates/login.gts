import Component from '@glimmer/component';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { pageTitle } from 'ember-page-title';
import type BrowserService from 'paas-console/services/browser';
import type SessionService from 'paas-console/services/session';
import { errorMessage, loginErrorMessage } from 'paas-console/utils/errors';

export default class LoginTemplate extends Component {
  @service declare session: SessionService;
  @service declare browser: BrowserService;

  get loginError(): string {
    return loginErrorMessage(this.session.loginError);
  }

  get loadError(): string {
    return this.session.loadError ? errorMessage(this.session.loadError) : '';
  }

  reload = () => this.browser.reload();

  <template>
    {{pageTitle "ログイン"}}

    <section class="login card" aria-labelledby="login-title">
      <h2 id="login-title">ログイン</h2>

      {{#if this.loadError}}
        <p class="alert alert-danger" role="alert" data-test-session-error>
          ログイン状態を確認できませんでした。{{this.loadError}}
        </p>
        <button type="button" class="button" {{on "click" this.reload}}>
          再読み込み
        </button>
      {{else}}
        {{#if this.loginError}}
          <p class="alert alert-danger" role="alert" data-test-login-error>
            {{this.loginError}}
          </p>
        {{/if}}

        <p>このコンソールを使うには、許可された GitHub
          アカウントでログインしてください。</p>
        <a
          class="button button-primary"
          href="/auth/github/login"
          data-test-login-link
        >GitHub でログイン</a>
      {{/if}}
    </section>
  </template>
}
