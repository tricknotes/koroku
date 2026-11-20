import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { on } from '@ember/modifier';
import { guidFor } from '@ember/object/internals';
import { LinkTo } from '@ember/routing';
import onDismiss, {
  type DismissReason,
} from 'koroku-console/modifiers/on-dismiss';
import type { SessionUser } from 'koroku-console/utils/types';
import Icon from './icon';

interface UserMenuSignature {
  Element: HTMLDivElement;
  Args: {
    user: SessionUser;
    /** The owner also manages the members. */
    owner?: boolean;
    /** The route on screen, to mark the menu item that leads there. */
    currentRoute?: string;
    /** Disables "log out" while the request runs. */
    busy?: boolean;
    /** The security page is the one on screen. */
    current?: boolean;
    onLogout: () => unknown;
  };
}

function initial(login: string): string {
  return login.slice(0, 1);
}

function currentIf(
  route: string | undefined,
  name: string,
): string | undefined {
  return route === name ? 'page' : undefined;
}

/**
 * Who is logged in, and the way out. A button that discloses a small panel;
 * it closes on Escape (focus returns to the button) or a press outside.
 */
export default class UserMenu extends Component<UserMenuSignature> {
  @tracked open = false;

  panelId = `menu-${guidFor(this)}`;
  trigger: HTMLButtonElement | null = null;

  toggle = (event: Event) => {
    this.trigger = event.currentTarget as HTMLButtonElement;
    this.open = !this.open;
  };

  close = () => {
    this.open = false;
  };

  dismiss = (reason: DismissReason) => {
    if (!this.open) return;
    this.open = false;
    if (reason === 'escape') this.trigger?.focus();
  };

  <template>
    <div class="menu" {{onDismiss this.dismiss}} ...attributes>
      <button
        type="button"
        class="menu-trigger"
        aria-expanded={{if this.open "true" "false"}}
        aria-controls={{this.panelId}}
        aria-label="ユーザーメニュー: {{@user.login}}"
        data-test-user-menu
        {{on "click" this.toggle}}
      >
        <span class="avatar">
          {{#if @user.avatar_url}}
            <img src={{@user.avatar_url}} alt="" />
          {{else}}
            {{initial @user.login}}
          {{/if}}
        </span>
        <span class="menu-trigger-label" data-test-user>{{@user.login}}</span>
        <Icon @name="chevron-down" />
      </button>

      {{#if this.open}}
        <div class="menu-panel" id={{this.panelId}}>
          <p class="menu-heading">
            ログイン中
            <strong>{{@user.login}}</strong>
            <span class="menu-heading-role" data-test-menu-role>{{if
                @owner
                "オーナー"
                "メンバー"
              }}</span>
          </p>
          <hr class="menu-separator" />
          {{#if @owner}}
            <LinkTo
              @route="members"
              class="menu-item"
              aria-current={{currentIf @currentRoute "members"}}
              data-test-menu-members
              {{on "click" this.close}}
            >メンバー</LinkTo>
          {{/if}}
          <LinkTo
            @route="security"
            class="menu-item"
            aria-current={{if @current "page"}}
            data-test-menu-security
            {{on "click" this.close}}
          >セキュリティ</LinkTo>
          <button
            type="button"
            class="menu-item"
            disabled={{@busy}}
            data-test-logout
            {{on "click" @onLogout}}
          >ログアウト</button>
        </div>
      {{/if}}
    </div>
  </template>
}
