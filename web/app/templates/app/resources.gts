import Component from '@glimmer/component';
import { service } from '@ember/service';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import AsyncView from 'koroku-console/components/async-view';
import ComposeSection from 'koroku-console/components/compose-section';
import Banner from 'koroku-console/components/banner';
import EmptyState from 'koroku-console/components/empty-state';
import Icon from 'koroku-console/components/icon';
import IdleSleepSettings from 'koroku-console/components/idle-sleep-settings';
import MemoryHistory from 'koroku-console/components/memory-history';
import MemorySettings from 'koroku-console/components/memory-settings';
import Meter from 'koroku-console/components/meter';
import RowList from 'koroku-console/components/row-list';
import ServiceReservation from 'koroku-console/components/service-reservation';
import TabSection from 'koroku-console/components/tab-section';
import UnsavedSettingsGuard from 'koroku-console/components/unsaved-settings-guard';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type AppResourcesController from 'koroku-console/controllers/app/resources';
import type AppContext from 'koroku-console/utils/app-context';
import { formatPercent } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import SettingsEditor, {
  IDLE_SLEEP_FIELDS,
} from 'koroku-console/utils/settings-editor';
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

/** The running release reserves memory for it (ARCHITECTURE 6.2). */
function reserves(service: ServiceStatus): boolean {
  const bytes = service.memory_reservation_bytes;
  return typeof bytes === 'number' && bytes > 0;
}

interface Signature {
  Args: { model: AppContext; controller: AppResourcesController };
}

/**
 * Memory and containers in one tab (ARCHITECTURE 9.1): what the services
 * use now, their limits and reservations (changed here and nowhere else),
 * idle sleep (which frees memory), the memory history with a suggested limit (ARCHITECTURE 9.5), and the
 * Compose setup behind them (ARCHITECTURE 9.10). Anyone who can see the app
 * can see this; changing a limit or a reservation needs the manage role.
 */
export default class AppResourcesTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  services = new Loadable<ServiceStatus[]>(() =>
    this.api.listServices(this.app.slug),
  );

  /** Idle sleep, saved on its own (ARCHITECTURE 8.4, 9.1). */
  idleEditor: SettingsEditor;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.idleEditor = new SettingsEditor(
      this.api,
      args.model,
      IDLE_SLEEP_FIELDS,
    );
    void this.services.load();
    this.poller.every(this, POLL_INTERVAL, () =>
      this.services.load({ quiet: this.services.loaded }),
    );
  }

  get app(): App {
    return this.args.model.app;
  }

  get section(): string {
    return this.args.controller.section;
  }

  get focusMemory(): boolean {
    return this.section === 'memory';
  }

  get focusIdleSleep(): boolean {
    return this.section === 'idle-sleep';
  }

  get focusCompose(): boolean {
    return this.section === 'compose';
  }

  get production(): boolean {
    return this.app.environment === 'production';
  }

  /**
   * Production apps show the reservation of each service (ARCHITECTURE
   * 9.5); so does a staging app whose running release still reserves.
   */
  showsReservations = (services: ServiceStatus[]): boolean =>
    this.production || services.some(reserves);

  /** The next reservations come from the server: ask again. */
  saved = (app: App) => {
    this.args.model.set(app);
    void this.services.load({ quiet: true });
  };

  <template>
    {{pageTitle "リソース"}}
    <div class="resources" data-test-resources>
      <TabSection @title="今の使用量" data-test-resources-now>
        <p class="pane-description">
          稼働中のコンテナを数秒ごとに測った値です。CPU は Docker と同じく 1
          コアを 100% とします。
          {{#if this.production}}
            予約は、ホストのメモリが足りなくなっても後回しに回収される量です。変更は次のデプロイから反映されます。
          {{else}}
            ステージングのアプリにはメモリの予約は付きません。
          {{/if}}
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
              {{#let (this.showsReservations services) as |withReservations|}}
                <RowList
                  class="resource-rows
                    {{if withReservations 'has-reservations'}}"
                  @label="サービスごとの使用量"
                  data-test-resource-services
                >
                  <:header>
                    <span>サービス</span>
                    <span>CPU</span>
                    <span>メモリ</span>
                    {{#if withReservations}}
                      <span>予約</span>
                    {{/if}}
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
                          </div>
                          {{#if withReservations}}
                            <ServiceReservation
                              @app={{this.app}}
                              @service={{svc}}
                              @onSaved={{this.saved}}
                            />
                          {{/if}}
                        </Row>
                      {{/let}}
                    {{/each}}
                  </:rows>
                </RowList>
              {{/let}}
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

      <MemorySettings
        @app={{this.app}}
        @services={{this.services.value}}
        @focus={{this.focusMemory}}
        @onSaved={{this.saved}}
      />

      <IdleSleepSettings
        @editor={{this.idleEditor}}
        @focus={{this.focusIdleSleep}}
      />

      <div class="pane">
        <MemoryHistory @app={{this.app}} @onSaved={{this.saved}} />
      </div>

      <ComposeSection @app={{this.app}} @focus={{this.focusCompose}} />
    </div>
    <UnsavedSettingsGuard @editor={{this.idleEditor}} @what="休止の設定" />
  </template>
}
