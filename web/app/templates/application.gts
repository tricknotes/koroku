import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { LinkTo } from '@ember/routing';
import type RouterService from '@ember/routing/router-service';
import { pageTitle } from 'ember-page-title';
import ErrorMessage from 'koroku-console/components/error-message';
import Icon from 'koroku-console/components/icon';
import ReauthDialog from 'koroku-console/components/reauth-dialog';
import ThemeSwitch from 'koroku-console/components/theme-switch';
import UserMenu from 'koroku-console/components/user-menu';
import type SessionService from 'koroku-console/services/session';

/**
 * The global shell: one top bar (wordmark and the two sections on the left,
 * theme switch and user menu on the right) above the centred content.
 */
export default class ApplicationTemplate extends Component {
  @service declare session: SessionService;
  @service declare router: RouterService;

  @tracked loggingOut = false;
  @tracked logoutError: unknown = null;

  /**
   * The section the current screen belongs to. `<LinkTo>` only adds the
   * `active` class; assistive technology needs `aria-current`.
   */
  get section(): 'apps' | 'system' | null {
    const route = this.router.currentRouteName ?? '';
    if (route === 'system') return 'system';
    if (/^(index|new-app|app)(\.|$)/.test(route)) return 'apps';
    return null;
  }

  get appsCurrent(): 'page' | undefined {
    return this.section === 'apps' ? 'page' : undefined;
  }

  /** The system and members screens are the owner's (ARCHITECTURE 10.7). */
  get isOwner(): boolean {
    return this.session.isOwner;
  }

  get currentRoute(): string | undefined {
    return this.router.currentRouteName ?? undefined;
  }

  get systemCurrent(): 'page' | undefined {
    return this.section === 'system' ? 'page' : undefined;
  }

  /** The security page lives in the user menu, not the main navigation. */
  get securityCurrent(): boolean {
    return this.router.currentRouteName === 'security';
  }

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
              aria-current={{this.appsCurrent}}
              data-test-nav="apps"
            >アプリ</LinkTo>
            {{#if this.isOwner}}
              <LinkTo
                @route="system"
                class="nav-link"
                aria-current={{this.systemCurrent}}
                data-test-nav="system"
              >システム</LinkTo>
            {{/if}}
          </nav>
        {{/if}}

        <div class="topbar-end">
          <ThemeSwitch />
          {{#if this.session.authenticated}}
            {{#if this.session.user}}
              <UserMenu
                @user={{this.session.user}}
                @owner={{this.isOwner}}
                @currentRoute={{this.currentRoute}}
                @current={{this.securityCurrent}}
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

    <ReauthDialog />
  </template>
}
