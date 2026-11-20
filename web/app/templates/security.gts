import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import ApiTokens from 'koroku-console/components/api-tokens';
import AsyncView from 'koroku-console/components/async-view';
import Badge from 'koroku-console/components/badge';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import ErrorMessage from 'koroku-console/components/error-message';
import RelativeTime from 'koroku-console/components/relative-time';
import RowList from 'koroku-console/components/row-list';
import StatusIndicator from 'koroku-console/components/status-indicator';
import TimeUntil from 'koroku-console/components/time-until';
import type ApiService from 'koroku-console/services/api';
import type ClockService from 'koroku-console/services/clock';
import type SessionService from 'koroku-console/services/session';
import Loadable from 'koroku-console/utils/loadable';
import type { StatusKind } from 'koroku-console/utils/status';
import type {
  LoginEvent,
  LoginOutcome,
  LoginSession,
} from 'koroku-console/utils/types';
import { describeUserAgent } from 'koroku-console/utils/user-agent';

const OUTCOME_LABELS: Record<LoginOutcome, string> = {
  succeeded: '成功',
  not_allowed: '許可されていないアカウント',
  // Only in histories written by older versions.
  two_factor_required: '2 段階認証が無効',
  two_factor_unknown: '2 段階認証を確認できない',
};

function outcomeLabel(event: LoginEvent): string {
  return OUTCOME_LABELS[event.outcome] ?? event.outcome;
}

function outcomeKind(event: LoginEvent): StatusKind {
  return event.outcome === 'succeeded' ? 'running' : 'failed';
}

function succeeded(event: LoginEvent): boolean {
  return event.outcome === 'succeeded';
}

function device(item: { user_agent: string }): string {
  return describeUserAgent(item.user_agent);
}

/**
 * The login of another user's session, which only the owner sees
 * (ARCHITECTURE 10.7); '' for the user's own.
 */
function otherUser(item: LoginSession, me: number | undefined): string {
  if (!item.user || item.user.id === me) return '';
  return item.user.login;
}

/**
 * Who is logged in where, and who tried to log in. The place to notice a
 * stolen session and end it (ARCHITECTURE 10.3).
 */
export default class SecurityTemplate extends Component {
  @service declare api: ApiService;
  @service declare session: SessionService;
  @service declare clock: ClockService;

  sessions = new Loadable<LoginSession[]>(() => this.api.listSessions());
  events = new Loadable<LoginEvent[]>(() => this.api.listLoginEvents());

