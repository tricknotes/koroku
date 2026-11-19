import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { fn } from '@ember/helper';
import { on } from '@ember/modifier';
import { LinkTo } from '@ember/routing';
import type ApiService from 'koroku-console/services/api';
import type AppContext from 'koroku-console/utils/app-context';
import { inputChecked, inputValue } from 'koroku-console/utils/dom';
import { errorMessage, fieldErrors } from 'koroku-console/utils/errors';
import {
  DNS_LABEL,
  appGroupId,
  groupMembers,
  linkLabel,
  linkNameProblem,
  linkTargetLabel,
  linkTargets,
  looksLikeDatabase,
  type LinkTarget,
} from 'koroku-console/utils/groups';
import type Loadable from 'koroku-console/utils/loadable';
import type {
  App,
  AppLink,
  Group,
  ServiceStatus,
} from 'koroku-console/utils/types';
import AsyncView from './async-view';
import Banner from './banner';
import Button from './button';
import ConfirmDialog from './confirm-dialog';
import EmptyState from './empty-state';
import ErrorMessage from './error-message';
import FormField from './form-field';

/** The add dialog's draft. */
class LinkDraft {
  @tracked name = '';
  @tracked target: string | null = null;
  @tracked services: string[] = [];
}

/** One consumer service as a choice of the add dialog. */
interface ConsumerChoice {
  name: string;
  checked: boolean;
  /** The name is not a DNS label: the API refuses it. */
  invalid: boolean;
}

function joined(names: string[]): string {
  return names.join('、');
}

/** Errors the dialog shows next to a field; anything else is a banner. */
const DIALOG_FIELDS = ['name', 'target_app', 'target_service', 'services'];

interface Signature {
  Args: {
    model: AppContext;
    groups: Loadable<Group[]>;
    services: Loadable<ServiceStatus[]>;
    links: Loadable<AppLink[]>;
    /** Reads the groups and the links again after a change. */
    onChanged: () => Promise<void>;
  };
}

/**
 * The settings tab's links (ARCHITECTURE 6.1): the names this app's own
 * Compose file uses for exposed services of other members of its group.
 * A link is made and removed here and takes effect at once on the target
 * side; this app's services join it from the next deploy or start.
 */
export default class AppLinks extends Component<Signature> {
  @service declare api: ApiService;

  @tracked adding = false;
  @tracked deleting: AppLink | null = null;
  @tracked draft = new LinkDraft();

  @tracked busy = false;
  @tracked dialogError: unknown = null;
  @tracked dialogErrors: Record<string, string> = {};
  @tracked notice = '';
  @tracked error: unknown = null;

  get app(): App {
    return this.args.model.app;
  }

  get groupId(): number | null {
    return appGroupId(this.app);
  }

  get group(): Group | null {
    if (this.groupId === null) return null;
    return (
      (this.args.groups.value ?? []).find((g) => g.id === this.groupId) ?? null
    );
  }

  get targets(): LinkTarget[] {
    return linkTargets(this.group, this.app.slug);
  }

  /** Other members, whatever they expose. */
  get otherMembers(): string[] {
    if (!this.group) return [];
    return groupMembers(this.group)
      .map((member) => member.slug)
      .filter((slug) => slug !== this.app.slug);
  }

  get firstOtherMember(): string | null {
    return this.otherMembers[0] ?? null;
  }

  get links(): AppLink[] {
    return this.args.links.value ?? [];
  }

  get ownServices(): string[] {
    return (this.args.services.value ?? []).map((service) => service.name);
  }

  get consumerChoices(): ConsumerChoice[] {
    return [...this.ownServices].sort().map((name) => ({
      name,
      checked: this.draft.services.includes(name),
      invalid: !DNS_LABEL.test(name),
    }));
  }

  /** The dialog error that has no field of its own to sit next to. */
  get dialogBanner(): unknown {
    const fields = Object.keys(this.dialogErrors);
    return fields.some((name) => DIALOG_FIELDS.includes(name))
      ? null
      : this.dialogError;
  }

  get chosenTarget(): LinkTarget | null {
    return this.targets.find((t) => t.key === this.draft.target) ?? null;
  }

  get chosenTargetIsDatabase(): boolean {
    const target = this.chosenTarget;
    return target !== null && looksLikeDatabase(target.service);
  }

