import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { fn } from '@ember/helper';
import { on } from '@ember/modifier';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import type ApiService from 'koroku-console/services/api';
import { setValue } from 'koroku-console/utils/dom';
import { errorMessage, fieldErrors } from 'koroku-console/utils/errors';
import {
  groupMembers,
  isEmptyGroup,
  linkLabel,
  memberLinks,
  memberServices,
} from 'koroku-console/utils/groups';
import Loadable from 'koroku-console/utils/loadable';
import type { Environment, Group } from 'koroku-console/utils/types';
import AsyncView from './async-view';
import Banner from './banner';
import Button from './button';
import ConfirmDialog from './confirm-dialog';
import EmptyState from './empty-state';
import EnvironmentBadge from './environment-badge';
import ErrorMessage from './error-message';
import FormField from './form-field';

/** Mirrors the API: 1 to 40 characters once trimmed. */
const NAME_MAX = 40;

function hasMembers(group: Group): boolean {
  return !isEmptyGroup(group);
}

function isChecked(current: Environment, value: Environment): boolean {
  return current === value;
}

function joined(names: string[]): string {
  return names.join('、');
}

function noteId(group: Group): string {
  return `group-${group.id}-delete-note`;
}

/** The name as the API will store it, or the reason it is refused. */
function checkName(raw: string): { name: string; error: string | null } {
  const name = raw.trim();
  if (name === '') return { name, error: '名前を入力してください。' };
  if ([...name].length > NAME_MAX) {
    return { name, error: `${NAME_MAX} 文字以内で入力してください。` };
  }
  return { name, error: null };
}

/** The draft of the create and rename dialogs. */
class GroupDraft {
  @tracked name: string;
  @tracked environment: Environment = 'staging';

  constructor(name = '') {
    this.name = name;
  }
}

/**
 * The groups of the host on the system screen: each with its environment,
 * and per member the services it exposes and the links it made. Groups are
 * made, renamed and removed here; apps join them, expose services and
 * make links on their settings tab.
 */
export default class GroupList extends Component {
  @service declare api: ApiService;

  groups = new Loadable<Group[]>(() => this.api.listGroups());

  @tracked creating = false;
  /** The group being renamed or deleted. */
  @tracked renaming: Group | null = null;
  @tracked deleting: Group | null = null;
  @tracked draft = new GroupDraft();

