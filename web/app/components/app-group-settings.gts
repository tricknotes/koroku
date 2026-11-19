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
  GROUP_SERVICE_NAME,
  appGroupId,
  appGroupServices,
  groupChangeEffect,
  groupHostname,
  groupMembers,
  joinBlocker,
  looksLikeDatabase,
} from 'koroku-console/utils/groups';
import Loadable from 'koroku-console/utils/loadable';
import { ENVIRONMENT_LABELS } from 'koroku-console/utils/status';
import type { App, Group, ServiceStatus } from 'koroku-console/utils/types';
import AsyncView from './async-view';
import Banner from './banner';
import Button from './button';
import ConfirmDialog from './confirm-dialog';
import CopyButton from './copy-button';
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

/** One service as a choice. */
interface ServiceChoice {
  name: string;
  hostname: string;
  checked: boolean;
  /** The name cannot be a DNS label: it cannot join. */
  invalid: boolean;
  database: boolean;
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
 * The settings tab's choice of a group and of the services that join its
 * shared network (ARCHITECTURE 6.1). Saved on its own, apart from the
 * settings form: the API applies it at once, and narrowing takes effect
 * immediately.
 */
export default class AppGroupSettings extends Component<Signature> {
  @service declare api: ApiService;

  groups: Loadable<Group[]>;
  services: Loadable<ServiceStatus[]>;

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
    void this.groups.load();
    void this.services.load();
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

  get effect() {
    return groupChangeEffect(
      { groupId: this.savedGroupId, services: this.savedServices },
      { groupId: this.groupId, services: this.chosen },
    );
  }

