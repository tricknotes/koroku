import Component from '@glimmer/component';
import { service } from '@ember/service';
import { tracked } from '@glimmer/tracking';
import { array, fn, hash } from '@ember/helper';
import { on } from '@ember/modifier';
import { LinkTo } from '@ember/routing';
import type RouterService from '@ember/routing/router-service';
import { schedule } from '@ember/runloop';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import ActionMenu from 'koroku-console/components/action-menu';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import ErrorMessage from 'koroku-console/components/error-message';
import { buttonClass } from 'koroku-console/components/button';
import EnvironmentBadge from 'koroku-console/components/environment-badge';
import ProtectionBadge from 'koroku-console/components/protection-badge';
import RoleBadge from 'koroku-console/components/role-badge';
import Icon from 'koroku-console/components/icon';
import StatusIndicator from 'koroku-console/components/status-indicator';
import Tabs from 'koroku-console/components/tabs';
import scrollActiveIntoView from 'koroku-console/modifiers/scroll-active-into-view';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type AppContext from 'koroku-console/utils/app-context';
import { errorMessage, policyViolations } from 'koroku-console/utils/errors';
import { hostOf } from 'koroku-console/utils/format';
import { can } from 'koroku-console/utils/roles';
import {
  APP_STATE_HINTS,
  APP_STATE_KINDS,
  APP_STATE_LABELS,
  deriveAppState,
} from 'koroku-console/utils/status';
import type { App } from 'koroku-console/utils/types';

/** How often the header re-reads the app while a detail page is open. */
const POLL_INTERVAL = 5000;

/**
 * Tabs that take the whole window instead of the centred column: the layout
 * gets the class `app-detail-full` on these routes (see app-detail.css).
 */
const FULL_SIZE_ROUTES = new Set(['app.logs']);

const DEPLOY_ROUTE = 'app.deploy';

type ControlAction = 'start' | 'stop' | 'restart' | 'wake';

const CONTROL_DONE: Record<ControlAction, string> = {
  start: '起動しました。',
  stop: '停止しました。',
  restart: '再起動しました。',
  wake: '起こしました。',
};

function stateKind(app: App) {
  return APP_STATE_KINDS[deriveAppState(app)];
}

function stateLabel(app: App): string {
  return APP_STATE_LABELS[deriveAppState(app)];
}

function stateHint(app: App): string {
  return APP_STATE_HINTS[deriveAppState(app)];
}

/** Where the header link leads: the primary domain, or the assigned one. */
function openUrl(app: App): string {
  return app.primary_url || app.url;
}

interface Signature {
  Args: { model: AppContext };
}

/**
 * App detail: a header that every tab shares (name, environment, state,
 * primary URL, the way to the deploy form and the start, stop and restart
 * actions; ARCHITECTURE 9.1) above the tabs. The state is re-read while the
 * page is open, so it never needs a reload.
 */
