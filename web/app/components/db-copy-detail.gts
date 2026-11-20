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
  copyStepLabel,
  parseCopyLog,
  type ParsedCopyLog,
} from 'koroku-console/utils/db-copy';
import type { DbCopy } from 'koroku-console/utils/types';
import Banner from './banner';
import Button, { buttonClass } from './button';
import DeploymentSteps from './deployment-steps';
import Icon from './icon';

type StreamState = 'streaming' | 'ended' | 'failed';

interface Signature {
  Element: HTMLDivElement;
  Args: {
    copy: DbCopy;
    /** The log reached its end: the copy has a final status now. */
    onLogEnd?: () => unknown;
  };
}

/**
 * The steps of one database copy with their logs, live over SSE while it
 * runs, and how it ended.
 */
export default class DbCopyDetail extends Component<Signature> {
  @service declare api: ApiService;
  @service('event-source') declare eventSource: EventSourceService;

  buffer = new DeployLogBuffer();

  @tracked streamState: StreamState = 'streaming';

  private stream: Stream | null = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.open();
    registerDestructor(this, () => {
      this.closeStream();
      this.buffer.stop();
    });
  }

  @cached
  get log(): ParsedCopyLog {
    return parseCopyLog(this.buffer.lines, this.args.copy);
  }

  get logTextUrl(): string {
    return `${this.api.dbCopyLogsUrl(this.args.copy.id)}?format=text`;
  }

  get isFailed(): boolean {
    return this.args.copy.status === 'failed';
  }

  get failedStepText(): string {
    const step = this.log.steps.find((entry) => entry.state === 'failed');
    return step ? `「${copyStepLabel(step.key)}」の手順で失敗しました。` : '';
  }

  get streamFailed(): boolean {
    return this.streamState === 'failed';
  }

  get isWaiting(): boolean {
    return this.streamState === 'streaming' && this.log.steps.length === 0;
  }

  reconnect = () => {
    this.closeStream();
    this.buffer.clear();
    this.streamState = 'streaming';
    this.open();
  };

  private open(): void {
    this.stream = this.eventSource.open(
      this.api.dbCopyLogsUrl(this.args.copy.id),
      {
        message: (line) => this.buffer.push(line),
        end: () => {
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
      },
    );
  }

  private closeStream(): void {
    this.stream?.close();
    this.stream = null;
  }

  <template>
    <div class="deploy-detail copy-detail" data-test-copy-detail ...attributes>
      <div class="deploy-detail-bar">
        <p class="copy-detail-title">
          コピー #{{@copy.id}}
          {{#if this.isFailed}}
            <span class="visually-hidden">失敗</span>
          {{/if}}
        </p>
        <div class="deploy-detail-actions">
          <a
            class={{buttonClass "default" "small"}}
            href={{this.logTextUrl}}
            target="_blank"
            rel="noopener noreferrer"
            data-test-copy-log-text
          >全文を開く<Icon @name="external" /></a>
        </div>
      </div>

      {{#if this.isFailed}}
        <Banner
          @tone="error"
          @title="コピーに失敗しました"
          data-test-copy-failed
        >
          {{this.failedStepText}}
          <span class="deploy-error-reason">{{@copy.error}}</span>
          {{#if @copy.backup_snapshot_id}}
            <span class="banner-detail">
              コピー前のデータはスナップショット
              <code>{{@copy.backup_snapshot_id}}</code>
              に保存してあります。
            </span>
          {{/if}}
        </Banner>
      {{/if}}

      {{#if this.log.leftStopped}}
        <Banner
          @tone="warning"
          @title="止めたサービスは止めたままです"
          data-test-copy-left-stopped
        >
          コピー後のコマンドが最後まで実行されなかったため、消すべき情報が残っているかもしれません。コピーをやり直すか、確認してからアプリを起動してください。
        </Banner>
      {{/if}}

      {{#if this.streamFailed}}
        <Banner @tone="error" data-test-copy-stream-error>
          <:default>ログに接続できませんでした。</:default>
          <:actions>
            <Button @size="small" {{on "click" this.reconnect}}>再接続</Button>
          </:actions>
        </Banner>
      {{/if}}

      {{#if this.log.steps.length}}
        <DeploymentSteps
          @steps={{this.log.steps}}
          @label={{copyStepLabel}}
          @listLabel="コピーの手順"
        />
      {{else if this.isWaiting}}
        <p class="deploy-note">ログを待っています…</p>
      {{else}}
        <p class="deploy-note">このコピーのログは残っていません。</p>
      {{/if}}
    </div>
  </template>
}