  get selectedGroup(): Group | null {
    if (this.groupId === null) return null;
    return (this.groups.value ?? []).find((g) => g.id === this.groupId) ?? null;
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

  /** Some groups are there, but none the app may join. */
  get onlyOtherEnvironments(): boolean {
    const choices = this.groupChoices;
    return choices.length > 0 && choices.every((choice) => choice.blocker);
  }

  get blockedCount(): number {
    return this.groupChoices.filter((choice) => choice.blocker).length;
  }

  /**
   * The running services, plus any connected one that is not running now
   * (stopped, or gone from the release): it can still be disconnected.
   */
  get serviceChoices(): ServiceChoice[] {
    const names = new Set<string>(this.chosen);
    for (const service of this.services.value ?? []) names.add(service.name);
    return [...names].sort().map((name) => ({
      name,
      hostname: groupHostname(name, this.app.slug),
      checked: this.chosen.includes(name),
      invalid: !GROUP_SERVICE_NAME.test(name),
      database: looksLikeDatabase(name),
    }));
  }

  get chosenHostnames(): string[] {
    return [...this.chosen]
      .sort()
      .map((name) => groupHostname(name, this.app.slug));
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
    // Leaving a group disconnects everything; another group starts from
    // the services chosen so far.
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
    if (this.args.model.isProduction) {
      this.confirming = true;
      return;
    }
    return this.apply();
  };

  cancel = () => {
    this.confirming = false;
  };

  apply = async () => {
    const effect = this.effect;
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
      this.notice = savedNotice(effect, leaving);
    } catch (error) {
      this.error = error;
    } finally {
      this.saving = false;
      this.confirming = false;
    }
    // The members of the groups changed (or someone else changed them).
    await this.groups.load({ quiet: true });
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
              同じグループのアプリどうしで、選んだサービスだけを共用のネットワークにつなぎます。ほかのアプリからは
              <span class="mono"><span
                  class="group-pattern"
                >&lt;サービス&gt;.</span>{{this.app.slug}}.<wbr
                />koroku.internal</span>
              の名前で接続します。
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
                      ほかのアプリとはネットワークでつながりません。
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
                  <legend>つなぐサービス</legend>
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
                            名前に英小文字・数字・ハイフン以外が含まれるため、つなげません。
                          </span>
                        {{else}}
                          <span
                            class="option-hint mono"
                          >{{choice.hostname}}</span>
                        {{/if}}
                      </span>
                    </label>
                  {{else}}
                    <p class="option-hint" data-test-group-services-empty>
                      稼働中のサービスがないため、一覧を出せません。デプロイ（停止中なら起動）すると選べます。
                    </p>
                  {{/each}}
                  <p class="option-hint" id="settings-group-services-note">
                    サービスを 1
                    つも選ばなければ、グループに入るだけで何もつなぎません。Compose
                    で
                    <code>container_name</code>
                    か
                    <code>network_mode</code>
                    を指定したサービスはつなげません（保存のときに断られます）。
                  </p>
                </fieldset>
                {{#if this.errors.services}}
                  <p
                    class="field-error group-field-error"
                    data-test-field-error="services"
                  >
                    つなげないサービスが含まれています。<code
                    >container_name</code>
                    か
                    <code>network_mode</code>
                    を指定したサービスと、稼働中のリリースにないサービスはつなげません（{{this.errors.services}}）。
                  </p>
                {{/if}}

                <div class="group-reach" data-test-group-reach>
                  <p class="group-reach-title">届く範囲</p>
                  <ul class="group-reach-list">
                    <li>
                      つないだサービスは、同じグループでつながっているほかのアプリのサービスと、<strong
                      >双方向に、すべてのポートで</strong>通信できます。ポートや向きは絞れません。
                    </li>
                    <li>
                      参加とサービスの追加は、次のデプロイか起動から効きます。外す変更は、保存した時点ですぐに切断します。
                    </li>
                    <li>
                      外へのポートの公開や volume
                      の共有はできません。グループで共有するのはネットワークだけです。
                    </li>
                  </ul>
                </div>

                {{#if this.chosenHostnames.length}}
                  <div class="group-hostname-list" data-test-group-hostnames>
                    <p class="group-reach-title">
                      ほかのメンバーからの接続先
                    </p>
                    <ul>
                      {{#each this.chosenHostnames as |hostname|}}
                        <li>
                          <span
                            class="mono"
                            data-test-group-chosen-hostname
                          >{{hostname}}</span>
                          <CopyButton @text={{hostname}} @label={{hostname}} />
                        </li>
                      {{/each}}
                    </ul>
                  </div>
                {{/if}}

                {{#if this.chosenDatabases.length}}
                  <Banner
                    @tone="warning"
                    @title="データベースらしいサービスをつなごうとしています"
                    data-test-group-database-warning
                  >
                    {{#each this.chosenDatabases as |name|}}<span
                        class="mono"
                      >{{name}}</span>
                    {{/each}}は、グループのほかのアプリから直接接続できるようになります。ほかのアプリに見せたいのが
                    API だけなら、API
                    のサービスだけをつないでください。データベースをつなぐなら、パスワードなどの認証が効いていることを確かめてください。
                  </Banner>
                {{/if}}
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
            {{else if this.effect.narrows}}
              外す変更は、保存した時点ですぐに切断します。
            {{else}}
              グループの設定は、ほかの設定とは別に保存します。
            {{/if}}
          </p>
        </div>
      </form>

      {{#if this.confirming}}
        <ConfirmDialog
          @title="本番アプリのグループの設定を変えますか？"
          @confirmLabel="保存する"
          @busy={{this.saving}}
          @onConfirm={{this.apply}}
          @onCancel={{this.cancel}}
        >
          <p data-test-group-confirm>
            本番アプリ
            <strong>{{this.app.slug}}</strong>
            {{#if this.selectedGroup}}
              {{#if this.staysInGroup}}
                がグループ
                <strong>{{this.selectedGroup.name}}</strong>
                でつなぐサービスを変えます。
              {{else}}
                をグループ
                <strong>{{this.selectedGroup.name}}</strong>
                に入れます。
              {{/if}}
              {{#if this.chosen.length}}
                保存後、ほかのメンバーから接続できるのは次の名前です。
              {{else}}
                サービスは何もつなぎません。
              {{/if}}
            {{else}}
              をグループから外し、共用ネットワークからすぐに切断します。
            {{/if}}
          </p>
          {{#if this.selectedGroup}}
            {{#if this.chosen.length}}
              <ul class="group-dialog-hostnames">
                {{#each this.chosenHostnames as |hostname|}}
                  <li class="mono">{{hostname}}</li>
                {{/each}}
              </ul>
            {{/if}}
          {{/if}}
          {{#if this.effect.narrows}}
            <p>
              外すサービスへの接続は、保存した時点で切れます。ほかのアプリがそのサービスを使っていれば、そのアプリはエラーになります。
            </p>
          {{/if}}
          {{#if this.effect.widens}}
            <p>新しくつなぐサービスは、次のデプロイか起動から接続されます。</p>
          {{/if}}
          {{#if this.chosenDatabases.length}}
            <p data-test-group-confirm-database>
              <strong
              >データベースらしいサービス（{{this.chosenDatabaseList}}）を含みます。</strong>
            </p>
          {{/if}}
        </ConfirmDialog>
      {{/if}}
    {{/unless}}
  </template>
}

function savedNotice(
  effect: { widens: boolean; narrows: boolean },
  leaving: boolean,
): string {
  if (leaving) {
    return '保存しました。グループから外し、共用ネットワークから切断しました。';
  }
  if (effect.widens && effect.narrows) {
    return '保存しました。外したサービスはすでに切断しました。つないだサービスは、次のデプロイか起動から接続されます。';
  }
  if (effect.narrows) {
    return '保存しました。外したサービスは共用ネットワークから切断しました。';
  }
  return '保存しました。次のデプロイか起動から接続されます。';
}
