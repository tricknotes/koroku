import Component from '@glimmer/component';
import { service } from '@ember/service';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import AsyncView from 'koroku-console/components/async-view';
import Banner from 'koroku-console/components/banner';
import EmptyState from 'koroku-console/components/empty-state';
import Icon from 'koroku-console/components/icon';
import MemoryHistory from 'koroku-console/components/memory-history';
import Meter from 'koroku-console/components/meter';
import RowList from 'koroku-console/components/row-list';
import TabSection from 'koroku-console/components/tab-section';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type AppContext from 'koroku-console/utils/app-context';
import { formatBytes, formatPercent } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import { memoryUsage, type MemoryUsage } from 'koroku-console/utils/services';
import type { App, ServiceStatus } from 'koroku-console/utils/types';

/** As often as the overview follows its services. */
const POLL_INTERVAL = 5000;

const SKELETON_ROWS = [1, 2];

function hasLimit(memory: MemoryUsage): boolean {
  return memory.percent !== null;
}

function percentOf(memory: MemoryUsage): number {
  return memory.percent ?? 0;
}

function cpuText(service: ServiceStatus): string {
  return formatPercent(service.cpu_percent);
}

/** What the running release reserves (ARCHITECTURE 6.2), or ''. */
function reservationText(service: ServiceStatus): string {
  const bytes = service.memory_reservation_bytes;
  return typeof bytes === 'number' && bytes > 0 ? formatBytes(bytes) : '';
}

interface Signature {
  Args: { model: AppContext };
}

/**
 * What the app's services use: CPU and memory now, and the memory history
 * with a suggested limit (ARCHITECTURE 9.5). Anyone who can see the app can
 * see this; saving a suggested limit needs the manage role.
 */
export default class AppResourcesTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  services = new Loadable<ServiceStatus[]>(() =>
    this.api.listServices(this.app.slug),
  );

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.services.load();
    this.poller.every(this, POLL_INTERVAL, () =>
      this.services.load({ quiet: this.services.loaded }),
    );
  }

  get app(): App {
    return this.args.model.app;
  }

  saved = (app: App) => {
    this.args.model.set(app);
  };

  <template>
    {{pageTitle "リソース"}}
    <div class="resources" data-test-resources>
      <TabSection @title="今の使用量" data-test-resources-now>
        <p class="pane-description">
          稼働中のコンテナを数秒ごとに測った値です。CPU は Docker と同じく 1
          コアを 100%
          とします。本番のアプリでは、稼働中のリリースが予約しているメモリも出します。
        </p>
        {{#if this.app.sleeping_since}}
          <Banner data-test-resources-sleeping>
            アクセスがないため休止中で、サービスは止まっています。休止の間はメモリの履歴も記録しません。
          </Banner>
        {{/if}}
        {{#if this.services.stale}}
          <Banner @tone="warning" data-test-stale>
            最新の値を取得できていません。表示は最後に取得できた値です。
          </Banner>
        {{/if}}
        <AsyncView
          @data={{this.services}}
          @errorTitle="サービスの使用量を取得できませんでした"
        >
          <:loading>
            <div
              class="row-list resource-rows"
              role="status"
              aria-label="読み込み中"
              data-test-loading
            >
              {{#each SKELETON_ROWS as |n|}}
                <div class="row" aria-hidden="true" data-skeleton={{n}}>
                  <span class="skeleton overview-skeleton"></span>
                </div>
              {{/each}}
            </div>
          </:loading>
          <:default as |services|>
            {{#if services.length}}
              <RowList
                class="resource-rows"
                @label="サービスごとの使用量"
                data-test-resource-services
              >
                <:header>
                  <span>サービス</span>
                  <span>CPU</span>
                  <span>メモリ</span>
                </:header>
                <:rows as |Row|>
                  {{#each services key="name" as |svc|}}
                    {{#let (memoryUsage svc) (cpuText svc) as |memory cpu|}}
                      <Row data-test-resource-service={{svc.name}}>
                        <div
                          class="resource-service-name mono"
                        >{{svc.name}}</div>
                        <div class="resource-cpu" data-test-resource-cpu>
                          <span class="cell-label">CPU</span>
                          {{#if cpu}}
                            {{cpu}}
                          {{else}}
                            <span
                              class="overview-secondary"
                              title="止まっているか、取得できませんでした。"
                            >—</span>
                          {{/if}}
                        </div>
                        <div
                          class="resource-memory {{if memory.high 'is-high'}}"
                          data-test-resource-memory
                        >
                          <span class="cell-label">メモリ</span>
                          {{#if memory}}
                            <span class="overview-memory-value">
                              {{#if memory.high}}
                                <Icon @name="warning" />
                              {{/if}}
                              {{memory.text}}
                            </span>
                            {{#if (hasLimit memory)}}
                              <Meter
                                class="overview-memory-meter"
                                @label="{{svc.name}} のメモリ使用率"
                                @percent={{percentOf memory}}
                                @tone={{if memory.high "warn" "ok"}}
                              />
                            {{/if}}
                          {{else}}
                            <span
                              class="overview-secondary"
                              title="止まっているか、取得できませんでした。"
                            >—</span>
                          {{/if}}
                          {{#let (reservationText svc) as |reserved|}}
                            {{#if reserved}}
                              <span
                                class="resource-reservation overview-secondary"
                                title="ホストのメモリが足りなくなっても、この量までは後回しに回収されます。"
                                data-test-resource-reservation
                              >予約 {{reserved}}</span>
                            {{/if}}
                          {{/let}}
                        </div>
                      </Row>
                    {{/let}}
                  {{/each}}
                </:rows>
              </RowList>
            {{else}}
              <EmptyState
                @title="動いているサービスはありません"
                @description="デプロイすると、サービスごとの CPU とメモリをここに出します。"
                data-test-resources-empty
              />
            {{/if}}
          </:default>
        </AsyncView>
      </TabSection>

      <div class="pane">
        <MemoryHistory @app={{this.app}} @onSaved={{this.saved}} />
      </div>
    </div>
  </template>
}
