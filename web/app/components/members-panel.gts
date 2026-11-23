import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn, get } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from 'koroku-console/components/async-view';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import EmptyState from 'koroku-console/components/empty-state';
import EnvironmentBadge from 'koroku-console/components/environment-badge';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import RelativeTime from 'koroku-console/components/relative-time';
import type ApiService from 'koroku-console/services/api';
import type SessionService from 'koroku-console/services/session';
import { preventDefault, setValue } from 'koroku-console/utils/dom';
import { ApiError, errorMessage } from 'koroku-console/utils/errors';
import Loadable from 'koroku-console/utils/loadable';
import { MEMBER_ROLES, ROLE_HINTS } from 'koroku-console/utils/roles';
import type { App, Member, MemberRole } from 'koroku-console/utils/types';

interface MembersData {
  members: Member[];
  apps: App[];
}

function roleOf(member: Member, slug: string): MemberRole | '' {
  return member.roles.find((r) => r.app === slug)?.role ?? '';
}

function isRole(value: string): value is MemberRole {
  return MEMBER_ROLES.some((r) => r.value === value);
}

function initial(login: string): string {
  return login.slice(0, 1);
}

function selected(a: string, b: string): boolean {
  return a === b;
}

/** Login errors shown under the field rather than above the list. */
const LOGIN_ERRORS = new Set([
  'user_not_found',
  'member_exists',
  'is_owner',
  'validation_failed',
]);

/**
 * The owner's part of the system screen for letting GitHub users into some
 * apps, with one role per app (ARCHITECTURE 9.1, 10.7). Every change is
 * saved at once; there is no form to submit.
 */
export default class MembersPanel extends Component {
  @service declare api: ApiService;
  @service declare session: SessionService;

  data = new Loadable<MembersData>(async () => {
    const [members, apps] = await Promise.all([
      this.api.listMembers(),
      this.api.listApps(),
    ]);
    // A review app's roles are its parent's (ARCHITECTURE 9.9).
    return { members, apps: apps.filter((app) => !app.review) };
  });

