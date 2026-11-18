import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { fn } from '@ember/helper';
import { on } from '@ember/modifier';
import type ApiService from 'paas-console/services/api';
import type BrowserService from 'paas-console/services/browser';
import { formatBytes, formatDateTimeJst } from 'paas-console/utils/format';
import type Loadable from 'paas-console/utils/loadable';
import {
  LOCAL_RETENTION_DAYS,
  formatCount,
  serviceLabel,
} from 'paas-console/utils/logs';
import type { LogArchive } from 'paas-console/utils/types';
import Banner from './banner';
import Button from './button';
import EmptyState from './empty-state';
import ErrorMessage from './error-message';

/** Rows added per "show more": a year of archives is thousands of files. */
const PAGE = 100;

interface LogArchivesSignature {
  Element: HTMLDivElement;
  Args: {
    slug: string;
    archives: Loadable<LogArchive[]>;
    /** The one next step when there is nothing to list. */
    onShowRecent: () => unknown;
  };
}

function isBusy(downloading: string | null, archive: LogArchive): boolean {
  return downloading === archive.key;
}

function downloadLabel(archive: LogArchive): string {
  return `${serviceLabel(archive.service)} ${formatDateTimeJst(archive.hour)} のログをダウンロード`;
}

/**
 * The logs that were moved to object storage: one compressed file per
 * service and archive run. They are not searched here; each is downloaded
 * through a short-lived presigned URL.
 */
export default class LogArchives extends Component<LogArchivesSignature> {
  @service declare api: ApiService;
  @service declare browser: BrowserService;

  @tracked shown = PAGE;
  @tracked downloading: string | null = null;
  @tracked downloadError: unknown = null;

  get all(): LogArchive[] {
    return this.args.archives.value ?? [];
  }

  get visible(): LogArchive[] {
    return this.all.slice(0, this.shown);
  }

  get remaining(): number {
    return Math.max(0, this.all.length - this.shown);
  }

  get notConfigured(): boolean {
    return this.args.archives.unavailable === 'not_configured';
  }

  showMore = () => {
    this.shown += PAGE;
  };

  download = async (archive: LogArchive) => {
    this.downloading = archive.key;
    this.downloadError = null;
    try {
      const url = await this.api.logArchiveUrl(this.args.slug, archive.key);
      this.browser.assign(url);
    } catch (error) {
      this.downloadError = error;
    } finally {
      this.downloading = null;
    }
  };

  <template>
    <div class="log-archives" data-test-log-archives ...attributes>
      <div class="log-archives-inner">
        {{#if this.notConfigured}}
          <EmptyState
            @title="ログのアーカイブは設定されていません"
            @description="オブジェクトストレージが未設定のため、見られるのはサーバーに残る直近 {{LOCAL_RETENTION_DAYS}} 日分だけです。サーバーの設定に保存先を加えると、それより前の分も定期的に退避されます。"
            role="status"
            data-test-archives-not-configured
          />
        {{else if @archives.error}}
          <Banner
            @tone="error"
            @title="アーカイブの一覧を取得できませんでした"
            data-test-error
          >
            <:default>{{@archives.errorMessage}}</:default>
            <:actions>
              <Button
                @size="small"
                {{on "click" @archives.reload}}
              >再読み込み</Button>
            </:actions>
          </Banner>
        {{else if @archives.loaded}}
          {{#if this.all.length}}
            <p class="log-archives-lead">
              サーバーに残るのは直近
              {{LOCAL_RETENTION_DAYS}}
              日分です。それより前のログは、圧縮ファイル（gzip）としてダウンロードできます。
            </p>

            <div aria-live="polite">
              <ErrorMessage @error={{this.downloadError}} />
            </div>

            <div class="row-list table-wrap">
              <table class="table log-archives-table">
                <thead>
                  <tr>
                    <th scope="col">時刻</th>
                    <th scope="col">サービス</th>
                    <th scope="col" class="log-archives-size">サイズ</th>
                    <th scope="col"><span
                        class="visually-hidden"
                      >操作</span></th>
                  </tr>
                </thead>
                <tbody>
                  {{#each this.visible key="key" as |archive|}}
                    <tr data-test-archive={{archive.key}}>
                      <td class="log-archives-time">
                        <time datetime={{archive.hour}}>
                          {{formatDateTimeJst archive.hour}}
                        </time>
                      </td>
                      <td class="log-archives-service">{{archive.service}}</td>
                      <td class="log-archives-size">
                        {{formatBytes archive.size}}
                      </td>
                      <td class="table-actions">
                        <Button
                          @size="small"
                          @busy={{isBusy this.downloading archive}}
                          aria-label={{downloadLabel archive}}
                          data-test-archive-download
                          {{on "click" (fn this.download archive)}}
                        >ダウンロード</Button>
                      </td>
                    </tr>
                  {{/each}}
                </tbody>
              </table>
            </div>

            {{#if this.remaining}}
              <p class="log-archives-more">
                <Button data-test-archives-more {{on "click" this.showMore}}>
                  さらに表示（残り
                  {{formatCount this.remaining}}
                  件）
                </Button>
              </p>
            {{/if}}
          {{else}}
            <EmptyState
              @title="アーカイブはまだありません"
              @description="ログは定期的に退避されます。直近 {{LOCAL_RETENTION_DAYS}} 日分は、期間を選ぶと表示できます。"
              data-test-no-archives
            >
              <:action>
                <Button data-test-empty-action {{on "click" @onShowRecent}}>直近
                  24 時間を表示</Button>
              </:action>
            </EmptyState>
          {{/if}}
        {{else}}
          <p class="logs-placeholder" role="status" data-test-loading>
            読み込み中…
          </p>
        {{/if}}
      </div>
    </div>
  </template>
}