  /** `auth → accounts / auth` while the dialog is filled in. */
  get preview(): string | null {
    const target = this.chosenTarget;
    if (!target || this.draft.name === '' || this.dialogErrors['name']) {
      return null;
    }
    return linkLabel(this.app.slug, {
      name: this.draft.name,
      target_app: target.app,
      target_service: target.service,
    });
  }

  get targetError(): string | undefined {
    return (
      this.dialogErrors['target_app'] ?? this.dialogErrors['target_service']
    );
  }

  private reset(): void {
    this.dialogError = null;
    this.dialogErrors = {};
    this.notice = '';
    this.error = null;
  }

  openAdd = () => {
    this.reset();
    this.draft = new LinkDraft();
    this.adding = true;
  };

  openDelete = (link: AppLink) => {
    this.reset();
    this.deleting = link;
  };

  close = () => {
    this.adding = false;
    this.deleting = null;
  };

  /** Brings the group choice above into view: the way into a group. */
  focusGroup = () => {
    const choice = document.querySelector<HTMLInputElement>(
      '[data-test-group-choices] input:checked',
    );
    choice?.scrollIntoView({ block: 'center' });
    choice?.focus();
  };

  setName = (event: Event) => {
    this.draft.name = inputValue(event).trim();
    if (this.dialogErrors['name']) {
      this.dialogErrors = Object.fromEntries(
        Object.entries(this.dialogErrors).filter(([field]) => field !== 'name'),
      );
    }
  };

  chooseTarget = (target: LinkTarget) => {
    this.draft.target = target.key;
    // The target's own service name is the usual choice of name.
    if (this.draft.name === '' && !this.ownServices.includes(target.service)) {
      this.draft.name = target.service;
    }
  };

  toggleConsumer = (name: string, event: Event) => {
    const others = this.draft.services.filter((s) => s !== name);
    this.draft.services = inputChecked(event) ? [...others, name] : others;
  };

  /** What the console can tell before asking the API. */
  private check(): Record<string, string> {
    const errors: Record<string, string> = {};
    const problem = linkNameProblem(
      this.draft.name,
      this.ownServices,
      this.links.map((link) => link.name),
    );
    if (problem) errors['name'] = problem;
    if (!this.chosenTarget) errors['target_app'] = '接続先を選んでください。';
    if (this.draft.services.length === 0) {
      errors['services'] = 'この名前を使うサービスを 1 つ以上選んでください。';
    }
    return errors;
  }

  add = async () => {
    const errors = this.check();
    const target = this.chosenTarget;
    if (Object.keys(errors).length > 0 || !target) {
      this.dialogError = null;
      this.dialogErrors = errors;
      return;
    }
    this.busy = true;
    this.dialogError = null;
    this.dialogErrors = {};
    try {
      const link = await this.api.createLink(this.app.slug, {
        name: this.draft.name,
        target_app: target.app,
        target_service: target.service,
        services: [...this.draft.services].sort(),
      });
      this.notice = `接続 ${link.name} を追加しました。${linkTargetLabel(link)} はすぐにつなぎました。このアプリの ${joined(link.services)} は、次のデプロイか起動からつながります。`;
      this.close();
    } catch (error) {
      this.dialogError = error;
      this.dialogErrors = fieldErrors(error);
      return;
    } finally {
      this.busy = false;
    }
    await this.args.onChanged();
  };

  remove = async () => {
    const link = this.deleting;
    if (!link) return;
    this.busy = true;
    try {
      await this.api.deleteLink(this.app.slug, link.id);
      this.notice = `接続 ${link.name} を削除し、切断しました。`;
    } catch (error) {
      this.error = error;
    } finally {
      this.busy = false;
      this.close();
    }
    await this.args.onChanged();
  };

