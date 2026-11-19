import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { fn } from '@ember/helper';
import { on } from '@ember/modifier';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import type ApiService from 'koroku-console/services/api';
import type AppContext from 'koroku-console/utils/app-context';
import { inputChecked, preventDefault } from 'koroku-console/utils/dom';
import { fieldErrors } from 'koroku-console/utils/errors';
import {
  DNS_LABEL,
  appGroupId,
  appGroupServices,
  groupMembers,
  joinBlocker,
  looksLikeDatabase,
  memberLinks,
  removedLinks,
  type RemovedLink,
} from 'koroku-console/utils/groups';
import Loadable from 'koroku-console/utils/loadable';
import { ENVIRONMENT_LABELS } from 'koroku-console/utils/status';
import type {
  App,
  AppLink,
  Group,
  ServiceStatus,
} from 'koroku-console/utils/types';
import AppLinks from './app-links';
import AsyncView from './async-view';
import Banner from './banner';
import Button from './button';
import ConfirmDialog from './confirm-dialog';
import EnvironmentBadge from './environment-badge';
import ErrorMessage from './error-message';
import Icon from './icon';

/** One group as a choice: whether the app may join it, and why not. */
interface GroupChoice {
  group: Group;
  selected: boolean;
  blocker: string | null;
  /** The other apps in it. */
  members: string;
}

/** One service as a choice of what to expose. */
interface ServiceChoice {
  name: string;
  checked: boolean;
  /** The name cannot be a DNS label: it cannot be exposed. */
  invalid: boolean;
  database: boolean;
  /** The saved links of other members to this service. */
  usedBy: string[];
}

function sameSet(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((value) => b.includes(value));
}

function environmentLabel(app: App): string {
  return ENVIRONMENT_LABELS[app.environment];
}

interface Signature {
  Args: { model: AppContext };
}

/**
 * The settings tab's group: which group the app is in and which of its
 * services the other members may link to (ARCHITECTURE 6.1), and below it
 * the app's own links. Saved on its own, apart from the settings form: the
 * API applies it at once, and leaving or un-exposing deletes the links
 * that depend on it, so those are named before saving.
 */
export default class AppGroupSettings extends Component<Signature> {
  @service declare api: ApiService;

  groups: Loadable<Group[]>;
  services: Loadable<ServiceStatus[]>;
  links: Loadable<AppLink[]>;

  @tracked groupId: number | null;
  @tracked chosen: string[];

