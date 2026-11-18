import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn } from '@ember/helper';
import { registerDestructor } from '@ember/destroyable';
import type Owner from '@ember/owner';
import AsyncView from 'paas-console/components/async-view';
import ErrorMessage from 'paas-console/components/error-message';
import FeatureUnavailable from 'paas-console/components/feature-unavailable';
import LogView from 'paas-console/components/log-view';
import type ApiService from 'paas-console/services/api';
import type BrowserService from 'paas-console/services/browser';
import type EventSourceService from 'paas-console/services/event-source';
import type { Stream } from 'paas-console/services/event-source';
import type AppContext from 'paas-console/utils/app-context';
import { inputValue, preventDefault, setValue } from 'paas-console/utils/dom';
import {
  formatBytes,
  formatClock,
  formatDateTime,
  localInputToIso,
} from 'paas-console/utils/format';
import LineBuffer from 'paas-console/utils/line-buffer';
import Loadable from 'paas-console/utils/loadable';
import type {
  LogArchive,
  LogHistory,
  LogLine,
  ServiceStatus,
} from 'paas-console/utils/types';

const TAIL = 200;
const HISTORY_LIMIT = 1000;

type LiveState =
  'connecting' | 'streaming' | 'reconnecting' | 'paused' | 'failed';

const LIVE_STATUS: Record<LiveState, string> = {
  connecting: '接続中…',
  streaming: '受信中',
  reconnecting: '接続が切れました。再接続中…',
  paused: '停止中',
  failed:
    'ライブログに接続できませんでした。この機能がまだ利用できない可能性があります。',
};

function isLogLine(value: unknown): value is LogLine {
  return (
    typeof value === 'object' &&
    value !== null &&
    typeof (value as LogLine).line === 'string'
  );
}

/** `12:34:56 web | message`; a payload that is not a log line stays as is. */
function formatLogLine(line: LogLine): string {
  const clock = formatClock(line.time);
  const service = line.service ? `${line.service} | ` : '';
  return `${clock ? `${clock} ` : ''}${service}${line.line}`;
}

