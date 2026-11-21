import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import type Owner from '@ember/owner';
import Button from 'koroku-console/components/button';
import ErrorMessage from 'koroku-console/components/error-message';
import RelativeTime from 'koroku-console/components/relative-time';
import TabSection from 'koroku-console/components/tab-section';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import { activityActor, describeActivity } from 'koroku-console/utils/activity';
import Loadable from 'koroku-console/utils/loadable';
import type { ActivityEntry, ActivityPage } from 'koroku-console/utils/types';

/** Entries shown at first; each 「さらに表示」 adds a page of MORE. */
const FIRST = 5;
const MORE = 20;
/** New entries appear without a reload, like the rest of the overview. */
const INTERVAL = 30000;

interface Signature {
  Element: HTMLElement;
  Args: { slug: string };
}

/**
 * Who did what to the app (ARCHITECTURE 10.7): settings, variables,
 * domains, start and stop, restores, tasks, copies and roles. The newest
 * few, with a way to read further back. Hidden when the backend has no
 * activity yet.
 */
export default class ActivityLog extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  first = new Loadable<ActivityPage>(() =>
    this.api.listActivity(this.args.slug, { limit: FIRST }),
  );

  /** Pages read with 「さらに表示」, oldest last. */
  @tracked older: ActivityEntry[] = [];
  /** Cursor after the older pages; undefined until one was read. */
  @tracked olderNext: number | null | undefined = undefined;
  @tracked loadingMore = false;
  @tracked moreError: unknown = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.first.load();
    this.poller.every(this, INTERVAL, () => this.first.load({ quiet: true }));
  }

  get hidden(): boolean {
    return this.first.unavailable !== null;
  }

  /** The first page and the older ones, newest first, each entry once. */
  get entries(): ActivityEntry[] {
    const seen = new Set<number>();
    return [...(this.first.value?.entries ?? []), ...this.older]
      .filter((entry) => !seen.has(entry.id) && seen.add(entry.id))
      .sort((a, b) => b.id - a.id);
  }

  get next(): number | null {
    if (this.olderNext !== undefined) return this.olderNext;
    return this.first.value?.next_before ?? null;
  }

  loadMore = async () => {
    const before = this.next;
    if (before === null) return;
    this.loadingMore = true;
    this.moreError = null;
    try {
      const page = await this.api.listActivity(this.args.slug, {
        limit: MORE,
        before,
      });
      this.older = [...this.older, ...page.entries];
      this.olderNext = page.next_before;
    } catch (error) {
      this.moreError = error;
    } finally {
      this.loadingMore = false;
    }
  };

  <template>
    {{#unless this.hidden}}
      <TabSection @title="操作の記録" data-test-activity-log ...attributes>
        {{#if this.first.loaded}}
          {{#if this.entries.length}}
            <ol class="box overview-panel activity-list">
              {{#each this.entries key="id" as |entry|}}
                {{#let (describeActivity entry) as |text|}}
                  <li class="activity-item" data-test-activity={{entry.action}}>
                    <p class="activity-summary" data-test-activity-summary>
                      {{text.summary}}
                    </p>
                    {{#each text.details as |line|}}
                      <p class="activity-detail">{{line}}</p>
                    {{/each}}
                    <p class="activity-meta">
                      <span
                        class="activity-actor
                          {{unless entry.actor 'is-system'}}"
                        data-test-activity-actor
                      >{{activityActor entry}}</span>
                      <RelativeTime @time={{entry.at}} />
                    </p>
                  </li>
                {{/let}}
              {{/each}}
            </ol>
            {{#if this.next}}
              <div class="activity-more">
                <Button
                  @variant="ghost"
                  @size="small"
                  @busy={{this.loadingMore}}
                  @busyLabel="読み込み中…"
                  data-test-activity-more
                  {{on "click" this.loadMore}}
                >さらに表示</Button>
              </div>
            {{/if}}
            <ErrorMessage @error={{this.moreError}} />
          {{else}}
            <p
              class="box overview-panel overview-quiet"
              data-test-activity-empty
            >
              まだ記録はありません。設定や環境変数を変えると、誰がいつ変えたかがここに残ります。
            </p>
          {{/if}}
        {{else if this.first.error}}
          <p class="box overview-panel overview-quiet" data-test-side-error>
            取得できませんでした。
            <Button
              @variant="ghost"
              @size="small"
              {{on "click" this.first.reload}}
            >再読み込み</Button>
          </p>
        {{else}}
          <div class="box overview-panel" role="status" aria-label="読み込み中">
            <span class="skeleton overview-skeleton"></span>
          </div>
        {{/if}}
      </TabSection>
    {{/unless}}
  </template>
}