  @tracked login = '';
  @tracked adding = false;
  @tracked addError: unknown = null;
  @tracked added: string | null = null;
  /** `<user id>:<slug>` of the role being saved. */
  @tracked saving: string | null = null;
  @tracked actionError: unknown = null;
  @tracked removing: Member | null = null;
  @tracked removeBusy = false;

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.data.load();
  }

  get loginError(): string | null {
    const error = this.addError;
    if (error instanceof ApiError && LOGIN_ERRORS.has(error.code)) {
      return error.code === 'validation_failed'
        ? 'GitHub のユーザー名（英数字とハイフン）を入れてください。'
        : errorMessage(error);
    }
    return null;
  }

  /** Errors that are not about the login itself. */
  get otherAddError(): unknown {
    return this.loginError ? null : this.addError;
  }

  roleKey = (member: Member, slug: string) =>
    `${member.github_user_id}:${slug}`;

  isSaving = (member: Member, slug: string): boolean =>
    this.saving === this.roleKey(member, slug);

  /** Replaces one member in the list with what the server returned. */
  private put(member: Member): void {
    const data = this.data.value;
    if (!data) return;
    this.data.set({
      ...data,
      members: data.members.map((m) =>
        m.github_user_id === member.github_user_id ? member : m,
      ),
    });
  }

  add = async () => {
    const login = this.login.trim().replace(/^@/, '');
    if (!login || this.adding) return;
    this.adding = true;
    this.addError = null;
    this.added = null;
    try {
      const member = await this.api.addMember(login);
      const data = this.data.value;
      if (data) this.data.set({ ...data, members: [...data.members, member] });
      this.login = '';
      this.added = member.login;
    } catch (error) {
      this.addError = error;
    } finally {
      this.adding = false;
    }
  };

  setRole = async (member: Member, slug: string, event: Event) => {
    const value = (event.target as HTMLSelectElement).value;
    this.saving = this.roleKey(member, slug);
    this.actionError = null;
    this.added = null;
    try {
      this.put(
        await this.api.setMemberRole(
          member.github_user_id,
          slug,
          isRole(value) ? value : null,
        ),
      );
    } catch (error) {
      this.actionError = error;
      // Put the select back to what the server has.
      await this.data.load({ quiet: true });
    } finally {
      this.saving = null;
    }
  };

  requestRemove = (member: Member) => {
    this.removing = member;
  };

  cancelRemove = () => {
    this.removing = null;
  };

  remove = async () => {
    const member = this.removing;
    if (!member) return;
    this.removeBusy = true;
    this.actionError = null;
    try {
      await this.api.removeMember(member.github_user_id);
      const data = this.data.value;
      if (data) {
        this.data.set({
          ...data,
          members: data.members.filter(
            (m) => m.github_user_id !== member.github_user_id,
          ),
        });
      }
    } catch (error) {
      this.actionError = error;
    } finally {
      this.removeBusy = false;
      this.removing = null;
    }
  };

  <template>
    <section
      class="pane members"
      id="members"
      aria-labelledby="members-section-title"
      data-test-members-section
    >
      <div class="pane-header">
        <h2 class="pane-title" id="members-section-title">メンバー</h2>
      </div>
      <p class="pane-description members-lead">
        GitHub
        のユーザーをメンバーに加え、アプリごとにロールを与えます。メンバーはロールのあるアプリだけを見て操作でき、ロールが
        1
        つもないとログインできません。アプリの作成と削除、システム画面、メンバーの管理はオーナー（{{this.session.user.login}}）だけができます。
      </p>

      <dl class="role-legend" data-test-role-legend>
        {{#each MEMBER_ROLES as |role|}}
          <div>
            <dt>{{role.label}}</dt>
            <dd>{{get ROLE_HINTS role.value}}</dd>
          </div>
        {{/each}}
      </dl>

      <section class="members-part" aria-labelledby="member-add-title">
        <h3 class="members-part-title" id="member-add-title">メンバーを追加</h3>
        <form
          class="member-form"
          novalidate
          data-test-member-form
          {{on "submit" (preventDefault this.add)}}
        >
          <FormField
            @label="GitHub のユーザー名"
            @hint="GitHub で数値の ID を調べて保存します（ユーザー名を変えられても同じ人として扱います）。追加したあとで、下の一覧でロールを与えます。"
            @error={{this.loginError}}
            as |field|
          >
            <div class="member-form-line">
              <input
                id={{field.id}}
                type="text"
                class="mono"
                placeholder="octocat"
                spellcheck="false"
                autocapitalize="off"
                autocomplete="off"
                value={{this.login}}
                aria-invalid={{field.invalid}}
                aria-describedby={{field.describedBy}}
                data-test-member-login
                {{on "input" (setValue this "login")}}
              />
              <Button
                @type="submit"
                @variant="primary"
                @busy={{this.adding}}
                @busyLabel="追加中…"
                data-test-member-add
              >追加</Button>
            </div>
          </FormField>
        </form>
        <div class="pane-notices" aria-live="polite">
          {{#if this.added}}
            <Banner data-test-member-added>
              {{this.added}}
              を追加しました。ロールを与えるとログインできるようになります。
            </Banner>
          {{/if}}
          <ErrorMessage @error={{this.otherAddError}} />
        </div>
      </section>

      <section class="members-part" aria-labelledby="members-title">
        <h3 class="members-part-title" id="members-title">メンバーとロール</h3>
        <div class="pane-notices" aria-live="polite">
          <ErrorMessage @error={{this.actionError}} />
        </div>

        <AsyncView
          @data={{this.data}}
          @errorTitle="メンバーの一覧を取得できませんでした"
        >
          <:default as |data|>
            {{#if data.members.length}}
              <ul class="member-list" aria-label="メンバー" data-test-members>
                {{#each data.members key="github_user_id" as |member|}}
                  <li class="member box" data-test-member={{member.login}}>
                    <div class="member-head">
                      <span class="avatar member-avatar">
                        {{#if member.avatar_url}}
                          <img src={{member.avatar_url}} alt="" />
                        {{else}}
                          {{initial member.login}}
                        {{/if}}
                      </span>
                      <div class="member-name">
                        <strong data-test-member-name>{{member.login}}</strong>
                        <span class="member-meta">
                          ID
                          <span class="mono">{{member.github_user_id}}</span>
                          ·
                          <RelativeTime @time={{member.added_at}} />
                          に追加
                        </span>
                      </div>
                      <Button
                        @size="small"
                        aria-label="{{member.login}} を外す"
                        data-test-member-remove
                        {{on "click" (fn this.requestRemove member)}}
                      >外す…</Button>
                    </div>
                    {{#unless member.roles.length}}
                      <p
                        class="member-no-role"
                        data-test-member-no-role
                      >ロールがないためログインできません。下でロールを与えてください。</p>
                    {{/unless}}
                    {{#if data.apps.length}}
                      <ul
                        class="member-roles"
                        aria-label="{{member.login}} のロール"
                      >
                        {{#each data.apps key="slug" as |app|}}
                          <li
                            class="member-role"
                            data-test-member-role={{app.slug}}
                          >
                            <label
                              class="member-role-app"
                              for="role-{{member.github_user_id}}-{{app.slug}}"
                            >
                              <span class="mono">{{app.slug}}</span>
                              <EnvironmentBadge
                                @environment={{app.environment}}
                              />
                            </label>
                            <select
                              id="role-{{member.github_user_id}}-{{app.slug}}"
                              disabled={{this.isSaving member app.slug}}
                              data-test-member-role-select
                              {{on "change" (fn this.setRole member app.slug)}}
                            >
                              <option
                                value=""
                                selected={{selected
                                  (roleOf member app.slug)
                                  ""
                                }}
                              >なし</option>
                              {{#each MEMBER_ROLES as |role|}}
                                <option
                                  value={{role.value}}
                                  selected={{selected
                                    (roleOf member app.slug)
                                    role.value
                                  }}
                                >{{role.label}}</option>
                              {{/each}}
                            </select>
                          </li>
                        {{/each}}
                      </ul>
                    {{else}}
                      <p class="member-meta">アプリがまだありません。</p>
                    {{/if}}
                  </li>
                {{/each}}
              </ul>
            {{else}}
              <EmptyState
                @title="メンバーはいません"
                @description="ほかの人にデプロイやログの確認を任せるときに、GitHub のユーザー名で追加します。"
                data-test-no-members
              />
            {{/if}}
          </:default>
        </AsyncView>
      </section>

    </section>

    {{#if this.removing}}
      <ConfirmDialog
        @title="{{this.removing.login}} を外しますか？"
        @confirmLabel="外す"
        @danger={{true}}
        @busy={{this.removeBusy}}
        @onConfirm={{this.remove}}
        @onCancel={{this.cancelRemove}}
      >
        <p>すべてのアプリのロールを外し、ログイン中のセッションと API
          トークンも無効にします。もう一度加えるときは、ロールを与え直します。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