function formatHistoryLine(line: LogLine): string {
  const time = formatDateTime(line.time);
  const clock = formatClock(line.time);
  const stamp = time ? `${time.slice(0, 10)} ${clock} ` : '';
  return `${stamp}${line.service ? `${line.service} | ` : ''}${line.line}`;
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppLogsTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare browser: BrowserService;
  @service('event-source') declare eventSource: EventSourceService;

  services = new Loadable<ServiceStatus[]>(() =>
    this.api.listServices(this.slug),
  );
  result = new Loadable<LogHistory>(() =>
    this.api.searchLogs(this.slug, {
      service: this.searchService,
      from: localInputToIso(this.from),
      to: localInputToIso(this.to),
      q: this.keyword.trim(),
      limit: HISTORY_LIMIT,
    }),
  );
  archives = new Loadable<LogArchive[]>(() =>
    this.api.listLogArchives(this.slug, this.archiveService),
  );
  live = new LineBuffer(5000);

  @tracked liveService = '';
  @tracked liveState: LiveState = 'connecting';

  @tracked searchService = '';
  @tracked from = '';
  @tracked to = '';
  @tracked keyword = '';
  @tracked searched = false;

  @tracked archiveService = '';
  @tracked downloading: string | null = null;
  @tracked downloadError: unknown = null;

  private stream: Stream | null = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.services.load();
    void this.archives.load();
    this.connect();
    registerDestructor(this, () => {
      this.disconnect();
      this.live.stop();
    });
  }

  get slug(): string {
    return this.args.model.slug;
  }

  /** Names for the service filters; just "all" when they are unknown. */
  get serviceNames(): string[] {
    return (this.services.value ?? []).map((s) => s.name);
  }

  get liveStatus(): string {
    return LIVE_STATUS[this.liveState];
  }

  get paused(): boolean {
    return this.liveState === 'paused' || this.liveState === 'failed';
  }

  get resultText(): string {
    return (this.result.value?.lines ?? []).map(formatHistoryLine).join('\n');
  }

  private connect(): void {
    this.disconnect();
    this.liveState = 'connecting';
    const url = this.api.liveLogsUrl(this.slug, {
      service: this.liveService,
      tail: TAIL,
    });
    this.stream = this.eventSource.open(url, {
      open: () => {
        // After a reconnect the server sends the tail again.
        if (this.liveState === 'reconnecting') this.live.clear();
        this.liveState = 'streaming';
      },
      message: (data) => {
        this.liveState = 'streaming';
        this.live.push(this.parse(data));
      },
      error: (closed) => {
        if (closed) {
          this.disconnect();
          this.liveState = 'failed';
        } else {
          this.liveState = 'reconnecting';
        }
      },
    });
  }

  private parse(data: string): string {
    try {
      const parsed: unknown = JSON.parse(data);
      if (isLogLine(parsed)) return formatLogLine(parsed);
    } catch {
      // Not JSON: show the payload as it came.
    }
    return data;
  }

  private disconnect(): void {
    this.stream?.close();
    this.stream = null;
  }

  selectLiveService = (event: Event) => {
    this.liveService = inputValue(event);
    this.live.clear();
    this.connect();
  };

  toggleLive = () => {
    if (this.paused) {
      this.connect();
    } else {
      this.disconnect();
      this.liveState = 'paused';
    }
  };

  clearLive = () => this.live.clear();

  search = async () => {
    this.searched = true;
    await this.result.load();
  };

  selectArchiveService = (event: Event) => {
    this.archiveService = inputValue(event);
    void this.archives.load();
  };

  download = async (archive: LogArchive) => {
    this.downloading = archive.key;
    this.downloadError = null;
    try {
      const url = await this.api.logArchiveUrl(this.slug, archive.key);
      this.browser.assign(url);
    } catch (error) {
      this.downloadError = error;
    } finally {
      this.downloading = null;
    }
  };

  <template>
    <section class="card" aria-labelledby="live-title" data-test-live>
      <div class="card-header">
        <h3 id="live-title">ライブ</h3>
        <span class="muted" aria-live="polite" data-test-live-state>
          {{this.liveStatus}}
        </span>
      </div>

      <div class="toolbar">
        <label class="field field-inline" for="live-service">
          <span class="field-label">サービス</span>
          <select
            id="live-service"
            data-test-live-service
            {{on "change" this.selectLiveService}}
          >
            <option value="" selected={{eq this.liveService ""}}>すべて</option>
            {{#each this.serviceNames as |name|}}
              <option value={{name}} selected={{eq name this.liveService}}>
                {{name}}
              </option>
            {{/each}}
          </select>
        </label>
        <button
          type="button"
          class="button button-small"
          data-test-live-toggle
          {{on "click" this.toggleLive}}
        >{{if this.paused "再開" "一時停止"}}</button>
        <button
          type="button"
          class="button button-small"
          {{on "click" this.clearLive}}
        >表示を消去</button>
      </div>

      <LogView
        @text={{this.live.text}}
        @label="ライブログ"
        @empty="ログを待っています…"
        data-test-live-text
      />
      {{#if this.live.dropped}}
        <p
          class="muted"
        >古い行は表示から外しました。過去の分は下の検索で確認できます。</p>
      {{/if}}
    </section>

    <section class="card" aria-labelledby="search-title" data-test-search>
      <h3 id="search-title">過去ログの検索</h3>
      <p class="muted">サーバーに残っている過去 14 日分を検索します。</p>

      <form
        class="form"
        data-test-search-form
        {{on "submit" (preventDefault this.search)}}
      >
        <div class="field-row">
          <label class="field" for="search-service">
            <span class="field-label">サービス</span>
            <select
              id="search-service"
              data-test-search-service
              {{on "change" (setValue this "searchService")}}
            >
              <option value="" selected={{eq this.searchService ""}}>
                すべて
              </option>
              {{#each this.serviceNames as |name|}}
                <option value={{name}} selected={{eq name this.searchService}}>
                  {{name}}
                </option>
              {{/each}}
            </select>
          </label>
          <label class="field" for="search-from">
            <span class="field-label">開始</span>
            <input
              id="search-from"
              type="datetime-local"
              value={{this.from}}
              data-test-search-from
              {{on "input" (setValue this "from")}}
            />
          </label>
          <label class="field" for="search-to">
            <span class="field-label">終了</span>
            <input
              id="search-to"
              type="datetime-local"
              value={{this.to}}
              data-test-search-to
              {{on "input" (setValue this "to")}}
            />
          </label>
          <label class="field field-grow" for="search-keyword">
            <span class="field-label">キーワード</span>
            <input
              id="search-keyword"
              type="search"
              value={{this.keyword}}
              data-test-search-keyword
              {{on "input" (setValue this "keyword")}}
            />
          </label>
        </div>
        <div class="form-actions">
          <button
            type="submit"
            class="button"
            disabled={{this.result.loading}}
            data-test-search-submit
          >{{if this.result.loading "検索中…" "検索"}}</button>
        </div>
      </form>

      {{#if this.searched}}
        <AsyncView @data={{this.result}} as |result|>
          {{#if result.truncated}}
            <p
              class="notice"
              data-test-search-truncated
            >結果が多いため途中までを表示しています。期間やキーワードで絞り込んでください。</p>
          {{/if}}
          <LogView
            @text={{this.resultText}}
            @label="検索結果"
            @empty="該当するログはありません。"
            data-test-search-result
          />
        </AsyncView>
      {{/if}}
    </section>

    <section class="card" aria-labelledby="archives-title" data-test-archives>
      <h3 id="archives-title">退避済みのログ</h3>
      <p class="muted">オブジェクトストレージに退避したログです（1 ファイルが 1
        回の退避分）。</p>

      {{#if this.archives.unavailable}}
        <FeatureUnavailable @kind={{this.archives.unavailable}} />
      {{else}}
        <div class="toolbar">
          <label class="field field-inline" for="archive-service">
            <span class="field-label">サービス</span>
            <select
              id="archive-service"
              data-test-archive-service
              {{on "change" this.selectArchiveService}}
            >
              <option value="" selected={{eq this.archiveService ""}}>
                すべて
              </option>
              {{#each this.serviceNames as |name|}}
                <option value={{name}} selected={{eq name this.archiveService}}>
                  {{name}}
                </option>
              {{/each}}
            </select>
          </label>
        </div>

        <div aria-live="polite">
          <ErrorMessage @error={{this.downloadError}} />
        </div>

        <AsyncView @data={{this.archives}} as |archives|>
          {{#if archives.length}}
            <div class="table-wrap">
              <table class="table">
                <thead>
                  <tr>
                    <th scope="col">時刻</th>
                    <th scope="col">サービス</th>
                    <th scope="col">サイズ</th>
                    <th scope="col"><span
                        class="visually-hidden"
                      >操作</span></th>
                  </tr>
                </thead>
                <tbody>
                  {{#each archives key="key" as |archive|}}
                    <tr data-test-archive={{archive.key}}>
                      <td>{{formatDateTime archive.hour}}</td>
                      <td>{{archive.service}}</td>
                      <td>{{formatBytes archive.size}}</td>
                      <td class="table-actions">
                        <button
                          type="button"
                          class="button button-small"
                          disabled={{eq this.downloading archive.key}}
                          data-test-archive-download
                          {{on "click" (fn this.download archive)}}
                        >ダウンロード</button>
                      </td>
                    </tr>
                  {{/each}}
                </tbody>
              </table>
            </div>
          {{else}}
            <p
              class="muted"
              data-test-no-archives
            >退避済みのログはまだありません。</p>
          {{/if}}
        </AsyncView>
      {{/if}}
    </section>
  </template>
}
