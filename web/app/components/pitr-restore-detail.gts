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
import { parseCopyLog, type ParsedCopyLog } from 'koroku-console/utils/db-copy';
import { pitrStepLabel } from 'koroku-console/utils/pitr';
import type { PitrRestore } from 'koroku-console/utils/types';
import Banner from './banner';
import Button, { buttonClass } from './button';
import DeploymentSteps from './deployment-steps';
import Icon from './icon';

type StreamState = 'streaming' | 'ended' | 'failed';

interface Signature {
  Element: HTMLDivElement;
  Args: {
    slug: string;
    restore: PitrRestore;
    onLogEnd?: () => unknown;
  };
}

/** The steps of one restore to a point in time with their logs. */
export default class PitrRestoreDetail extends Component<Signature> {
  @service declare api: ApiService;
  @service('event-source') declare eventSource: EventSourceService;

  buffer = new DeployLogBuffer();

  @tracked streamState: StreamState = 'streaming';

  private stream: Stream | null = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.open();
    registerDestructor(this, () => {
      this.stream?.close();
      this.buffer.stop();
    });
  }

  get url(): string {
    return this.api.pitrRestoreLogsUrl(this.args.slug, this.args.restore.id);
  }

  @cached
  get log(): ParsedCopyLog {
    return parseCopyLog(this.buffer.lines, this.args.restore);
  }

  get failedStepText(): string {
    const step = this.log.steps.find((entry) => entry.state === 'failed');
    return step ? `「${pitrStepLabel(step.key)}」の手順で失敗しました。` : '';
  }

  reconnect = () => {
    this.stream?.close();
    this.buffer.clear();
    this.streamState = 'streaming';
    this.open();
  };

  private open(): void {
    this.stream = this.eventSource.open(this.url, {
      message: (line) => this.buffer.push(line),
      end: () => {
        this.stream?.close();
        this.buffer.flush();
        this.streamState = 'ended';
        void this.args.onLogEnd?.();
      },
      error: (closed) => {
        if (!closed) return;
        this.stream?.close();
        this.buffer.flush();
        this.streamState = 'failed';
      },
    });
  }

  <template>
    <div
      class="deploy-detail copy-detail"
      data-test-pitr-restore-detail
      ...attributes
    >
      <div class="deploy-detail-bar">
        <p class="copy-detail-title">時点指定の復旧 #{{@restore.id}}</p>
        <div class="deploy-detail-actions">
          <a
            class={{buttonClass "default" "small"}}
            href="{{this.url}}?format=text"
            target="_blank"
            rel="noopener noreferrer"
          >全文を開く<Icon @name="external" /></a>
        </div>
      </div>
      {{#if (isFailed @restore)}}
        <Banner
          @tone="error"
          @title="時点指定の復旧に失敗しました"
          data-test-pitr-restore-failed
        >
          {{this.failedStepText}}
          <span class="deploy-error-reason">{{@restore.error}}</span>
          <span class="banner-detail">
            DB
            は戻す前のデータに戻してあります。戻せなかったときは、ログの末尾に、退避したデータの場所が書いてあります。
          </span>
        </Banner>
      {{/if}}
      {{#if (eqState this.streamState "failed")}}
        <Banner @tone="error">
          <:default>ログに接続できませんでした。</:default>
          <:actions>
            <Button @size="small" {{on "click" this.reconnect}}>再接続</Button>
          </:actions>
        </Banner>
      {{/if}}
      {{#if this.log.steps.length}}
        <DeploymentSteps
          @steps={{this.log.steps}}
          @label={{pitrStepLabel}}
          @listLabel="時点指定の復旧の手順"
        />
      {{else if (eqState this.streamState "streaming")}}
        <p class="deploy-note">ログを待っています…</p>
      {{else}}
        <p class="deploy-note">ログは残っていません。</p>
      {{/if}}
    </div>
  </template>
}

function isFailed(restore: PitrRestore): boolean {
  return restore.status === 'failed';
}

function eqState(a: string, b: string): boolean {
  return a === b;
}
