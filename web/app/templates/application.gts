import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { LinkTo } from '@ember/routing';
import { pageTitle } from 'ember-page-title';
import ErrorMessage from 'koroku-console/components/error-message';
import Icon from 'koroku-console/components/icon';
import ThemeSwitch from 'koroku-console/components/theme-switch';
import UserMenu from 'koroku-console/components/user-menu';
import type SessionService from 'koroku-console/services/session';

/**
 * The global shell: one top bar (wordmark and the two sections on the left,
 * theme switch and user menu on the right) above the centred content.
 */
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
    {{pageTitle "koroku"}}

    <header class="topbar">
      <div class="topbar-inner">
        <LinkTo @route="index" class="wordmark" data-test-wordmark>
          <Icon @name="logo" />
          koroku
        </LinkTo>

        {{#if this.session.authenticated}}
          <nav class="topbar-nav" aria-label="メインメニュー">
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
        {{/if}}

        <div class="topbar-end">
          <ThemeSwitch />
          {{#if this.session.authenticated}}
            {{#if this.session.user}}
              <UserMenu
                @user={{this.session.user}}
                @busy={{this.loggingOut}}
                @onLogout={{this.logout}}
              />
            {{/if}}
          {{/if}}
        </div>
      </div>
    </header>

    <main class="app-main">
      <ErrorMessage @error={{this.logoutError}} />
      {{outlet}}
    </main>
  </template>
}