  @tracked busy = false;
  /** A failure inside the open dialog: field errors or a banner. */
  @tracked dialogError: unknown = null;
  @tracked dialogErrors: Record<string, string> = {};
  /** What happened last, shown above the list. */
  @tracked notice = '';
  @tracked error: unknown = null;

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.groups.load();
  }

  /** The dialog error that has no field of its own to sit next to. */
  get dialogBanner(): unknown {
    return Object.keys(this.dialogErrors).length > 0 ? null : this.dialogError;
  }

  private resetDialog(): void {
    this.dialogError = null;
    this.dialogErrors = {};
    this.notice = '';
    this.error = null;
  }

  setEnvironment = (environment: Environment) => {
    this.draft.environment = environment;
  };

  openCreate = () => {
    this.resetDialog();
    this.draft = new GroupDraft();
    this.creating = true;
  };

  openRename = (group: Group) => {
    this.resetDialog();
    this.draft = new GroupDraft(group.name);
    this.renaming = group;
  };

  openDelete = (group: Group) => {
    this.resetDialog();
    this.deleting = group;
  };

  close = () => {
    this.creating = false;
    this.renaming = null;
    this.deleting = null;
  };

  create = async () => {
    const { name, error } = checkName(this.draft.name);
    if (error) {
      this.dialogErrors = { name: error };
      return;
    }
    await this.run(async () => {
      const group = await this.api.createGroup({
        name,
        environment: this.draft.environment,
      });
      this.notice = `グループ「${group.name}」を作成しました。入れるアプリの設定タブで、このグループを選んでください。`;
    });
  };

  rename = async () => {
    const group = this.renaming;
    if (!group) return;
    const { name, error } = checkName(this.draft.name);
    if (error) {
      this.dialogErrors = { name: error };
      return;
    }
    if (name === group.name) {
      this.close();
      return;
    }
    await this.run(async () => {
      const renamed = await this.api.renameGroup(group.id, name);
      this.notice = `グループの名前を「${renamed.name}」に変えました。`;
    });
  };

  remove = async () => {
    const group = this.deleting;
    if (!group) return;
    this.busy = true;
    try {
      await this.api.deleteGroup(group.id);
      this.notice = `グループ「${group.name}」を削除しました。`;
    } catch (error) {
      // Shown above the list: the dialog has nothing left to offer.
      this.error = error;
    } finally {
      this.busy = false;
      this.close();
    }
    await this.groups.load({ quiet: true });
  };

  /** Runs a create or rename; a refusal keeps the dialog open. */
  private async run(action: () => Promise<void>): Promise<void> {
    this.busy = true;
    this.dialogError = null;
    this.dialogErrors = {};
    try {
      await action();
      this.close();
    } catch (error) {
      this.dialogError = error;
      this.dialogErrors = fieldErrors(error);
      return;
    } finally {
      this.busy = false;
    }
    await this.groups.load({ quiet: true });
  }

  <template>
    {{! A backend without groups: the section is left out. }}
    {{#unless this.groups.unavailable}}
      <section class="pane" aria-labelledby="groups-title" data-test-groups>
        <div class="pane-header">
          <h2 class="pane-title" id="groups-title">グループ</h2>
          {{#if this.groups.value.length}}
            <Button data-test-group-create {{on "click" this.openCreate}}>
              グループを作成
            </Button>
          {{/if}}
        </div>
        <p class="pane-description">
          グループは、接続してよいアプリの範囲です。同じグループのアプリは、ほかのアプリが公開するサービスを、自分の
          Compose
          で決めた名前で呼べます（接続）。所属、公開するサービス、接続は、各アプリの設定タブで決めます。
        </p>

        <div class="pane-notices" aria-live="polite">
          {{#if this.notice}}
            <Banner data-test-group-notice>{{this.notice}}</Banner>
          {{/if}}
          <ErrorMessage @error={{this.error}} />
        </div>

        <AsyncView
          @data={{this.groups}}
          @errorTitle="グループを取得できませんでした"
        >
          <:default as |groups|>
            {{#if groups.length}}
              <ul class="group-cards" aria-label="グループ">
                {{#each groups key="id" as |group|}}
                  <li class="box group-card" data-test-group={{group.name}}>
                    <div class="group-card-header">
                      <div class="group-card-title">
                        <h3 class="group-name" data-test-group-name>
                          {{group.name}}
                        </h3>
                        <EnvironmentBadge
                          @environment={{group.environment}}
                          data-test-group-environment
                        />
                      </div>
                      <div class="group-actions">
                        <Button
                          @size="small"
                          aria-label="{{group.name}} の名前を変更"
                          data-test-group-rename
                          {{on "click" (fn this.openRename group)}}
                        >名前を変更</Button>
                        <Button
                          @size="small"
                          class="button-danger-outline"
                          @disabled={{hasMembers group}}
                          aria-label="{{group.name}} を削除"
                          aria-describedby={{if
                            (hasMembers group)
                            (noteId group)
                          }}
                          data-test-group-delete
                          {{on "click" (fn this.openDelete group)}}
                        >削除…</Button>
                      </div>
                    </div>

                    {{#if (hasMembers group)}}
                      <ul
                        class="group-members"
                        aria-label="{{group.name}} のメンバー"
                      >
                        {{#each (groupMembers group) key="slug" as |member|}}
                          <li
                            class="group-member"
                            data-test-group-member={{member.slug}}
                          >
                            <LinkTo
                              @route="app.settings"
                              @model={{member.slug}}
                              class="group-member-app"
                            >{{member.slug}}</LinkTo>
                            <dl class="group-member-facts">
                              <dt>公開</dt>
                              <dd data-test-group-exposed>
                                {{#each
                                  (memberServices member)
                                  as |name index|
                                }}{{if index "、"}}<span
                                    class="mono"
                                    data-test-group-exposed-service
                                  >{{name}}</span>{{else}}<span
                                    class="group-member-none"
                                    data-test-group-no-services
                                  >なし</span>{{/each}}
                              </dd>
                              <dt>接続</dt>
                              <dd>
                                {{#let (memberLinks member) as |links|}}
                                  {{#if links.length}}
                                    <ul
                                      class="group-links"
                                      aria-label="{{member.slug}} の接続"
                                    >
                                      {{#each links key="id" as |link|}}
                                        <li data-test-group-link={{link.name}}>
                                          <span class="mono">{{linkLabel
                                              member.slug
                                              link
                                            }}</span>
                                          <span class="group-link-consumers">
                                            （{{joined link.services}}）
                                          </span>
                                        </li>
                                      {{/each}}
                                    </ul>
                                  {{else}}
                                    <span
                                      class="group-member-none"
                                      data-test-group-no-links
                                    >なし</span>
                                  {{/if}}
                                {{/let}}
                              </dd>
                            </dl>
                          </li>
                        {{/each}}
                      </ul>
                      <p
                        class="group-note"
                        id={{noteId group}}
                        data-test-group-delete-note
                      >
                        メンバーがいる間は削除できません。先に各アプリの設定タブでグループから外してください。
                      </p>
                    {{else}}
                      <p class="group-note" data-test-group-empty>
                        メンバーはいません。アプリの設定タブの「グループ」から入れます。
                      </p>
                    {{/if}}
                  </li>
                {{/each}}
              </ul>
            {{else}}
              <EmptyState
                @title="グループはありません"
                @description="アプリどうしを分けたまま、一部のサービスだけを互いに呼べるようにするときに作ります。"
                data-test-groups-empty
              >
                <:action>
                  <Button
                    @variant="primary"
                    data-test-group-create
                    {{on "click" this.openCreate}}
                  >グループを作成</Button>
                </:action>
              </EmptyState>
            {{/if}}
          </:default>
        </AsyncView>
      </section>
    {{/unless}}

    {{#if this.creating}}
      <ConfirmDialog
        @title="グループを作成"
        @confirmLabel="作成する"
        @busy={{this.busy}}
        @onConfirm={{this.create}}
        @onCancel={{this.close}}
      >
        <FormField
          @label="名前"
          @hint="40 文字以内。あとから変えられます。"
          @error={{this.dialogErrors.name}}
          data-test-field-group="group-name"
          as |field|
        >
          <input
            id={{field.id}}
            type="text"
            autocomplete="off"
            spellcheck="false"
            value={{this.draft.name}}
            disabled={{this.busy}}
            aria-describedby={{field.describedBy}}
            aria-invalid={{field.invalid}}
            data-test-group-name-input
            {{on "input" (setValue this.draft "name")}}
          />
        </FormField>
        <fieldset class="option-group dialog-options">
          <legend>環境</legend>
          <label class="option">
            <input
              type="radio"
              name="group-environment"
              value="staging"
              checked={{isChecked this.draft.environment "staging"}}
              disabled={{this.busy}}
              data-test-group-environment-option="staging"
              {{on "change" (fn this.setEnvironment "staging")}}
            />
            <span class="option-body">
              <span class="option-label">ステージング</span>
            </span>
          </label>
          <label class="option">
            <input
              type="radio"
              name="group-environment"
              value="production"
              checked={{isChecked this.draft.environment "production"}}
              disabled={{this.busy}}
              data-test-group-environment-option="production"
              {{on "change" (fn this.setEnvironment "production")}}
            />
            <span class="option-body">
              <span class="option-label">本番</span>
            </span>
          </label>
          <p class="option-hint">
            同じ環境のアプリだけが入れます。ステージングから本番に届く構成を作れないようにするためです。作成後は変えられません。
          </p>
        </fieldset>
        {{#if this.dialogErrors.environment}}
          <p class="field-error" data-test-field-error>
            {{this.dialogErrors.environment}}
          </p>
        {{/if}}
        {{#if this.dialogBanner}}
          <p class="field-error" role="alert" data-test-dialog-error>
            {{errorMessage this.dialogBanner}}
          </p>
        {{/if}}
      </ConfirmDialog>
    {{/if}}

    {{#if this.renaming}}
      <ConfirmDialog
        @title="グループの名前を変更"
        @confirmLabel="変更する"
        @busy={{this.busy}}
        @onConfirm={{this.rename}}
        @onCancel={{this.close}}
      >
        <FormField
          @label="新しい名前"
          @hint="40 文字以内。メンバーと接続は変わりません。"
          @error={{this.dialogErrors.name}}
          data-test-field-group="group-name"
          as |field|
        >
          <input
            id={{field.id}}
            type="text"
            autocomplete="off"
            spellcheck="false"
            value={{this.draft.name}}
            disabled={{this.busy}}
            aria-describedby={{field.describedBy}}
            aria-invalid={{field.invalid}}
            data-test-group-name-input
            {{on "input" (setValue this.draft "name")}}
          />
        </FormField>
        {{#if this.dialogBanner}}
          <p class="field-error" role="alert" data-test-dialog-error>
            {{errorMessage this.dialogBanner}}
          </p>
        {{/if}}
      </ConfirmDialog>
    {{/if}}

    {{#if this.deleting}}
      <ConfirmDialog
        @title="グループを削除しますか？"
        @confirmLabel="削除する"
        @danger={{true}}
        @busy={{this.busy}}
        @onConfirm={{this.remove}}
        @onCancel={{this.close}}
      >
        <p>
          グループ
          <strong>{{this.deleting.name}}</strong>
          を削除します。メンバーはいないので、切断される接続はありません。
        </p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