  /** Id of the session being ended. */
  @tracked ending: string | null = null;
  @tracked confirmingRevokeAll = false;
  @tracked revokingAll = false;
  @tracked revoked: number | null = null;
  @tracked actionError: unknown = null;

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.sessions.load();
    void this.events.load();
  }

  /** The user's own sessions in other browsers ("end the others" ends these). */
  get others(): number {
    const me = this.session.user?.id;
    return (this.sessions.value ?? []).filter(
      (s) => !s.current && (!s.user || s.user.id === me),
    ).length;
  }

  get me(): number | undefined {
    return this.session.user?.id;
  }

  get isOwner(): boolean {
    return this.session.isOwner;
  }

  /** Whole minutes left in which dangerous operations need no new login. */
  get reauthMinutesLeft(): number | null {
    const until = this.session.reauthUntil;
    if (until === null) return null;
    const left = until - this.clock.now;
    return left > 0 ? Math.max(1, Math.ceil(left / 60_000)) : 0;
  }

  get reauthActive(): boolean {
    return (this.reauthMinutesLeft ?? 0) > 0;
  }

  get reauthApplies(): boolean {
    return this.reauthMinutesLeft !== null;
  }

  isEnding = (id: string): boolean => this.ending === id;

  end = async (item: LoginSession) => {
    if (this.ending) return;
    this.ending = item.id;
    this.actionError = null;
    this.revoked = null;
    try {
      await this.api.deleteSession(item.id);
      await this.sessions.load({ quiet: true });
    } catch (error) {
      this.actionError = error;
    } finally {
      this.ending = null;
    }
  };

  requestRevokeAll = () => {
    this.confirmingRevokeAll = true;
  };

  cancelRevokeAll = () => {
    this.confirmingRevokeAll = false;
  };

  revokeAll = async () => {
    this.revokingAll = true;
    this.actionError = null;
    try {
      const { revoked } = await this.api.revokeOtherSessions();
      this.revoked = revoked;
      await this.sessions.load({ quiet: true });
    } catch (error) {
      this.actionError = error;
    } finally {
      this.revokingAll = false;
      this.confirmingRevokeAll = false;
    }
  };

  reauthenticate = () => this.session.reauthenticate();

  <template>
    {{pageTitle "セキュリティ"}}

    <div class="page-header">
      <h1>セキュリティ</h1>
    </div>

    {{#if this.reauthApplies}}
      <section class="pane" aria-labelledby="reauth-title">
        <div class="pane-header">
          <h2 class="pane-title" id="reauth-title">危険な操作の確認</h2>
          {{#unless this.reauthActive}}
            <Button data-test-reauth-now {{on "click" this.reauthenticate}}>
              今すぐ再ログイン
            </Button>
          {{/unless}}
        </div>
        <p class="security-note">
          アプリの削除、本番アプリのデプロイ、環境変数の値の表示と保存、リストア、ダンプのダウンロード、環境やリポジトリの変更、API
          トークンの作成、メンバーの追加とロールの付与には、直近 10
          分以内のログインが必要です。
        </p>
        <p class="security-reauth" data-test-reauth-state>
          {{#if this.reauthActive}}
            <StatusIndicator
              @kind="running"
              @label="あと {{this.reauthMinutesLeft}} 分は再ログインなしで操作できます"
            />
          {{else}}
            <StatusIndicator
              @kind="stopped"
              @label="次に危険な操作をするときは再ログインが必要です"
            />
          {{/if}}
        </p>
      </section>
    {{/if}}

    <section class="pane" aria-labelledby="sessions-title">
      <div class="pane-header">
        <h2 class="pane-title" id="sessions-title">ログイン中のセッション</h2>
        {{#if this.others}}
          <Button
            @busy={{this.revokingAll}}
            @busyLabel="終了中…"
            data-test-revoke-others
            {{on "click" this.requestRevokeAll}}
          >ほかのセッションをすべて終了…</Button>
        {{/if}}
      </div>
      <p class="security-note">
        {{#if this.isOwner}}
          ログインしているブラウザーです。オーナーには、メンバーのセッションも表示します。心当たりのないものがあれば終了し、GitHub
          のパスワードも変えてください。
        {{else}}
          このアカウントでログインしているブラウザーです。心当たりのないものがあれば終了し、GitHub
          のパスワードも変えてください。
        {{/if}}
      </p>

      <div class="pane-notices" aria-live="polite">
        {{#if this.revoked}}
          <Banner data-test-revoked>
            {{this.revoked}}
            件のセッションを終了しました。
          </Banner>
        {{/if}}
        <ErrorMessage @error={{this.actionError}} />
      </div>

      <AsyncView
        @data={{this.sessions}}
        @errorTitle="セッションの一覧を取得できませんでした"
      >
        <:default as |sessions|>
          {{#if sessions.length}}
            <RowList
              class="session-rows"
              @label="ログイン中のセッション"
              data-test-sessions
            >
              <:header>
                <span>ブラウザー</span>
                <span>ログイン</span>
                <span>最終利用</span>
                <span>有効期限</span>
                <span></span>
              </:header>
              <:rows as |Row|>
                {{#each sessions key="id" as |item|}}
                  <Row data-test-session={{item.id}}>
                    <span class="session-device">
                      {{#if (otherUser item this.me)}}
                        <span
                          class="session-user"
                          data-test-session-user
                        >{{otherUser item this.me}}</span>
                      {{/if}}
                      <span
                        class="session-agent"
                        title={{item.user_agent}}
                        data-test-session-agent
                      >{{device item}}</span>
                      {{#if item.ip}}
                        <span
                          class="mono session-ip"
                          data-test-session-ip
                        >{{item.ip}}</span>
                      {{/if}}
                    </span>
                    <span class="session-time">
                      <span class="cell-label">ログイン</span>
                      <RelativeTime @time={{item.created_at}} />
                    </span>
                    <span class="session-time">
                      <span class="cell-label">最終利用</span>
                      <RelativeTime @time={{item.last_seen_at}} />
                    </span>
                    <span class="session-time">
                      <span class="cell-label">有効期限</span>
                      <TimeUntil @time={{item.expires_at}} />
                    </span>
                    <span class="session-action">
                      {{#if item.current}}
                        <Badge @tone="info" data-test-session-current>
                          このブラウザー
                        </Badge>
                      {{else}}
                        <Button
                          @size="small"
                          @busy={{this.isEnding item.id}}
                          @busyLabel="終了中…"
                          aria-label="{{device item}} のセッションを終了"
                          data-test-session-end
                          {{on "click" (fn this.end item)}}
                        >終了</Button>
                      {{/if}}
                    </span>
                  </Row>
                {{/each}}
              </:rows>
            </RowList>
          {{else}}
            <p class="security-note" data-test-no-sessions>
              セッションはありません（開発用の認証なしモードでは記録しません）。
            </p>
          {{/if}}
        </:default>
      </AsyncView>
    </section>

    <ApiTokens />

    <section class="pane" aria-labelledby="events-title">
      <div class="pane-header">
        <h2 class="pane-title" id="events-title">ログイン履歴</h2>
      </div>
      <p class="security-note">
        {{#if this.isOwner}}
          GitHub
          の認証まで進んだログインの記録です（メンバーのものを含め、新しい順に最大
          50 件）。断ったログインも残ります。
        {{else}}
          あなたのアカウントのログインの記録です（新しい順に最大 50 件）。
        {{/if}}
      </p>

      <AsyncView
        @data={{this.events}}
        @errorTitle="ログイン履歴を取得できませんでした"
      >
        <:default as |events|>
          {{#if events.length}}
            <RowList
              class="event-rows"
              @label="ログイン履歴"
              data-test-login-events
            >
              <:header>
                <span>結果</span>
                <span>日時</span>
                <span>アカウント</span>
                <span>ブラウザー</span>
              </:header>
              <:rows as |Row|>
                {{#each events as |event|}}
                  <Row data-test-login-event={{event.outcome}}>
                    <span class="event-outcome">
                      <StatusIndicator
                        @kind={{outcomeKind event}}
                        @label={{outcomeLabel event}}
                        @quiet={{succeeded event}}
                      />
                    </span>
                    <span class="session-time">
                      <span class="cell-label">日時</span>
                      <RelativeTime @time={{event.at}} />
                    </span>
                    <span class="event-login">
                      <span class="cell-label">アカウント</span>
                      {{event.login}}
                    </span>
                    <span class="session-device">
                      <span
                        class="session-agent"
                        title={{event.user_agent}}
                      >{{device event}}</span>
                      {{#if event.ip}}
                        <span class="mono session-ip">{{event.ip}}</span>
                      {{/if}}
                    </span>
                  </Row>
                {{/each}}
              </:rows>
            </RowList>
          {{else}}
            <p class="security-note" data-test-no-login-events>
              まだ記録はありません。
            </p>
          {{/if}}
        </:default>
      </AsyncView>
    </section>

    {{#if this.confirmingRevokeAll}}
      <ConfirmDialog
        @title="ほかのセッションをすべて終了しますか？"
        @confirmLabel="すべて終了する"
        @danger={{true}}
        @busy={{this.revokingAll}}
        @onConfirm={{this.revokeAll}}
        @onCancel={{this.cancelRevokeAll}}
      >
        <p>このブラウザー以外の
          <strong>{{this.others}} 件</strong>
          のセッションを終了します。それらのブラウザーでは、もう一度 GitHub
          でのログインが必要になります。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
