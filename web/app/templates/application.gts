import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { LinkTo } from '@ember/routing';
import { pageTitle } from 'ember-page-title';
import ErrorMessage from 'paas-console/components/error-message';
import type SessionService from 'paas-console/services/session';

export default class ApplicationTemplate extends Component {
  @service declare session: SessionService;

  @tracked loggingOut = false;
  @tracked logoutError: unknown = null;

  logout = async () => {
    this.loggingOut = true;
    this.logoutError = null;
    try {
      await this.session.logout();
    } catch (error) {
      this.logoutError = error;
    } finally {
      this.loggingOut = false;
    }
  };

  <template>
    {{pageTitle "PaaS コンソール"}}

    <header class="app-header">
      <h1 class="app-header-title">
        <LinkTo @route="index">PaaS コンソール</LinkTo>
      </h1>

      {{#if this.session.authenticated}}
        <nav class="app-nav" aria-label="メインメニュー">
          <LinkTo
            @route="index"
            @current-when="index new-app app"
            class="nav-link"
            data-test-nav="apps"
          >アプリ</LinkTo>
          <LinkTo
            @route="system"
            class="nav-link"
            data-test-nav="system"
          >システム</LinkTo>
        </nav>

        <div class="app-user">
          {{#if this.session.user}}
            {{#if this.session.user.avatar_url}}
              <img
                class="avatar"
                src={{this.session.user.avatar_url}}
                alt=""
                width="24"
                height="24"
              />
            {{/if}}
            <span data-test-user>{{this.session.user.login}}</span>
            <button
              type="button"
              class="button button-small"
              disabled={{this.loggingOut}}
              data-test-logout
              {{on "click" this.logout}}
            >ログアウト</button>
          {{/if}}
        </div>
      {{/if}}
    </header>

    <main class="app-main">
      <ErrorMessage @error={{this.logoutError}} />
      {{outlet}}
    </main>
  </template>
}
