import Component from '@glimmer/component';
import { cached, tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { array, eq } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import { cancel, debounce, later, schedule } from '@ember/runloop';
import { isDestroying, registerDestructor } from '@ember/destroyable';
import type Owner from '@ember/owner';
import { modifier } from 'ember-modifier';
import { pageTitle } from 'ember-page-title';
import Banner from 'koroku-console/components/banner';
import Button, { buttonClass } from 'koroku-console/components/button';
import EmptyState from 'koroku-console/components/empty-state';
import Icon from 'koroku-console/components/icon';
import LogArchives from 'koroku-console/components/log-archives';
import LogDetail from 'koroku-console/components/log-detail';
import LogStream from 'koroku-console/components/log-stream';
import ToggleSwitch from 'koroku-console/components/toggle-switch';
import type AppLogsController from 'koroku-console/controllers/app/logs';
import type ApiService from 'koroku-console/services/api';
import type EventSourceService from 'koroku-console/services/event-source';
import type { Stream } from 'koroku-console/services/event-source';
import type StorageService from 'koroku-console/services/storage';
import type AppContext from 'koroku-console/utils/app-context';
import { inputValue, preventDefault } from 'koroku-console/utils/dom';
import { errorMessage } from 'koroku-console/utils/errors';
import Loadable from 'koroku-console/utils/loadable';
import LogBuffer, { type LogRow } from 'koroku-console/utils/log-buffer';
import {
  ACCESS_SERVICE,
  LOCAL_RETENTION_DAYS,
  PERIODS,
  formatCount,
  isBeyondRetention,
  isPeriod,
  jstInputToIso,
  parseLogLine,
  periodSpan,
  serviceLabel,
  toJstInput,
  type Period,
} from 'koroku-console/utils/logs';
import type {
  LogArchive,
  LogLine,
  ServiceStatus,
} from 'koroku-console/utils/types';

/** Lines the live tail starts with. */
const TAIL = 1000;
/** Rows kept on screen: the tail drops the oldest beyond this. */
const TAIL_LIMIT = 2000;
/** Lines per search request; "load more" asks for the next page. */
const PAGE = 1000;
/** Rows a search result may grow to before the oldest are dropped. */
const HISTORY_LIMIT = 5000;
/** How long typing may pause before the search runs. */
const SEARCH_DELAY = 400;

/** How long a connected tail may stay silent before it counts as empty. */
const QUIET_DELAY = 500;

const WRAP_KEY = 'koroku.logs.wrap';
const HOUR = 60 * 60 * 1000;

type Timer = ReturnType<typeof later>;

type Mode = 'tail' | 'history' | 'archive';

type LiveState = 'connecting' | 'streaming' | 'reconnecting' | 'off' | 'failed';

interface SearchQuery {
  service: string;
  from: string;
  to: string;
  q: string;
  limit: number;
}

/** What to say, and the one thing to do, where there are no rows. */
interface Empty {
  title: string;
  description: string;
  /** The label of the one next step. */
  action?: string;
  run?: () => unknown;
  /** The next step is the deploy tab instead of a button. */
  deploy?: boolean;
}

/**
 * Lines of the next page that the previous page already ended with: the
 * next page starts at the time of the last row, inclusive.
 */
function withoutOverlap(lines: LogLine[], rows: LogRow[]): LogLine[] {
  const last = rows[rows.length - 1];
  if (!last) return lines;
  const seen = new Set<string>();
  for (let i = rows.length - 1; i >= 0 && rows[i]?.time === last.time; i--) {
    const row = rows[i] as LogRow;
    seen.add(`${row.service}\n${row.line}`);
  }
  return lines.filter(
    (line) =>
      line.time !== last.time || !seen.has(`${line.service}\n${line.line}`),
  );
}

interface Signature {
  Args: { model: AppContext; controller: AppLogsController };
}

/**
 * The log tab. One toolbar decides what the list shows: the live tail (the
 * newest lines, followed as they arrive), a search over what the host still
 * keeps, or the archives of what it no longer has.
 */
export default class AppLogsTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service('event-source') declare eventSource: EventSourceService;
  @service declare storage: StorageService;

  services = new Loadable<ServiceStatus[]>(() =>
    this.api.listServices(this.slug),
  );
  archives = new Loadable<LogArchive[]>(() =>
    this.api.listLogArchives(this.slug, this.service),
  );
  tail = new LogBuffer(TAIL_LIMIT);
  history = new LogBuffer(HISTORY_LIMIT);

  /** The live tail switch. It only matters while the period is "最新". */
  @tracked live = true;
  @tracked liveState: LiveState = 'connecting';
  /** The tail is connected and had time to send what it has. */
  @tracked tailSettled = false;
  /** The reader is at the end of the tail (they did not scroll up). */
  @tracked following = true;
  @tracked selected: LogRow | null = null;
  @tracked wrap = true;

  @tracked searching = false;
  @tracked searchError: unknown = null;
  @tracked truncated = false;
  @tracked rangeError = '';
  /** Counts search results, to give each its own freshly scrolled list. */
  @tracked generation = 0;

  private stream: Stream | null = null;
  private sequence = 0;
  private lastQuery: SearchQuery | null = null;
  private lastAttempt: (() => unknown) | null = null;
  private body: HTMLElement | null = null;
  private pendingSearch: Timer | undefined;
  private quietTimer: Timer | undefined;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.wrap = this.storage.get(WRAP_KEY) !== '0';
    void this.services.load();
    this.apply();
    registerDestructor(this, () => {
      this.disconnect();
      this.tail.stop();
      this.cancelSearch();
    });
  }

  // --- What is selected (the URL holds it) --------------------------------

  get slug(): string {
    return this.args.model.slug;
  }

  get filters(): AppLogsController {
    return this.args.controller;
  }

  get service(): string {
    return this.filters.service;
  }

  get period(): Period {
    return isPeriod(this.filters.period) ? this.filters.period : 'tail';
  }

  get keyword(): string {
    return this.filters.q;
  }

  get mode(): Mode {
    if (this.period === 'tail') return 'tail';
    return this.period === 'archive' ? 'archive' : 'history';
  }

  get isCustom(): boolean {
    return this.period === 'custom';
  }

  get isArchive(): boolean {
    return this.mode === 'archive';
  }

  get isTail(): boolean {
    return this.mode === 'tail';
  }

  /** Service names for the switch: the running ones, then any seen in logs. */
  get serviceNames(): string[] {
    const names: string[] = [];
    const add = (name: string) => {
      if (name && name !== ACCESS_SERVICE && !names.includes(name)) {
        names.push(name);
      }
    };
    (this.services.value ?? []).forEach((status) => add(status.name));
    this.tail.services.forEach(add);
    this.history.services.forEach(add);
    (this.archives.value ?? []).forEach((archive) => add(archive.service));
    add(this.service);
    // The access log of the reverse proxy is a service of its own.
    return [...names, ACCESS_SERVICE];
  }

  get showService(): boolean {
    return this.service === '';
  }

  get serviceWidth(): number {
    const longest = Math.max(...this.serviceNames.map((name) => name.length));
    return Math.min(20, Math.max(4, longest));
  }

  // --- What is on screen --------------------------------------------------

  get buffer(): LogBuffer {
    return this.isTail ? this.tail : this.history;
  }

  /** The tail is filtered here; a search was already filtered by the API. */
  get rows(): LogRow[] {
    const { rows } = this.buffer;
    const keyword = this.keyword;
    if (!this.isTail || !keyword) return rows;
    return rows.filter((row) => row.line.includes(keyword));
  }

  get isFiltered(): boolean {
    return this.isTail && this.keyword !== '';
  }

  /** The reader is at the end of the tail (a search is never followed). */
  get tailFollowing(): boolean {
    return this.isTail && this.following;
  }

  /** New lines flow into the list (and it scrolls with them). */
  get flowing(): boolean {
    return this.isTail && this.following && this.selected === null;
  }

  /** The tail is connected but the list is kept still. */
  get paused(): boolean {
    return (
      this.isTail &&
      !this.flowing &&
      (this.liveState === 'streaming' || this.liveState === 'reconnecting')
    );
  }

  get liveOn(): boolean {
    return this.isTail && this.live;
  }

  get busy(): boolean {
    if (this.isTail) {
      return (
        this.liveState === 'connecting' || this.liveState === 'reconnecting'
      );
    }
    return this.searching;
  }

  get stateText(): string {
    if (!this.isTail) {
      if (this.searching) return '検索中…';
      if (this.searchError) return '取得できませんでした';
      return (
        PERIODS.find((option) => option.value === this.period)?.label ?? ''
      );
    }
    switch (this.liveState) {
      case 'connecting':
        return '接続中…';
      case 'reconnecting':
        return '接続が切れました。再接続中…';
      case 'failed':
        return '未接続';
      case 'off':
        return 'ライブ停止中';
      default:
        return this.flowing ? 'ライブ受信中' : '追従を一時停止中';
    }
  }

  get isReceiving(): boolean {
    return this.isTail && this.liveState === 'streaming' && this.flowing;
  }

  get countText(): string {
    const shown = formatCount(this.rows.length);
    if (this.isFiltered) {
      return `${shown} / ${formatCount(this.tail.rows.length)} 行が一致`;
    }
    return `${shown} 行`;
  }

  /** Nothing to show yet, and it is too early to call that "empty". */
  get loadingFirst(): boolean {
    if (this.rows.length > 0 || this.isFiltered) return false;
    if (this.busy) return true;
    return this.isTail && this.liveState === 'streaming' && !this.tailSettled;
  }

  get showTruncated(): boolean {
    return this.mode === 'history' && this.truncated;
  }

  get liveFailed(): boolean {
    return this.isTail && this.liveState === 'failed';
  }

  get historyError(): unknown {
    return this.isTail ? null : this.searchError;
  }

  get rangeBeyondRetention(): boolean {
    return (
      this.isCustom &&
      isBeyondRetention(jstInputToIso(this.filters.from), Date.now())
    );
  }

  @cached
  get empty(): Empty | null {
    if (this.rows.length > 0 || this.loadingFirst) return null;
    if (this.liveFailed || this.historyError || this.rangeError) return null;

    if (this.isTail) {
      if (this.isFiltered && this.tail.rows.length > 0) {
        return {
          title: `「${this.keyword}」に一致する行はありません`,
          description: `最新の ${formatCount(this.tail.rows.length)} 行から絞り込んでいます。大文字と小文字は区別されます。`,
          action: '直近 24 時間を検索',
          run: () => this.setPeriod('24h'),
        };
      }
      if (this.liveState === 'off') {
        return {
          title: 'ライブ tail は停止中です',
          description: '再開すると、最新の行から表示し直します。',
          action: 'ライブ tail を再開',
          run: () => this.setLive(true),
        };
      }
      if (!this.args.model.app.current_deployment) {
        return {
          title: 'まだログがありません',
          description:
            'アプリをデプロイすると、各サービスの出力がここに流れます。',
          action: 'デプロイへ進む',
          deploy: true,
        };
      }
      return {
        title: 'まだログがありません',
        description: 'サービスが何か出力すると、ここに流れます。',
        action: '直近 24 時間を表示',
        run: () => this.setPeriod('24h'),
      };
    }

    const description = `サーバーに残るのは直近 ${LOCAL_RETENTION_DAYS} 日分です。`;
    if (this.keyword) {
      return {
        title: 'この条件に一致するログはありません',
        description: 'キーワードは大文字と小文字を区別します。',
        action: 'キーワードを消す',
        run: () => this.setKeyword(''),
      };
    }
    if (this.period !== '7d' && !this.isCustom) {
      return {
        title: 'この期間のログはありません',
        description,
        action: '直近 7 日に広げる',
        run: () => this.setPeriod('7d'),
      };
    }
    return {
      title: 'この期間のログはありません',
      description,
      action: 'アーカイブを見る',
      run: () => this.setPeriod('archive'),
    };
  }

  // --- Applying the filters -----------------------------------------------

  /** Shows what the filters ask for: reconnects, searches or lists. */
  private apply(): void {
    this.selected = null;
    this.sequence++;
    this.searching = false;
    this.searchError = null;
    this.rangeError = '';
    this.cancelSearch();

    if (this.mode === 'tail') {
      if (this.live) {
        this.connect();
      } else {
        // The tail is switched off: rows of another filter must not stay.
        this.disconnect();
        this.tail.clear();
        this.liveState = 'off';
      }
      return;
    }
    this.disconnect();
    if (this.mode === 'archive') {
      void this.archives.load();
    } else {
      // Rows of an earlier search would look like the answer to this one.
      this.history.clear();
      this.truncated = false;
      void this.search();
    }
  }

  private cancelSearch(): void {
    // eslint-disable-next-line ember/no-runloop
    if (this.pendingSearch) cancel(this.pendingSearch);
    this.pendingSearch = undefined;
  }

  private connect(): void {
    this.disconnect();
    this.tail.clear();
    this.following = true;
    this.tail.release();
    this.liveState = 'connecting';
    this.tailSettled = false;
    const url = this.api.liveLogsUrl(this.slug, {
      service: this.service,
      tail: TAIL,
    });
    this.stream = this.eventSource.open(url, {
      open: () => {
        // After a reconnect the server sends the tail again.
        if (this.liveState === 'reconnecting') {
          this.tail.clear();
          this.selected = null;
          this.syncHold();
        }
        this.streaming();
      },
      message: (data) => {
        this.streaming();
        this.tail.push(parseLogLine(data));
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

  /** The stream is up. A moment later, having no rows means "empty". */
  private streaming(): void {
    if (this.liveState === 'streaming') return;
    this.liveState = 'streaming';
    // A run loop timer, so tests' `settled()` waits for it.
    // eslint-disable-next-line ember/no-runloop
    this.quietTimer ??= later(() => {
      this.quietTimer = undefined;
      this.tailSettled = true;
    }, QUIET_DELAY);
  }

  private disconnect(): void {
    this.stream?.close();
    this.stream = null;
    // eslint-disable-next-line ember/no-runloop
    if (this.quietTimer) cancel(this.quietTimer);
    this.quietTimer = undefined;
  }

  /** Lines wait while the reader looks at older ones or at a row. */
  private syncHold(): void {
    if (this.flowing) {
      this.tail.release();
    } else {
      this.tail.hold();
    }
  }

  /** The range to search, or null (with `rangeError` set) when invalid. */
  private range(): { from: string; to: string } | null {
    const span = periodSpan(this.period);
    if (span !== undefined) {
      return { from: new Date(Date.now() - span).toISOString(), to: '' };
    }
    const from = jstInputToIso(this.filters.from);
    const to = jstInputToIso(this.filters.to);
    if (from && to && to <= from) {
      this.rangeError = '終了は開始より後の時刻にしてください。';
      return null;
    }
    return { from, to };
  }

  search = async (): Promise<void> => {
    this.cancelSearch();
    if (this.mode !== 'history') return;
    this.rangeError = '';
    const range = this.range();
    if (!range) {
      this.sequence++;
      this.searching = false;
      this.history.clear();
      this.truncated = false;
      return;
    }
    const query: SearchQuery = {
      service: this.service,
      ...range,
      q: this.keyword,
      limit: PAGE,
    };
    this.lastQuery = query;
    await this.fetch(query, false);
  };

  private searchSoon(): void {
    // eslint-disable-next-line ember/no-runloop
    this.pendingSearch = debounce(this, this.search, SEARCH_DELAY);
  }

  private async fetch(query: SearchQuery, more: boolean): Promise<void> {
    const sequence = ++this.sequence;
    this.lastAttempt = () => this.fetch(query, more);
    this.searching = true;
    this.searchError = null;
    try {
      const result = await this.api.searchLogs(this.slug, query);
      if (sequence !== this.sequence || isDestroying(this)) return;
      const lines = result.lines ?? [];
      if (more) {
        this.history.append(withoutOverlap(lines, this.history.rows));
      } else {
        this.history.replace(lines);
        this.selected = null;
        this.generation++;
      }
      this.truncated = result.truncated;
    } catch (error) {
      if (sequence !== this.sequence || isDestroying(this)) return;
      this.searchError = error;
      if (!more) {
        this.history.clear();
        this.truncated = false;
      }
    } finally {
      if (sequence === this.sequence && !isDestroying(this)) {
        this.searching = false;
      }
    }
  }

  // --- Toolbar ------------------------------------------------------------

  changeService = (event: Event) => {
    this.filters.service = inputValue(event);
    this.apply();
  };

  changePeriod = (event: Event) => {
    const value = inputValue(event);
    this.setPeriod(isPeriod(value) ? value : 'tail');
  };

  setPeriod = (period: Period) => {
    this.filters.period = period;
    if (period === 'tail') this.live = true;
    if (period === 'custom' && !this.filters.from && !this.filters.to) {
      // Start from something sensible rather than from all 14 days.
      const now = Date.now();
      this.filters.from = toJstInput(now - HOUR);
      this.filters.to = toJstInput(now);
    }
    this.apply();
  };

  changeKeyword = (event: Event) => {
    this.setKeyword(inputValue(event), { soon: true });
  };

  setKeyword = (keyword: string, options: { soon?: boolean } = {}) => {
    this.filters.q = keyword;
    if (this.mode !== 'history') return;
    if (options.soon) {
      this.searchSoon();
    } else {
      void this.search();
    }
  };

  changeFrom = (event: Event) => {
    this.filters.from = inputValue(event);
    this.searchSoon();
  };

  changeTo = (event: Event) => {
    this.filters.to = inputValue(event);
    this.searchSoon();
  };

  setLive = (live: boolean) => {
    this.live = live;
    if (!live) {
      // Stop receiving; what is on screen stays to be read.
      if (this.isTail) {
        this.disconnect();
        this.liveState = 'off';
      }
      return;
    }
    if (this.period !== 'tail') this.filters.period = 'tail';
    this.apply();
  };

  setWrap = (wrap: boolean) => {
    this.wrap = wrap;
    this.storage.set(WRAP_KEY, wrap ? '1' : '0');
  };

  // --- The list -----------------------------------------------------------

  private get viewport(): HTMLElement | null {
    return this.body?.querySelector<HTMLElement>('.log-viewport') ?? null;
  }

  select = (row: LogRow | null) => {
    const closing = row === null && this.selected !== null;
    this.selected = row;
    this.syncHold();
    if (closing && this.isTail) {
      // The detail gave its room back to the list, and the newest row may
      // be in view again: have the follow modifier look, as after a scroll.
      // eslint-disable-next-line ember/no-runloop
      schedule('afterRender', () => {
        this.viewport?.dispatchEvent(new Event('scroll'));
      });
    }
  };

  closeDetail = () => {
    this.select(null);
    // The button that had focus is gone; hand it back to the list.
    this.viewport?.focus();
  };

  scrolled = (atEnd: boolean) => {
    if (!this.isTail || this.following === atEnd) return;
    this.following = atEnd;
    this.syncHold();
  };

  resume = () => {
    this.selected = null;
    this.following = true;
    this.syncHold();
  };

  reconnect = () => this.connect();

  loadMore = () => {
    const last = this.history.rows[this.history.rows.length - 1];
    if (!last || !this.lastQuery) return;
    void this.fetch({ ...this.lastQuery, from: last.time }, true);
  };

  retry = () => {
    void this.lastAttempt?.();
  };

  showRecent = () => this.setPeriod('24h');
  showArchives = () => this.setPeriod('archive');

  bodyElement = modifier<{ Element: HTMLElement }>((element) => {
    this.body = element;
  });

  <template>
    {{pageTitle "ログ"}}

    <section class="logs-page" aria-label="ログ" data-test-logs>
      <form
        class="logs-toolbar"
        role="search"
        aria-label="ログの絞り込み"
        {{on "submit" (preventDefault this.search)}}
      >
        <div class="logs-filters">
          <label class="visually-hidden" for="log-service">サービス</label>
          <select
            id="log-service"
            class="logs-select"
            data-test-log-service
            {{on "change" this.changeService}}
          >
            <option value="" selected={{eq this.service ""}}>
              すべてのサービス
            </option>
            {{#each this.serviceNames as |name|}}
              <option value={{name}} selected={{eq name this.service}}>
                {{serviceLabel name}}
              </option>
            {{/each}}
          </select>

          <label class="visually-hidden" for="log-period">期間</label>
          <select
            id="log-period"
            class="logs-select"
            data-test-log-period
            {{on "change" this.changePeriod}}
          >
            {{#each PERIODS as |choice|}}
              <option
                value={{choice.value}}
                selected={{eq choice.value this.period}}
              >{{choice.label}}</option>
            {{/each}}
          </select>

          {{#unless this.isArchive}}
            <label class="visually-hidden" for="log-keyword">キーワード</label>
            <input
              id="log-keyword"
              type="search"
              class="logs-keyword"
              placeholder="キーワードで絞り込み"
              autocomplete="off"
              spellcheck="false"
              value={{this.keyword}}
              data-test-log-keyword
              {{on "input" this.changeKeyword}}
            />
          {{/unless}}
        </div>

        {{#unless this.isArchive}}
          <div class="logs-toggles">
            <ToggleSwitch
              @checked={{this.liveOn}}
              @onChange={{this.setLive}}
              data-test-log-live
            >ライブ</ToggleSwitch>
            <ToggleSwitch
              @checked={{this.wrap}}
              @onChange={{this.setWrap}}
              data-test-log-wrap
            >折り返し</ToggleSwitch>
          </div>
        {{/unless}}

        {{#if this.isCustom}}
          <div class="logs-range" data-test-log-range>
            <span class="logs-range-field">
              <label for="log-from">開始</label>
              <input
                id="log-from"
                type="datetime-local"
                class={{if this.rangeError "is-invalid"}}
                value={{this.filters.from}}
                aria-describedby="log-range-note"
                data-test-log-from
                {{on "change" this.changeFrom}}
              />
            </span>
            <span class="logs-range-field">
              <label for="log-to">終了</label>
              <input
                id="log-to"
                type="datetime-local"
                class={{if this.rangeError "is-invalid"}}
                value={{this.filters.to}}
                aria-describedby="log-range-note"
                data-test-log-to
                {{on "change" this.changeTo}}
              />
            </span>
            <span class="logs-range-note" id="log-range-note">
              {{#if this.rangeError}}
                <span
                  class="field-error"
                  role="alert"
                  data-test-log-range-error
                >
                  {{this.rangeError}}
                </span>
              {{else if this.rangeBeyondRetention}}
                <span data-test-log-range-old>
                  日本時間。{{LOCAL_RETENTION_DAYS}}
                  日より前の分は
                  <button
                    type="button"
                    class="logs-link"
                    {{on "click" this.showArchives}}
                  >アーカイブ</button>
                  にあります。
                </span>
              {{else}}
                日本時間（JST）
              {{/if}}
            </span>
          </div>
        {{/if}}
      </form>

      {{#if this.isArchive}}
        <LogArchives
          @slug={{this.slug}}
          @archives={{this.archives}}
          @onShowRecent={{this.showRecent}}
        />
      {{else}}
        <div class="logs-body" {{this.bodyElement}}>
          {{#if this.liveFailed}}
            <Banner
              @tone="error"
              @title="ライブログに接続できませんでした"
              data-test-log-error
            >
              <:default>
                サーバーに届かないか、この機能がまだ利用できない可能性があります。
              </:default>
              <:actions>
                <Button
                  @size="small"
                  data-test-log-retry
                  {{on "click" this.reconnect}}
                >再接続</Button>
              </:actions>
            </Banner>
          {{else if this.historyError}}
            <Banner
              @tone="error"
              @title="ログを取得できませんでした"
              data-test-log-error
            >
              <:default>{{errorMessage this.historyError}}</:default>
              <:actions>
                <Button
                  @size="small"
                  data-test-log-retry
                  {{on "click" this.retry}}
                >再試行</Button>
              </:actions>
            </Banner>
          {{/if}}

          {{#if this.rows.length}}
            {{! A new search result gets a new list, scrolled to its start. }}
            {{#each (array this.generation) key="@identity"}}
              <LogStream
                @rows={{this.rows}}
                @label={{if this.isTail "ライブログ" "検索結果"}}
                @keyword={{this.keyword}}
                @wrap={{this.wrap}}
                @showService={{this.showService}}
                @serviceWidth={{this.serviceWidth}}
                @following={{this.tailFollowing}}
                @paused={{this.paused}}
                @waiting={{this.tail.waiting}}
                @selected={{this.selected}}
                @onSelect={{this.select}}
                @onFollowChange={{this.scrolled}}
                @onResume={{this.resume}}
              />
            {{/each}}
          {{else if this.loadingFirst}}
            <p class="logs-placeholder" role="status" data-test-loading>
              <Icon @name="spinner" />
              {{this.stateText}}
            </p>
          {{else if this.empty}}
            <EmptyState
              class="logs-empty"
              @title={{this.empty.title}}
              @description={{this.empty.description}}
              data-test-log-empty
            >
              <:action>
                {{#if this.empty.deploy}}
                  <LinkTo
                    @route="app.deploy"
                    @model={{this.slug}}
                    class={{buttonClass "default"}}
                    data-test-empty-action
                  >{{this.empty.action}}</LinkTo>
                {{else if this.empty.run}}
                  <Button
                    data-test-empty-action
                    {{on "click" this.empty.run}}
                  >{{this.empty.action}}</Button>
                {{/if}}
              </:action>
            </EmptyState>
          {{/if}}

          {{#if this.selected}}
            <LogDetail @row={{this.selected}} @onClose={{this.closeDetail}} />
          {{/if}}
        </div>

        <footer class="logs-status">
          <span
            class="logs-state {{if this.isReceiving 'is-receiving'}}"
            role="status"
            data-test-log-state
          >
            {{#if this.busy}}
              <Icon @name="spinner" />
            {{else if this.isReceiving}}
              <svg
                class="icon icon-fill"
                viewBox="0 0 16 16"
                aria-hidden="true"
              >
                <circle cx="8" cy="8" r="4" />
              </svg>
            {{else if this.isTail}}
              <svg class="icon" viewBox="0 0 16 16" aria-hidden="true">
                <path d="M5.75 4v8M10.25 4v8" />
              </svg>
            {{/if}}
            {{this.stateText}}
          </span>

          <span class="logs-count" data-test-log-count>
            {{this.countText}}
            {{#if this.buffer.dropped}}
              <span data-test-log-dropped>
                （上限
                {{formatCount this.buffer.limit}}
                行。古い行は表示から外しました）
              </span>
            {{/if}}
          </span>

          {{#if this.showTruncated}}
            <span class="logs-truncated" data-test-log-truncated>
              <span class="logs-truncated-text">
                <Icon @name="info" />
                一致するログが多いため、古い方から途中までを表示しています。
              </span>
              <Button
                @size="small"
                @busy={{this.searching}}
                data-test-log-more
                {{on "click" this.loadMore}}
              >続きを読み込む</Button>
            </span>
          {{/if}}
        </footer>
      {{/if}}
    </section>
  </template>
}
