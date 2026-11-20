import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { registerDestructor } from '@ember/destroyable';
import type Owner from '@ember/owner';
import type ApiService from 'koroku-console/services/api';
import type EventSourceService from 'koroku-console/services/event-source';
import type { Stream } from 'koroku-console/services/event-source';
import DeployLogBuffer from 'koroku-console/utils/deploy-log-buffer';
import { formatDateTimeJst } from 'koroku-console/utils/format';
import type { TaskRun } from 'koroku-console/utils/types';
import Banner, { type BannerTone } from './banner';
import Button, { buttonClass } from './button';
import CopyButton from './copy-button';
import Icon from './icon';
import StepLog from './step-log';

type StreamState = 'streaming' | 'ended' | 'failed';

const STREAM_LABELS: Record<StreamState, string> = {
  streaming: '出力を受信中…',
  ended: '出力の終わりです。',
  failed: '出力に接続できませんでした。',
};

interface Notice {
  tone: BannerTone;
  title: string;
  text: string;
  detail: string;
}

/** How a run ended, in the reader's words; nothing for a success. */
function noticeFor(run: TaskRun): Notice | null {
  switch (run.status) {
    case 'failed':
      if (run.exit_code !== null) {
        return {
          tone: 'error',
          title: `終了コード ${run.exit_code} で終了しました`,
          text: 'コマンドが失敗を返しました。原因は出力の末尾にあることが多いです。',
          detail: '',
        };
      }
      return {
        tone: 'error',
        title: 'コマンドを最後まで実行できませんでした',
        text: '',
        detail: run.error,
      };
    case 'canceled':
      return {
        tone: 'info',
        title: 'キャンセルしました',
        text: 'コマンドを止め、実行していたコンテナを削除しました。それまでに行った変更は残っています。',
        detail: '',
      };
    default:
      return null;
  }
}

interface TaskRunDetailSignature {
  Element: HTMLDivElement;
  Args: {
    run: TaskRun;
    /** The output reached its end: the run has a final status now. */
    onLogEnd?: () => unknown;
  };
}

/**
 * The output of one run of a command, live over SSE while it runs, and how
 * it ended. The log is the same kind of file as a deploy log (masked, one
 * line per message), shown as one block instead of steps.
 */
export default class TaskRunDetail extends Component<TaskRunDetailSignature> {
  @service declare api: ApiService;
  @service('event-source') declare eventSource: EventSourceService;

  buffer = new DeployLogBuffer();

  @tracked streamState: StreamState = 'streaming';

  private stream: Stream | null = null;

  constructor(owner: Owner, args: TaskRunDetailSignature['Args']) {
    super(owner, args);
    this.open();
    registerDestructor(this, () => {
      this.closeStream();
      this.buffer.stop();
    });
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

  get isRunning(): boolean {
    return this.args.run.status === 'running';
  }

  get isFailed(): boolean {
    return this.args.run.status === 'failed';
  }

  get notice(): Notice | null {
    return noticeFor(this.args.run);
  }

  get logTextUrl(): string {
    return `${this.api.taskRunLogsUrl(this.args.run.id)}?format=text`;
  }

  get hasLines(): boolean {
    return this.buffer.lines.length > 0;
  }

  get isEmpty(): boolean {
    return this.streamState === 'ended' && !this.hasLines;
  }

  reconnect = () => {
    this.closeStream();
    this.buffer.clear();
    this.streamState = 'streaming';
    this.open();
  };

  private open(): void {
    const url = this.api.taskRunLogsUrl(this.args.run.id);
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
    <div
      class="deploy-detail task-run-detail"
      data-test-task-run-detail
      ...attributes
    >
      <div class="deploy-detail-bar">
        <dl class="deploy-facts">
          <div>
            <dt>実行</dt>
            <dd>#{{@run.id}}</dd>
          </div>
          <div>
            <dt>サービス</dt>
            <dd class="mono">{{@run.service}}</dd>
          </div>
          {{#if @run.deployment_id}}
            <div>
              <dt>リリース</dt>
              <dd>デプロイ #{{@run.deployment_id}}</dd>
            </div>
          {{/if}}
          <div>
            <dt>開始</dt>
            <dd>{{formatDateTimeJst @run.started_at}}</dd>
          </div>
        </dl>

        <div class="deploy-detail-actions">
          <span
            class="stream-state
              {{unless this.isStreaming 'visually-hidden'}}
              {{if this.isRunning 'is-live'}}"
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

      <div class="task-run-command">
        <code class="mono" data-test-run-command>{{@run.command}}</code>
        <CopyButton @text={{@run.command}} @label="コマンド" />
      </div>

      {{#if this.notice}}
        <Banner
          @tone={{this.notice.tone}}
          @title={{this.notice.title}}
          data-test-run-notice={{@run.status}}
        >
          {{this.notice.text}}
          {{#if this.notice.detail}}
            <span class="deploy-error-reason">{{this.notice.detail}}</span>
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
          出力が長いため、古い行を省いています。すべての行は「全文を開く」で確認できます。
        </p>
      {{/if}}

      {{#if this.hasLines}}
        <StepLog
          class="task-run-log"
          @lines={{this.buffer.lines}}
          @label="コマンドの出力"
          @live={{this.isRunning}}
          @highlightErrors={{this.isFailed}}
        />
      {{else if this.isEmpty}}
        <p class="deploy-note" data-test-no-log>
          この実行の出力は残っていません。
        </p>
      {{else if this.isStreaming}}
        <p class="deploy-note" data-test-log-waiting>出力を待っています…</p>
      {{/if}}
    </div>
  </template>
}
