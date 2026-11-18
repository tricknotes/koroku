import Component from '@glimmer/component';
import { cached, tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { registerDestructor } from '@ember/destroyable';
import type Owner from '@ember/owner';
import type ApiService from 'koroku-console/services/api';
import type EventSourceService from 'koroku-console/services/event-source';
import type { Stream } from 'koroku-console/services/event-source';
import DeployLogBuffer from 'koroku-console/utils/deploy-log-buffer';
import {
  parseDeployLog,
  stepLabel,
  type ParsedDeployLog,
  type RollbackKind,
  type RollbackOutcome,
} from 'koroku-console/utils/deploy-steps';
import { formatDateTimeJst } from 'koroku-console/utils/format';
import { isActiveDeployment } from 'koroku-console/utils/status';
import type { Deployment } from 'koroku-console/utils/types';
import Banner, { type BannerTone } from './banner';
import Button, { buttonClass } from './button';
import CopyButton from './copy-button';
import DeploymentSteps from './deployment-steps';
import Icon from './icon';

type StreamState = 'streaming' | 'ended' | 'failed';

const STREAM_LABELS: Record<StreamState, string> = {
  streaming: 'ログを受信中…',
  ended: 'ログの終わりです。',
  failed: 'ログに接続できませんでした。',
};

interface Notice {
  tone: BannerTone;
  title: string;
  text: string;
}

function target(outcome: RollbackOutcome): string {
  return outcome.deploymentId === null
    ? '旧版'
    : `旧版（デプロイ #${outcome.deploymentId}）`;
}

/** What became of the release that was serving, in the reader's words. */
const ROLLBACK_NOTICES: Record<
  RollbackKind,
  (outcome: RollbackOutcome) => Notice
> = {
  restored: (outcome) => ({
    tone: 'info',
    title: '旧版に自動で戻しました',
    text: `${target(outcome)}を起動し直しました。旧版が応答し続けています。`,
  }),
  restoring: (outcome) => ({
    tone: 'info',
    title: '旧版に自動で戻しています',
    text: `${target(outcome)}を起動し直しています。`,
  }),
  failed: (outcome) => ({
    tone: 'error',
    title: '旧版に戻せませんでした',
    text: `${target(outcome)}を自動で起動し直せませんでした。アプリが応答していない可能性があります。原因を直して再デプロイしてください。`,
  }),
  removed: () => ({
    tone: 'info',
    title: '起動しかけたコンテナを削除しました',
    text: '初回のデプロイで戻す先のリリースがないため、起動しかけたコンテナを削除しました。volume は残っています。',
  }),
  untouched: () => ({
    tone: 'info',
    title: '稼働中のリリースには影響していません',
    text: 'コンテナを入れ替える前に止まったため、それまでのリリースがそのまま応答しています。',
  }),
  skipped: () => ({
    tone: 'warning',
    title: '旧版には自動で戻していません',
    text: 'koroku の停止と重なったため、ロールバックを実行していません。アプリの状態を確認してください。',
  }),
};

interface DeploymentDetailSignature {
  Element: HTMLDivElement;
  Args: {
    deployment: Deployment;
    /** The log reached its end: the deployment has a final status now. */
    onLogEnd?: () => unknown;
  };
}

/**
 * Everything about one deployment: its steps with their logs, live over SSE
 * while it runs, and in plain words how it ended (what failed, and whether
 * the previous release was brought back).
 */
export default class DeploymentDetail extends Component<DeploymentDetailSignature> {
  @service declare api: ApiService;
  @service('event-source') declare eventSource: EventSourceService;

  buffer = new DeployLogBuffer();

  @tracked streamState: StreamState = 'streaming';

  private stream: Stream | null = null;

  constructor(owner: Owner, args: DeploymentDetailSignature['Args']) {
    super(owner, args);
    this.open();
    registerDestructor(this, () => {
      this.closeStream();
      this.buffer.stop();
    });
  }

  @cached
  get log(): ParsedDeployLog {
    return parseDeployLog(this.buffer.lines, this.args.deployment);
  }

  get streamLabel(): string {
    return STREAM_LABELS[this.streamState];
  }

  get isStreaming(): boolean {
    return this.streamState === 'streaming';
  }

  get streamFailed(): boolean {
    return this.streamState === 'failed';
  }

  get logTextUrl(): string {
    return `${this.api.deploymentLogsUrl(this.args.deployment.id)}?format=text`;
  }

  get isFailed(): boolean {
    return this.args.deployment.status === 'failed';
  }

  /** "「確認」の手順で失敗しました。" when the log tells which step it was. */
  get failedStepText(): string {
    const step =
      this.log.failure?.step ??
      this.log.steps.find((entry) => entry.state === 'failed');
    return step ? `「${stepLabel(step.key)}」の手順で失敗しました。` : '';
  }

  get rollbackNotice():
    (Notice & { kind: RollbackKind; detail: string }) | null {
    const outcome = this.log.rollback;
    if (!outcome) return null;
    return {
      ...ROLLBACK_NOTICES[outcome.kind](outcome),
      kind: outcome.kind,
      detail: outcome.detail,
    };
  }

  /** Nothing to show although the log is complete: it was cleaned up. */
  get isEmpty(): boolean {
    return this.streamState === 'ended' && this.log.steps.length === 0;
  }

  get isWaiting(): boolean {
    return this.isStreaming && this.log.steps.length === 0;
  }

  /** Starts over after the connection was lost for good. */
  reconnect = () => {
    this.closeStream();
    this.buffer.clear();
    this.streamState = 'streaming';
    this.open();
  };

  /** Shows the log from its start and follows it until it ends. */
  private open(): void {
    const url = this.api.deploymentLogsUrl(this.args.deployment.id);
    this.stream = this.eventSource.open(url, {
      message: (line) => this.buffer.push(line),
      end: () => {
        // The server is done; without close() EventSource would reconnect.
        this.closeStream();
        this.buffer.flush();
        this.streamState = 'ended';
        void this.args.onLogEnd?.();
      },
      error: (closed) => {
        if (!closed) return;
        this.closeStream();
        this.buffer.flush();
        this.streamState = 'failed';
      },
    });
  }

  private closeStream(): void {
    this.stream?.close();
    this.stream = null;
  }

  <template>
    <div class="deploy-detail" data-test-log ...attributes>
      <div class="deploy-detail-bar">
        <dl class="deploy-facts">
          <div>
            <dt>デプロイ</dt>
            <dd>#{{@deployment.id}}</dd>
          </div>
          {{#if @deployment.ref}}
            <div>
              <dt>ブランチ</dt>
              <dd>{{@deployment.ref}}</dd>
            </div>
          {{/if}}
          {{#if @deployment.started_at}}
            <div>
              <dt>開始</dt>
              <dd>{{formatDateTimeJst @deployment.started_at}}</dd>
            </div>
          {{/if}}
          {{#if @deployment.commit_sha}}
            <div class="deploy-fact-sha">
              <dt>コミット</dt>
              <dd>
                <span class="deploy-sha-full">{{@deployment.commit_sha}}</span>
                <CopyButton
                  @text={{@deployment.commit_sha}}
                  @label="コミット SHA"
                />
              </dd>
            </div>
          {{/if}}
        </dl>

        <div class="deploy-detail-actions">
          <span
            class="stream-state
              {{unless this.isStreaming 'visually-hidden'}}
              {{if (isActiveDeployment @deployment) 'is-live'}}"
            aria-live="polite"
            data-state={{this.streamState}}
            data-test-stream-state
          >{{this.streamLabel}}</span>
          <a
            class={{buttonClass "default" "small"}}
            href={{this.logTextUrl}}
            target="_blank"
            rel="noopener noreferrer"
            data-test-log-text-link
          >全文を開く<Icon @name="external" /></a>
        </div>
      </div>

      {{#if this.isFailed}}
        <Banner @tone="error" @title="デプロイに失敗しました">
          {{this.failedStepText}}
          <span
            class="deploy-error-reason"
            data-test-deployment-error
          >{{@deployment.error}}</span>
        </Banner>
      {{/if}}

      {{#if this.rollbackNotice}}
        <Banner
          @tone={{this.rollbackNotice.tone}}
          @title={{this.rollbackNotice.title}}
          data-rollback={{this.rollbackNotice.kind}}
          data-test-rollback-notice
        >
          {{this.rollbackNotice.text}}
          {{#if this.rollbackNotice.detail}}
            <span
              class="deploy-error-reason"
            >{{this.rollbackNotice.detail}}</span>
          {{/if}}
        </Banner>
      {{/if}}

      {{#if this.streamFailed}}
        <Banner @tone="error" data-test-stream-error>
          <:default>{{this.streamLabel}}</:default>
          <:actions>
            <Button
              @size="small"
              data-test-reconnect
              {{on "click" this.reconnect}}
            >再接続</Button>
          </:actions>
        </Banner>
      {{/if}}

      {{#if this.buffer.dropped}}
        <p class="deploy-note" data-test-log-dropped>
          ログが長いため、古い行を省いています。すべての行は「全文を開く」で確認できます。
        </p>
      {{/if}}

      {{#if this.log.steps.length}}
        <DeploymentSteps @steps={{this.log.steps}} />
      {{else if this.isEmpty}}
        <p class="deploy-note" data-test-no-log>
          このデプロイのログは残っていません。
        </p>
      {{else if this.isWaiting}}
        <p class="deploy-note" data-test-log-waiting>ログを待っています…</p>
      {{/if}}
    </div>
  </template>
}