  @tracked saving = false;
  @tracked confirming = false;
  @tracked error: unknown = null;
  @tracked notice = '';
  @tracked nothingToSave = false;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.groupId = appGroupId(this.app);
    this.chosen = appGroupServices(this.app);
    this.groups = new Loadable(() => this.args.model.loadGroups());
    this.services = new Loadable(() => this.api.listServices(this.app.slug));
    this.links = new Loadable(() => this.api.listLinks(this.app.slug));
    void this.groups.load();
    void this.services.load();
    if (this.savedGroupId !== null) void this.links.load();
  }

  get app(): App {
    return this.args.model.app;
  }

  get savedGroupId(): number | null {
    return appGroupId(this.app);
  }

  get savedServices(): string[] {
    return appGroupServices(this.app);
  }

  get changed(): boolean {
    return (
      this.groupId !== this.savedGroupId ||
      !sameSet(this.chosen, this.savedServices)
    );
  }

  /** The group the app is in now, as saved. */
  get savedGroup(): Group | null {
    if (this.savedGroupId === null) return null;
    return (
      (this.groups.value ?? []).find((g) => g.id === this.savedGroupId) ?? null
    );
  }

  get selectedGroup(): Group | null {
    if (this.groupId === null) return null;
    return (this.groups.value ?? []).find((g) => g.id === this.groupId) ?? null;
  }

  /** The links that saving the current choice deletes. */
  get removed(): RemovedLink[] {
    return removedLinks(this.savedGroup, this.app.slug, {
      groupId: this.groupId,
      services: this.chosen,
    });
  }

  get leaving(): boolean {
    return this.savedGroupId !== null && this.groupId !== this.savedGroupId;
  }

  /** Newly exposed services: they connect nothing by themselves. */
  get exposing(): string[] {
    if (this.groupId === null) return [];
    if (this.leaving) return [...this.chosen].sort();
    return this.chosen.filter((s) => !this.savedServices.includes(s)).sort();
  }

  get groupChoices(): GroupChoice[] {
    return (this.groups.value ?? []).map((group) => ({
      group,
      selected: group.id === this.groupId,
      blocker: joinBlocker(group, this.app.environment),
      members: groupMembers(group)
        .map((member) => member.slug)
        .filter((slug) => slug !== this.app.slug)
        .join('、'),
    }));
  }

  get blockedCount(): number {
    return this.groupChoices.filter((choice) => choice.blocker).length;
  }

  /** Who links to which of this app's services now. */
  private get linksToApp(): Map<string, string[]> {
    const used = new Map<string, string[]>();
    if (!this.savedGroup) return used;
    for (const member of groupMembers(this.savedGroup)) {
      for (const link of memberLinks(member)) {
        if (link.target_app !== this.app.slug) continue;
        const list = used.get(link.target_service) ?? [];
        list.push(`${member.slug} の ${link.name}`);
        used.set(link.target_service, list);
      }
    }
    return used;
  }

  /**
   * The running services, plus any exposed one that is not running now
   * (stopped, or gone from the release): it can still be un-exposed.
   */
  get serviceChoices(): ServiceChoice[] {
    const names = new Set<string>(this.chosen);
    for (const service of this.services.value ?? []) names.add(service.name);
    const used = this.groupId === this.savedGroupId ? this.linksToApp : null;
    return [...names].sort().map((name) => ({
      name,
      checked: this.chosen.includes(name),
      invalid: !DNS_LABEL.test(name),
      database: looksLikeDatabase(name),
      usedBy: used?.get(name) ?? [],
    }));
  }

  get chosenDatabases(): string[] {
    return this.chosen.filter((name) => looksLikeDatabase(name)).sort();
  }

  get chosenDatabaseList(): string {
    return this.chosenDatabases.join('、');
  }

  /** The change is about the services only, not about the group. */
  get staysInGroup(): boolean {
    return this.groupId !== null && this.groupId === this.savedGroupId;
  }

  /** Nothing to choose from: the groups could not be read. */
  get cannotSave(): boolean {
    return !this.groups.loaded;
  }

  get errors(): Record<string, string> {
    return fieldErrors(this.error);
  }

  private touch(): void {
    this.notice = '';
    this.nothingToSave = false;
  }

  chooseGroup = (id: number | null) => {
    this.touch();
    this.groupId = id;
    // Leaving a group exposes nothing; another group starts from the
    // services chosen so far.
    if (id === null) this.chosen = [];
  };

  toggleService = (name: string, event: Event) => {
    this.touch();
    const checked = inputChecked(event);
    const others = this.chosen.filter((service) => service !== name);
    this.chosen = checked ? [...others, name] : others;
  };

  save = () => {
    this.touch();
    this.error = null;
    if (!this.changed) {
      this.nothingToSave = true;
      return;
    }
    if (this.args.model.isProduction || this.removed.length > 0) {
      this.confirming = true;
      return;
    }
    return this.apply();
  };

  cancel = () => {
    this.confirming = false;
  };

  apply = async () => {
    const removed = this.removed.length;
    const exposing = this.exposing.length > 0;
    const leaving = this.groupId === null;
    this.saving = true;
    try {
      const app = await this.api.setAppGroup(this.app.slug, {
        group_id: this.groupId,
        services: leaving ? [] : [...this.chosen].sort(),
      });
      this.args.model.set(app);
      this.groupId = appGroupId(app);
      this.chosen = appGroupServices(app);
      this.notice = savedNotice({ leaving, removed, exposing });
    } catch (error) {
      this.error = error;
    } finally {
      this.saving = false;
      this.confirming = false;
    }
    await this.refresh();
  };

  /** The members and links changed (or someone else changed them). */
  refresh = async () => {
    await Promise.all([
      this.groups.load({ quiet: true }),
      this.savedGroupId === null
        ? Promise.resolve()
        : this.links.load({ quiet: this.links.loaded }),
    ]);
  };

  <template>
    {{! A backend without groups: the section is left out. }}
    {{#unless this.groups.unavailable}}
      <form
        class="settings-group-form"
        novalidate
        aria-labelledby="settings-group-title"
        data-test-group-settings
        {{on "submit" (preventDefault this.save)}}
      >
        <div class="form-group">
          <div>
            <h2 class="form-group-title" id="settings-group-title">グループ</h2>
            <p class="form-group-description">
              接続を作れるのは、同じグループのアプリの間だけです。グループに入ることや、サービスを公開することだけでは、どこにもつながりません。
            </p>
          </div>
          <div class="form-group-body">
            <AsyncView
              @data={{this.groups}}
              @errorTitle="グループを取得できませんでした"
            >
              <fieldset
                class="option-group"
                aria-describedby="settings-group-environment"
                data-test-group-choices
              >
                <legend>所属するグループ</legend>
                <label class="option">
                  <input
                    type="radio"
                    name="group"
                    checked={{if this.groupId false true}}
                    disabled={{this.saving}}
                    data-test-group-option="none"
                    {{on "change" (fn this.chooseGroup null)}}
                  />
                  <span class="option-body">
                    <span class="option-label">グループに入れない</span>
                    <span class="option-hint">
                      ほかのアプリとは接続できません。
                    </span>
                  </span>
                </label>
                {{#each this.groupChoices key="group.id" as |choice|}}
                  <label
                    class="option"
                    data-test-group-option-row={{choice.group.name}}
                  >
                    <input
                      type="radio"
                      name="group"
                      checked={{choice.selected}}
                      disabled={{if choice.blocker true this.saving}}
                      data-test-group-option={{choice.group.name}}
                      {{on "change" (fn this.chooseGroup choice.group.id)}}
                    />
                    <span class="option-body">
                      <span class="option-label group-option-label">
                        {{choice.group.name}}
                        <EnvironmentBadge
                          @environment={{choice.group.environment}}
                        />
                      </span>
                      {{#if choice.blocker}}
                        <span class="option-hint" data-test-group-blocker>
                          {{choice.blocker}}
                        </span>
                      {{else if choice.members}}
                        <span class="option-hint">
                          メンバー:
                          {{choice.members}}
                        </span>
                      {{else}}
                        <span
                          class="option-hint"
                        >メンバーはまだいません。</span>
                      {{/if}}
                    </span>
                  </label>
                {{/each}}
                <p
                  class="option-hint"
                  id="settings-group-environment"
                  data-test-group-environment-note
                >
                  {{#if this.groupChoices.length}}
                    このアプリは{{environmentLabel
                      this.app
                    }}なので、{{environmentLabel
                      this.app
                    }}のグループにだけ入れます。{{#if
                      this.blockedCount
                    }}ほかの環境のグループは選べません。ステージングから本番に届く構成を作れないようにするためです。{{/if}}
                  {{else}}
                    グループはまだありません。
                  {{/if}}
                  <LinkTo @route="system" data-test-group-system-link>
                    グループはシステム画面で作成します。
                  </LinkTo>
                </p>
              </fieldset>
              {{#if this.errors.group_id}}
                <p
                  class="field-error group-field-error"
                  data-test-field-error="group_id"
                >
                  {{this.errors.group_id}}
                </p>
              {{/if}}

              {{#if this.groupId}}
                <fieldset
                  class="option-group group-services"
                  aria-describedby="settings-group-services-note"
                  data-test-group-services
                >
                  <legend>公開するサービス</legend>
                  {{#each this.serviceChoices key="name" as |choice|}}
                    <label
                      class="option"
                      data-test-group-service={{choice.name}}
                    >
                      <input
                        type="checkbox"
                        checked={{choice.checked}}
                        disabled={{if
                          choice.invalid
                          (if choice.checked this.saving true)
                          this.saving
                        }}
                        data-test-group-service-checkbox={{choice.name}}
                        {{on "change" (fn this.toggleService choice.name)}}
                      />
                      <span class="option-body">
                        <span class="option-label group-option-label">
                          <span class="mono">{{choice.name}}</span>
                          {{#if choice.database}}
                            <span
                              class="badge badge-warn"
                              data-test-group-service-database
                            >データベースらしい</span>
                          {{/if}}
                        </span>
                        {{#if choice.invalid}}
                          <span class="option-hint">
                            名前に英小文字・数字・ハイフン以外が含まれるため、公開できません。
                          </span>
                        {{else if choice.usedBy.length}}
                          <span
                            class="option-hint"
                            data-test-group-service-used-by
                          >
                            接続している:
                            {{#each choice.usedBy as |user index|}}{{if
                                index
                                "、"
                              }}<span class="mono">{{user}}</span>{{/each}}
                          </span>
                        {{/if}}
                      </span>
                    </label>
                  {{else}}
                    <p class="option-hint" data-test-group-services-empty>
                      稼働中のサービスがないため、一覧を出せません。デプロイ（停止中なら起動）すると選べます。
                    </p>
                  {{/each}}
                  <p class="option-hint" id="settings-group-services-note">
                    公開したサービスは、グループのほかのアプリが接続先に選べるようになります。公開するだけでは、どこにもつながりません。Compose
                    で
                    <code>container_name</code>
                    か
                    <code>network_mode</code>
                    を指定したサービスは公開できません（保存のときに断られます）。
                  </p>
                </fieldset>
                {{#if this.errors.services}}
                  <p
                    class="field-error group-field-error"
                    data-test-field-error="services"
                  >
                    公開できないサービスが含まれています。<code
                    >container_name</code>
                    か
                    <code>network_mode</code>
                    を指定したサービスと、稼働中のリリースにないサービスは公開できません（{{this.errors.services}}）。
                  </p>
                {{/if}}

                <div class="group-reach" data-test-group-reach>
                  <p class="group-reach-title">届く範囲</p>
                  <ul class="group-reach-list">
                    <li>
                      公開したサービスは、それに接続したアプリの利用側のサービスと、<strong
                      >双方向に、すべてのポートで</strong>通信できます。ポートや向きは絞れません。
                    </li>
                    <li>
                      グループからの離脱と公開の解除は、保存した時点ですぐに切断し、それに依存する接続も削除します。
                    </li>
                    <li>
                      外へのポートの公開や volume
                      の共有はできません。共有するのはネットワークだけです。
                    </li>
                  </ul>
                </div>

                {{#if this.chosenDatabases.length}}
                  <Banner
                    @tone="warning"
                    @title="データベースらしいサービスを公開しようとしています"
                    data-test-group-database-warning
                  >
                    {{#each this.chosenDatabases as |name|}}<span
                        class="mono"
                      >{{name}}</span>
                    {{/each}}に接続したアプリからは、データベースへ直接届くようになります。ほかのアプリに見せたいのが
                    API だけなら、API
                    のサービスだけを公開してください。データベースを公開するなら、パスワードなどの認証が効いていることを確かめてください。
                  </Banner>
                {{/if}}
              {{/if}}

              {{#if this.removed.length}}
                <Banner
                  @tone="warning"
                  @title="保存すると、次の接続が削除されます"
                  class="group-removed-banner"
                  data-test-group-removed
                >
                  <ul class="group-removed-list">
                    {{#each this.removed key="id" as |link|}}
                      <li
                        class="mono"
                        data-test-group-removed-link
                      >{{link.label}}</li>
                    {{/each}}
                  </ul>
                  その名前を使っているサービスは、保存した時点で相手に接続できなくなります。
                </Banner>
              {{/if}}
            </AsyncView>
          </div>
        </div>

        <div aria-live="polite">
          <ErrorMessage @error={{this.error}} />
        </div>

        <div class="form-footer">
          <Button
            @type="submit"
            @variant="primary"
            @busy={{this.saving}}
            @busyLabel="保存中…"
            @disabled={{this.cannotSave}}
            data-test-group-save
          >グループの設定を保存</Button>
          <p class="form-status" aria-live="polite">
            {{#if this.notice}}
              <span class="form-saved" data-test-group-saved>
                <Icon @name="check" />
                {{this.notice}}
              </span>
            {{else if this.nothingToSave}}
              <span data-test-group-unchanged>変更はありません。</span>
            {{else if this.removed.length}}
              保存した時点で、{{this.removed.length}}
              件の接続を削除します。
            {{else}}
              グループの設定は、ほかの設定とは別に保存します。
            {{/if}}
          </p>
        </div>
      </form>

      <AppLinks
        @model={{@model}}
        @groups={{this.groups}}
        @services={{this.services}}
        @links={{this.links}}
        @onChanged={{this.refresh}}
      />

      {{#if this.confirming}}
        <ConfirmDialog
          @title={{if
            @model.isProduction
            "本番アプリのグループの設定を変えますか？"
            "接続を削除して保存しますか？"
          }}
          @confirmLabel={{if this.removed.length "削除して保存する" "保存する"}}
          @danger={{if this.removed.length true false}}
          @busy={{this.saving}}
          @onConfirm={{this.apply}}
          @onCancel={{this.cancel}}
        >
          <p data-test-group-confirm>
            {{if @model.isProduction "本番アプリ" "アプリ"}}
            <strong>{{this.app.slug}}</strong>
            {{#if this.selectedGroup}}
              {{#if this.staysInGroup}}
                がグループ
                <strong>{{this.selectedGroup.name}}</strong>
                で公開するサービスを変えます。
              {{else}}
                をグループ
                <strong>{{this.selectedGroup.name}}</strong>
                に入れます。
              {{/if}}
              {{#if this.chosen.length}}
                保存後に公開するのは
                {{#each this.chosen as |name index|}}{{if index "、"}}<code
                  >{{name}}</code>{{/each}}
                です。
              {{else}}
                サービスは何も公開しません。
              {{/if}}
            {{else}}
              をグループから外します。
            {{/if}}
          </p>
          {{#if this.removed.length}}
            <p>次の接続を、保存した時点で削除して切断します。</p>
            <ul class="group-dialog-links" data-test-group-confirm-removed>
              {{#each this.removed key="id" as |link|}}
                <li class="mono">{{link.label}}</li>
              {{/each}}
            </ul>
            <p>
              その名前を使っているサービスは、相手に接続できなくなります。元に戻すには、接続を作り直してください。
            </p>
          {{/if}}
          {{#if this.exposing.length}}
            <p>
              新しく公開するサービスは、ほかのアプリが接続を作るまで、どこにもつながりません。
            </p>
          {{/if}}
          {{#if this.chosenDatabases.length}}
            <p data-test-group-confirm-database>
              <strong
              >データベースらしいサービス（{{this.chosenDatabaseList}}）を公開します。</strong>
            </p>
          {{/if}}
        </ConfirmDialog>
      {{/if}}
    {{/unless}}
  </template>
}

function savedNotice({
  leaving,
  removed,
  exposing,
}: {
  leaving: boolean;
  removed: number;
  exposing: boolean;
}): string {
  if (leaving) {
    return removed > 0
      ? `保存しました。グループから外し、${removed} 件の接続を削除しました。`
      : '保存しました。グループから外しました。';
  }
  if (removed > 0) {
    return `保存しました。${removed} 件の接続を削除し、切断しました。`;
  }
  if (exposing) {
    return '保存しました。公開したサービスは、ほかのアプリが接続を作るとつながります。';
  }
  return '保存しました。';
}