export default class AppTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;
  @service declare router: RouterService;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.poller.every(this, POLL_INTERVAL, () => this.refresh());
  }

  @tracked controlling: ControlAction | null = null;
  @tracked controlNotice = '';
  @tracked controlError: unknown = null;
  @tracked confirmingStop = false;

  get app(): App {
    return this.args.model.app;
  }

  /** Start, stop and restart need a release to act on. */
  get deployed(): boolean {
    return this.app.current_deployment !== null;
  }

  get stopped(): boolean {
    return this.app.desired_state === 'stopped';
  }

  /** Stopped while idle; a request or 「起こす」 wakes it (ARCHITECTURE 8.4). */
  get sleeping(): boolean {
    return !this.stopped && Boolean(this.app.sleeping_since);
  }

  /** What to do with a stopped or sleeping app, shown as a button. */
  get showsStart(): boolean {
    return this.canDeploy && this.deployed && this.stopped;
  }

  get showsWake(): boolean {
    return this.canDeploy && this.deployed && this.sleeping;
  }

  /** Restart and stop, for an app that is not stopped. */
  get showsMenu(): boolean {
    return this.canDeploy && this.deployed && !this.stopped;
  }

  get violations(): string[] {
    return policyViolations(this.controlError);
  }

  isControlling = (action: ControlAction): boolean =>
    this.controlling === action;

  control = async (action: ControlAction) => {
    if (this.controlling) return;
    this.controlling = action;
    this.controlNotice = '';
    this.controlError = null;
    try {
      const app =
        action === 'wake'
          ? await this.api.wakeApp(this.args.model.slug)
          : await this.api.controlApp(this.args.model.slug, action);
      this.args.model.set(app);
      this.controlNotice = CONTROL_DONE[action];
    } catch (error) {
      this.controlError = error;
    } finally {
      this.controlling = null;
      this.confirmingStop = false;
    }
  };

  requestStop = () => {
    this.controlNotice = '';
    this.controlError = null;
    this.confirmingStop = true;
  };

  cancelStop = () => {
    this.confirmingStop = false;
  };

  confirmStop = () => this.control('stop');

  get isFullSize(): boolean {
    return FULL_SIZE_ROUTES.has(this.router.currentRouteName ?? '');
  }

  get onDeployTab(): boolean {
    return this.router.currentRouteName === DEPLOY_ROUTE;
  }

  /** Members below the deploy role only look (ARCHITECTURE 10.7). */
  get canDeploy(): boolean {
    return can(this.args.model.app, 'deploy');
  }

  get deployUrl(): string {
    return this.router.urlFor(DEPLOY_ROUTE, this.args.model.slug);
  }

  /**
   * The deploy tab has the form's own button as its main action, and a
   * stopped or sleeping app has 「起動」 or 「起こす」.
   */
  get deployButtonClass(): string {
    const quiet = this.onDeployTab || this.showsStart || this.showsWake;
    return buttonClass(quiet ? 'default' : 'primary');
  }

  /**
   * Picks up changes made elsewhere (a deployment finishing, a domain being
   * verified). The context is only replaced when something differs, so the
   * tabs are not re-rendered every few seconds for nothing.
   */
  async refresh(): Promise<void> {
    const context = this.args.model;
    if (!context.loaded) return;
    let app: App;
    try {
      app = await this.api.getApp(context.slug);
    } catch {
      // What is on screen stays; the next tick tries again.
      return;
    }
    if (this.args.model !== context) return;
    if (JSON.stringify(app) !== JSON.stringify(context.app)) context.set(app);
  }

  /** Opens the deploy tab and puts the focus on what to deploy. */
  openDeploy = (event: MouseEvent) => {
    // Leave "open in a new tab" and the like to the browser.
    if (
      event.button !== 0 ||
      event.metaKey ||
      event.ctrlKey ||
      event.shiftKey
    ) {
      return;
    }
    event.preventDefault();
    void this.focusDeployForm();
  };

  private async focusDeployForm(): Promise<void> {
    try {
      await this.router.transitionTo(DEPLOY_ROUTE, this.args.model.slug);
    } catch {
      // Held back (unsaved settings ask first); the tab goes on by itself.
      return;
    }
    // After the tab has rendered. A run loop queue, so tests wait for it.
    // eslint-disable-next-line ember/no-runloop
    schedule('afterRender', () => {
      const control = document.querySelector<HTMLElement>(
        '[data-deploy-focus]',
      );
      control?.focus();
      control?.scrollIntoView({ block: 'nearest' });
    });
  }

  <template>
    {{pageTitle @model.slug}}

    {{#if @model.loaded}}
      <div
        class="app-detail {{if this.isFullSize 'app-detail-full'}}"
        data-test-app-detail
      >
        <header class="app-header">
          <div class="app-header-main">
            <div class="app-header-title">
              <h1 data-test-app-title>{{@model.app.slug}}</h1>
              <EnvironmentBadge
                @environment={{@model.app.environment}}
                data-test-app-environment
              />
              {{#if @model.app.access_protection}}
                <ProtectionBadge data-test-app-protected />
              {{/if}}
              <RoleBadge @role={{@model.app.role}} />
            </div>
            <div class="app-header-meta">
              <StatusIndicator
                @kind={{stateKind @model.app}}
                @label={{stateLabel @model.app}}
                title={{stateHint @model.app}}
                data-test-app-state
              />
              {{#if (openUrl @model.app)}}
                <a
                  class="app-header-url"
                  href={{openUrl @model.app}}
                  target="_blank"
                  rel="noopener noreferrer"
                  aria-label="{{hostOf
                    (openUrl @model.app)
                  }} を新しいタブで開く"
                  data-test-app-url
                ><span>{{hostOf (openUrl @model.app)}}</span><Icon
                    @name="external"
                  /></a>
              {{/if}}
              {{#if @model.app.review}}
                <span class="app-header-parent" data-test-app-parent>
                  <span class="app-header-parent-label">親</span>
                  <LinkTo
                    @route="app.deploy"
                    @model={{@model.app.review.parent}}
                    @query={{hash section="review-apps"}}
                    data-test-app-parent-link
                  >{{@model.app.review.parent}}</LinkTo>
                  <span class="app-header-parent-pr">PR #{{@model.app.review.pr_number}}</span>
                </span>
              {{/if}}
            </div>
          </div>
          {{#if this.canDeploy}}
            <div class="app-header-actions" data-test-header-actions>
              {{#if this.showsStart}}
                <Button
                  @variant="primary"
                  @busy={{this.isControlling "start"}}
                  @busyLabel="起動中…"
                  @disabled={{if this.controlling true false}}
                  data-test-control="start"
                  {{on "click" (fn this.control "start")}}
                >起動</Button>
              {{else if this.showsWake}}
                <Button
                  @variant="primary"
                  @busy={{this.isControlling "wake"}}
                  @busyLabel="起動中…"
                  @disabled={{if this.controlling true false}}
                  data-test-control="wake"
                  {{on "click" (fn this.control "wake")}}
                >起こす</Button>
              {{/if}}
              <a
                class={{this.deployButtonClass}}
                href={{this.deployUrl}}
                data-test-header-deploy
                {{on "click" this.openDeploy}}
              >デプロイ</a>
              {{#if this.showsMenu}}
                <ActionMenu @label="その他の操作" as |close|>
                  <button
                    type="button"
                    class="menu-item"
                    disabled={{if this.controlling true false}}
                    data-test-control="restart"
                    {{on "click" close}}
                    {{on "click" (fn this.control "restart")}}
                  >再起動</button>
                  <button
                    type="button"
                    class="menu-item menu-item-danger"
                    disabled={{if this.controlling true false}}
                    data-test-control="stop"
                    {{on "click" close}}
                    {{on "click" this.requestStop}}
                  >停止…</button>
                </ActionMenu>
              {{/if}}
            </div>
          {{/if}}
        </header>

        <div class="app-header-notices" aria-live="polite">
          {{#if this.controlling}}
            <p class="app-header-note" data-test-control-busy>
              <Icon @name="spinner" />
              {{if
                (this.isControlling "stop")
                "停止しています…"
                "完了まで待っています…"
              }}
            </p>
          {{else if this.controlNotice}}
            <Banner data-test-notice>{{this.controlNotice}}</Banner>
          {{/if}}
          {{#if this.controlError}}
            <ErrorMessage @error={{this.controlError}} />
            {{#if this.violations.length}}
              <ul class="form-problems" data-test-violations>
                {{#each this.violations as |line|}}
                  <li>{{line}}</li>
                {{/each}}
              </ul>
            {{/if}}
          {{/if}}
        </div>

        <Tabs
          @label="アプリのメニュー"
          class="app-tabs"
          {{scrollActiveIntoView this.router.currentRouteName}}
          as |tabs|
        >
          <tabs.Tab
            @route="app.index"
            @model={{@model.slug}}
            data-test-tab="index"
          >概要</tabs.Tab>
          <tabs.Tab
            @route="app.deploy"
            @model={{@model.slug}}
            data-test-tab="deploy"
          >デプロイ</tabs.Tab>
          <tabs.Tab
            @route="app.logs"
            @model={{@model.slug}}
            data-test-tab="logs"
          >ログ</tabs.Tab>
          <tabs.Tab
            @route="app.resources"
            @model={{@model.slug}}
            data-test-tab="resources"
          >リソース</tabs.Tab>
          <tabs.Tab
            @route="app.tasks"
            @model={{@model.slug}}
            data-test-tab="tasks"
          >タスク</tabs.Tab>
          <tabs.Tab
            @route="app.env"
            @model={{@model.slug}}
            data-test-tab="env"
          >環境変数</tabs.Tab>
          <tabs.Tab
            @route="app.backups"
            @model={{@model.slug}}
            data-test-tab="backups"
          >バックアップ</tabs.Tab>
          <tabs.Tab
            @route="app.settings"
            @model={{@model.slug}}
            data-test-tab="settings"
          >設定</tabs.Tab>
        </Tabs>

        <div class="app-detail-body">
          {{! Going straight from one app to another keeps the same routes,
              and Ember would keep the tab's component (and what it loaded
              for the previous app). Keying on the context gives each app a
              fresh tab. }}
          {{#each (array @model) key="@identity"}}
            {{outlet}}
          {{/each}}
        </div>
      </div>

      {{#if this.confirmingStop}}
        <ConfirmDialog
          @title={{if
            @model.isProduction
            "本番アプリを停止しますか？"
            "アプリを停止しますか？"
          }}
          @confirmLabel="停止する"
          @danger={{true}}
          @busy={{this.isControlling "stop"}}
          @onConfirm={{this.confirmStop}}
          @onCancel={{this.cancelStop}}
        >
          <p data-test-stop-dialog>
            {{if @model.isProduction "本番アプリ" "アプリ"}}
            <strong>{{@model.app.slug}}</strong>
            のコンテナを止めます。起動するまで、アクセスには 503
            のページが返ります。データは消えません。
          </p>
        </ConfirmDialog>
      {{/if}}
    {{else}}
      <div class="app-missing" data-test-app-error>
        <h1>{{@model.slug}}</h1>
        <Banner @tone="error" @title="アプリを表示できません">
          {{errorMessage @model.error}}
        </Banner>
        <LinkTo
          @route="index"
          class={{buttonClass "default"}}
          data-test-back
        >アプリ一覧に戻る</LinkTo>
      </div>
    {{/if}}
  </template>
}