  <template>
    <section
      class="settings-links"
      aria-labelledby="settings-links-title"
      data-test-links
    >
      <div class="form-group">
        <div>
          <h2 class="form-group-title" id="settings-links-title">接続</h2>
          <p class="form-group-description">
            このアプリの Compose の中で決めた名前（例:
            <code
            >auth</code>）で、同じグループのほかのアプリが公開するサービスを呼べるようにします。koroku
            が接続ごとに専用のネットワークを作り、相手のサービスをその名前でつなぎます。
          </p>
        </div>
        <div class="form-group-body">
          <div class="pane-notices" aria-live="polite">
            {{#if this.notice}}
              <Banner data-test-link-notice>{{this.notice}}</Banner>
            {{/if}}
            <ErrorMessage @error={{this.error}} />
          </div>

          {{#if this.groupId}}
            <AsyncView
              @data={{@links}}
              @errorTitle="接続を取得できませんでした"
            >
              {{#if this.links.length}}
                <ul class="link-list" aria-label="接続">
                  {{#each this.links key="id" as |link|}}
                    <li class="link-row" data-test-link={{link.name}}>
                      <div class="link-body">
                        <p class="link-route">
                          <span
                            class="mono link-name"
                            data-test-link-name
                          >{{link.name}}</span>
                          <span class="link-arrow" aria-hidden="true">→</span>
                          <span class="visually-hidden">の接続先は</span>
                          <span
                            class="mono"
                            data-test-link-target
                          >{{linkTargetLabel link}}</span>
                        </p>
                        <p class="link-consumers" data-test-link-services>
                          使うサービス:
                          {{#each link.services as |name index|}}{{if
                              index
                              "、"
                            }}<span class="mono">{{name}}</span>{{/each}}
                        </p>
                      </div>
                      <Button
                        @size="small"
                        class="button-danger-outline"
                        aria-label="接続 {{link.name}} を削除"
                        data-test-link-delete
                        {{on "click" (fn this.openDelete link)}}
                      >削除…</Button>
                    </li>
                  {{/each}}
                </ul>
                <div class="link-actions">
                  <Button
                    @disabled={{if this.targets.length false true}}
                    data-test-link-add
                    {{on "click" this.openAdd}}
                  >接続を追加</Button>
                  {{#unless this.targets.length}}
                    <p class="option-hint">
                      ほかのメンバーが公開しているサービスがないため、追加できません。
                    </p>
                  {{/unless}}
                </div>
              {{else if this.targets.length}}
                <EmptyState
                  @title="接続はありません"
                  @description="グループのほかのアプリが公開しているサービスを、このアプリの Compose で使う名前に結びつけます。"
                  data-test-links-empty
                >
                  <:action>
                    <Button
                      @variant="primary"
                      data-test-link-add
                      {{on "click" this.openAdd}}
                    >接続を追加</Button>
                  </:action>
                </EmptyState>
              {{else}}
                <EmptyState
                  @title="接続できるサービスがありません"
                  @description="グループ {{if
                    this.group
                    this.group.name
                  }} のほかのアプリが、まだサービスを公開していません。相手のアプリの設定タブの「公開するサービス」で選ぶと、ここで接続先に選べます。"
                  data-test-links-no-targets
                >
                  <:action>
                    {{#if this.firstOtherMember}}
                      <LinkTo
                        @route="app.settings"
                        @model={{this.firstOtherMember}}
                        class="button"
                        data-test-links-open-member
                      >{{this.firstOtherMember}} の設定を開く</LinkTo>
                    {{else}}
                      <LinkTo
                        @route="system"
                        class="button"
                        data-test-links-open-system
                      >システム画面でグループを見る</LinkTo>
                    {{/if}}
                  </:action>
                </EmptyState>
              {{/if}}
            </AsyncView>
          {{else}}
            <EmptyState
              @title="グループに入っていません"
              @description="接続を作れるのは、同じグループのアプリの間だけです。上の「所属するグループ」でグループを選んで保存してください。"
              data-test-links-no-group
            >
              <:action>
                <Button
                  data-test-links-choose-group
                  {{on "click" this.focusGroup}}
                >グループを選ぶ</Button>
              </:action>
            </EmptyState>
          {{/if}}
        </div>
      </div>
    </section>

    {{#if this.adding}}
      <ConfirmDialog
        @title="接続を追加"
        @confirmLabel="追加する"
        @busy={{this.busy}}
        @onConfirm={{this.add}}
        @onCancel={{this.close}}
      >
        {{#if @model.isProduction}}
          <Banner
            @tone="warning"
            @title="本番アプリの接続です"
            class="link-dialog-banner"
            data-test-link-production
          >
            追加すると、相手のサービスをすぐにこの接続のネットワークにつなぎます。
          </Banner>
        {{/if}}
        <fieldset class="option-group dialog-options" data-test-link-targets>
          <legend>接続先</legend>
          {{#each this.targets key="key" as |target|}}
            <label class="option" data-test-link-target-option={{target.key}}>
              <input
                type="radio"
                name="link-target"
                checked={{if (eq this.draft.target target.key) true false}}
                disabled={{this.busy}}
                data-test-link-target-input={{target.key}}
                {{on "change" (fn this.chooseTarget target)}}
              />
              <span class="option-body">
                <span class="option-label group-option-label">
                  <span class="mono">{{target.app}} / {{target.service}}</span>
                  {{#if (isDatabase target.service)}}
                    <span class="badge badge-warn">データベースらしい</span>
                  {{/if}}
                </span>
              </span>
            </label>
          {{/each}}
          <p class="option-hint">
            同じグループのほかのアプリが公開しているサービスだけを選べます。
          </p>
        </fieldset>
        {{#if this.targetError}}
          <p
            class="field-error link-field-error"
            data-test-field-error="target"
          >
            {{this.targetError}}
          </p>
        {{/if}}

        <FormField
          @label="名前"
          @hint="このアプリの Compose の中で相手を呼ぶ名前です（例: auth）。英小文字・数字・ハイフンで、このアプリのサービスと同じ名前は使えません。"
          @error={{this.dialogErrors.name}}
          data-test-field-group="link-name"
          as |field|
        >
          <input
            id={{field.id}}
            type="text"
            class="mono"
            autocomplete="off"
            autocapitalize="off"
            spellcheck="false"
            value={{this.draft.name}}
            disabled={{this.busy}}
            aria-describedby={{field.describedBy}}
            aria-invalid={{field.invalid}}
            data-test-link-name-input
            {{on "input" this.setName}}
          />
        </FormField>

        <fieldset class="option-group dialog-options" data-test-link-consumers>
          <legend>この名前を使うサービス</legend>
          {{#each this.consumerChoices key="name" as |choice|}}
            <label class="option" data-test-link-consumer={{choice.name}}>
              <input
                type="checkbox"
                checked={{choice.checked}}
                disabled={{if choice.invalid true this.busy}}
                data-test-link-consumer-checkbox={{choice.name}}
                {{on "change" (fn this.toggleConsumer choice.name)}}
              />
              <span class="option-body">
                <span class="option-label mono">{{choice.name}}</span>
              </span>
            </label>
          {{else}}
            <p class="option-hint" data-test-link-consumers-empty>
              稼働中のサービスがないため、選べません。デプロイ（停止中なら起動）してから追加してください。
            </p>
          {{/each}}
          <p class="option-hint">
            選んだサービスだけが接続のネットワークにつながり、相手とは双方向に、すべてのポートで通信できます。次のデプロイか起動からつながります。
          </p>
        </fieldset>
        {{#if this.dialogErrors.services}}
          <p
            class="field-error link-field-error"
            data-test-field-error="services"
          >
            {{this.dialogErrors.services}}
          </p>
        {{/if}}

        {{#if this.chosenTargetIsDatabase}}
          <p class="link-dialog-note" data-test-link-database>
            データベースらしいサービスへの接続です。認証を必須にしたまま使ってください（接続の通信は暗号化されません）。
          </p>
        {{/if}}
        {{#if this.preview}}
          <p class="link-dialog-note" data-test-link-preview>
            追加後:
            <span class="mono">{{this.preview}}</span>
          </p>
        {{/if}}
        {{#if this.dialogBanner}}
          <p class="field-error" role="alert" data-test-dialog-error>
            {{errorMessage this.dialogBanner}}
          </p>
        {{/if}}
      </ConfirmDialog>
    {{/if}}

    {{#if this.deleting}}
      <ConfirmDialog
        @title={{if
          @model.isProduction
          "本番アプリの接続を削除しますか？"
          "接続を削除しますか？"
        }}
        @confirmLabel="削除する"
        @danger={{true}}
        @busy={{this.busy}}
        @onConfirm={{this.remove}}
        @onCancel={{this.close}}
      >
        <p data-test-link-delete-confirm>
          <span class="mono">{{linkLabel this.app.slug this.deleting}}</span>
          を削除し、すぐに切断します。
        </p>
        <p>
          このアプリの
          {{#each this.deleting.services as |name index|}}{{if index "、"}}<code
            >{{name}}</code>{{/each}}
          は、<code>{{this.deleting.name}}</code>
          の名前で相手に接続できなくなります。
        </p>
      </ConfirmDialog>
    {{/if}}
  </template>
}

function eq(a: unknown, b: unknown): boolean {
  return a === b;
}

function isDatabase(name: string): boolean {
  return looksLikeDatabase(name);
}
