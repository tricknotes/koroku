import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { hash } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import AsyncView from 'koroku-console/components/async-view';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import ErrorMessage from 'koroku-console/components/error-message';
import Meter, { type MeterTone } from 'koroku-console/components/meter';
import RowList from 'koroku-console/components/row-list';
import StatusIndicator from 'koroku-console/components/status-indicator';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import {
  diskLimitPercent,
  diskOverLimit,
} from 'koroku-console/utils/attention';
import {
  containerGroupRows,
  containerTotals,
} from 'koroku-console/utils/container-resources';
import { setChecked } from 'koroku-console/utils/dom';
import { ApiError, unavailability } from 'koroku-console/utils/errors';
import { formatBytes, formatPercent } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import { reservationView } from 'koroku-console/utils/memory-reservation';
import type { StatusKind } from 'koroku-console/utils/status';
import type {
  ContainerResources,
  DockerUsageItem,
  SystemInfo,
} from 'koroku-console/utils/types';

/** The host figures move slowly. */
const HOST_INTERVAL = 30000;

/** Memory in use above which the row is flagged. */
const MEMORY_WARN_PERCENT = 90;

const SKELETON_ROWS = [1, 2, 3];

/** One line of the resources block: a share of something finite. */
interface Resource {
  name: 'disk' | 'memory' | 'swap';
  label: string;
  /** `null` when the host did not report it. */
  percent: number | null;
  detail: string;
  /** Text shown instead of the figures (no swap, not readable). */
  missing?: string;
  tone: MeterTone;
  /** Threshold drawn on the bar. */
  mark?: number;
  /** What is wrong, as a state with a label; absent when all is well. */
  flag?: { kind: StatusKind; label: string };
}

function share(used: number, total: number): number {
  return total > 0 ? (used / total) * 100 : 0;
}

function resources(system: SystemInfo): Resource[] {
  const rows: Resource[] = [];

  if (system.disk) {
    const over = diskOverLimit(system);
    const limit = diskLimitPercent(system);
    rows.push({
      name: 'disk',
      label: 'ディスク',
      percent: system.disk.used_percent,
      detail: `${formatBytes(system.disk.used_bytes)} / ${formatBytes(system.disk.total_bytes)}、デプロイの上限 ${limit}%`,
      tone: over ? 'danger' : 'ok',
      mark: limit,
      ...(over ? { flag: { kind: 'failed', label: '上限超過' } } : {}),
    });
  } else {
    rows.push(missing('disk', 'ディスク'));
  }

  if (system.memory) {
    const { total_bytes: total, available_bytes: available } = system.memory;
    const used = Math.max(0, total - available);
    const percent = share(used, total);
    const tight = percent >= MEMORY_WARN_PERCENT;
    rows.push({
      name: 'memory',
      label: 'メモリ',
      percent,
      detail: `${formatBytes(used)} / ${formatBytes(total)}、空き ${formatBytes(available)}`,
      tone: tight ? 'warn' : 'ok',
      ...(tight ? { flag: { kind: 'pending', label: '残りわずか' } } : {}),
    });
  } else {
    rows.push(missing('memory', 'メモリ'));
  }

  // Absent on a backend that does not report swap: then there is no row.
  if (system.swap) {
    const { total_bytes: total, used_bytes: used } = system.swap;
    rows.push({
      name: 'swap',
      label: 'スワップ',
      percent: share(used, total),
      detail: `${formatBytes(used)} / ${formatBytes(total)}`,
      tone: 'ok',
    });
  } else if (system.swap === null) {
    rows.push({ ...missing('swap', 'スワップ'), missing: 'ありません' });
  }
  return rows;
}

function missing(name: Resource['name'], label: string): Resource {
  return {
    name,
    label,
    percent: null,
    detail: '',
    missing: '取得できません',
    tone: 'ok',
  };
}

function hasPercent(resource: Resource): resource is Resource & {
  percent: number;
} {
  return resource.percent !== null;
}

const DOCKER_LABELS: Record<DockerUsageItem['type'], string> = {
  images: 'イメージ',
  build_cache: 'ビルドキャッシュ',
  volumes: 'volume',
  containers: 'コンテナ',
};

const DOCKER_ORDER: DockerUsageItem['type'][] = [
  'images',
  'build_cache',
  'volumes',
  'containers',
];

function dockerRows(items: DockerUsageItem[]): DockerUsageItem[] {
  return [...items].sort(
    (a, b) => DOCKER_ORDER.indexOf(a.type) - DOCKER_ORDER.indexOf(b.type),
  );
}

function dockerLabel(item: DockerUsageItem): string {
  return DOCKER_LABELS[item.type] ?? item.type;
}

/** Volumes and containers are never pruned: nothing to expect there. */
function reclaimable(item: DockerUsageItem): string {
  if (item.type === 'volumes' || item.type === 'containers') return '—';
  return formatBytes(item.reclaimable_bytes);
}

function dockerTotal(items: DockerUsageItem[]): string {
  return formatBytes(items.reduce((sum, item) => sum + item.total_bytes, 0));
}

/**
 * 状態 of the system screen (ARCHITECTURE 9.1): the host's resources, what
 * each app's containers use (9.3), the production reservations (6.2) and
 * Docker's disk use with the prune. The container figures and Docker's disk
 * use take seconds, so only this tab asks for them.
 */
export default class SystemIndexTemplate extends Component {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  system = new Loadable<SystemInfo>(() => this.api.getSystem());
  docker = new Loadable<DockerUsageItem[]>(() => this.api.getDockerUsage());
  containers = new Loadable<ContainerResources>(() =>
    this.api.getContainerResources(),
  );

  @tracked confirmingPrune = false;
  @tracked pruneImages = true;
  @tracked pruneBuilder = true;
  @tracked pruning = false;
  @tracked pruned = false;
  @tracked pruneOutput: string | null = null;
  @tracked prunedBytes: number | null = null;
  @tracked pruneError: unknown = null;

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.system.load();
    void this.docker.load();
    void this.containers.load();
    this.poller.every(this, HOST_INTERVAL, () => this.refreshHost());
  }

  /** What the last prune freed, as Docker reported it. */
  get prunedSize(): string | null {
    return this.prunedBytes === null ? null : formatBytes(this.prunedBytes);
  }

  get nothingToPrune(): boolean {
    return !this.pruneImages && !this.pruneBuilder;
  }

  /** The backend has no prune endpoint: said in place of the result. */
  get pruneUnavailable(): boolean {
    return unavailability(this.pruneError) !== null;
  }

  get stale(): boolean {
    return this.system.stale || this.containers.stale;
  }

  private async refreshHost(): Promise<void> {
    const reads = [];
    if (this.system.loaded) reads.push(this.system.load({ quiet: true }));
    if (this.containers.loaded) {
      reads.push(this.containers.load({ quiet: true }));
    }
    await Promise.all(reads);
  }

  requestPrune = () => {
    this.pruneImages = true;
    this.pruneBuilder = true;
    this.confirmingPrune = true;
  };

  cancelPrune = () => {
    this.confirmingPrune = false;
  };

  prune = async () => {
    if (this.nothingToPrune) return;
    this.pruning = true;
    this.pruned = false;
    this.pruneOutput = null;
    this.prunedBytes = null;
    this.pruneError = null;
    try {
      const { output, reclaimed_bytes } = await this.api.prune({
        images: this.pruneImages,
        builder: this.pruneBuilder,
      });
      this.pruneOutput = output;
      this.prunedBytes = reclaimed_bytes ?? null;
      this.pruned = true;
    } catch (error) {
      this.pruneError = error;
      // `500 prune_failed` carries what the command printed until it failed.
      if (error instanceof ApiError) {
        const output = error.details['output'];
        if (typeof output === 'string') this.pruneOutput = output;
      }
    } finally {
      this.pruning = false;
      this.confirmingPrune = false;
    }
    // Whatever happened, the numbers on screen are now out of date.
    await Promise.all([
      this.system.load({ quiet: true }),
      this.docker.load({ quiet: true }),
    ]);
  };

  <template>
    {{pageTitle "状態"}}

    {{#if this.stale}}
      <div class="pane-notices">
        <Banner @tone="warning" data-test-stale>
          最新の状態を取得できていません。表示は最後に取得できた内容です。
        </Banner>
      </div>
    {{/if}}

    <section class="pane" aria-labelledby="resources-title">
      <div class="pane-header">
        <h2 class="pane-title" id="resources-title">ホストのリソース</h2>
      </div>

      <AsyncView
        @data={{this.system}}
        @errorTitle="ホストの状態を取得できませんでした"
      >
        <:loading>
          <div
            class="box resource-rows"
            role="status"
            aria-label="読み込み中"
            data-test-loading
          >
            {{#each SKELETON_ROWS as |n|}}
              <div class="resource" aria-hidden="true" data-skeleton={{n}}>
                <span class="skeleton system-skeleton"></span>
              </div>
            {{/each}}
          </div>
        </:loading>

        <:default as |system|>
          {{#if (diskOverLimit system)}}
            <div class="pane-notices">
              <Banner
                @tone="error"
                @title="ディスク使用率が上限を超えています"
                data-test-disk-warning
              >
                使用率が
                {{diskLimitPercent system}}%
                を下回るまで、デプロイを受け付けません。下の「不要なデータを削除」で空きを作ってください。
              </Banner>
            </div>
          {{/if}}

          <ul class="box resource-rows" data-test-system>
            {{#each (resources system) key="name" as |resource|}}
              <li
                class="resource"
                data-test-resource={{resource.name}}
                data-tone={{resource.tone}}
              >
                <span class="resource-name">{{resource.label}}</span>
                {{#if (hasPercent resource)}}
                  <span class="resource-value">
                    {{formatPercent resource.percent}}
                  </span>
                  <Meter
                    class="resource-meter"
                    @label="{{resource.label}}の使用率"
                    @percent={{resource.percent}}
                    @mark={{resource.mark}}
                    @tone={{resource.tone}}
                  />
                  <span class="resource-detail">{{resource.detail}}</span>
                  <span class="resource-flag">
                    {{#if resource.flag}}
                      <StatusIndicator
                        @kind={{resource.flag.kind}}
                        @label={{resource.flag.label}}
                        data-test-resource-flag
                      />
                    {{/if}}
                  </span>
                {{else}}
                  <span class="resource-missing">{{resource.missing}}</span>
                {{/if}}
              </li>
            {{/each}}
          </ul>
        </:default>
      </AsyncView>
    </section>

    <section class="pane" aria-labelledby="containers-title">
      <div class="pane-header">
        <h2 class="pane-title" id="containers-title">アプリごとのメモリと CPU</h2>
      </div>

      <AsyncView
        @data={{this.containers}}
        @errorTitle="コンテナのメモリと CPU を取得できませんでした"
      >
        <:loading>
          <div
            class="row-list container-rows"
            role="status"
            aria-label="読み込み中"
            data-test-containers-loading
          >
            {{#each SKELETON_ROWS as |n|}}
              <div class="row" aria-hidden="true" data-skeleton={{n}}>
                <span class="skeleton system-skeleton"></span>
              </div>
            {{/each}}
          </div>
        </:loading>

        <:default as |resources|>
          {{#if resources.groups.length}}
            <RowList
              class="container-rows"
              @label="アプリごとのメモリと CPU"
              data-test-containers
            >
              <:header>
                <span>アプリ</span>
                <span>メモリ</span>
                <span>CPU（{{resources.cpu_count}} コア）</span>
              </:header>
              <:rows as |Row|>
                {{#each (containerGroupRows resources) key="key" as |row|}}
                  <Row data-test-container-group={{row.label}}>
                    <span class="usage-app">
                      {{#if row.app}}
                        <LinkTo
                          @route="app.resources"
                          @model={{row.app}}
                          class="row-link usage-name"
                          data-test-container-app
                        >{{row.app}}</LinkTo>
                      {{else}}
                        <span class="usage-name">{{row.label}}</span>
                      {{/if}}
                      <span
                        class="usage-breakdown"
                        data-test-container-breakdown
                      >
                        {{row.breakdown}}
                      </span>
                    </span>
                    <span class="usage-cell usage-memory">
                      <span class="cell-label">メモリ</span>
                      <span class="usage-value" data-test-container-memory>
                        {{row.memory}}
                      </span>
                      <Meter
                        class="usage-meter"
                        @label="{{row.label}}のメモリ（ホスト全体に対する割合）"
                        @percent={{row.memoryPercent}}
                      />
                      <span class="usage-detail" data-test-container-limit>
                        {{row.limit}}
                        {{#if row.nearLimit}}
                          <StatusIndicator
                            @kind="pending"
                            @label="上限間近: {{row.nearLimit}}"
                            data-test-container-near-limit
                          />
                        {{/if}}
                      </span>
                    </span>
                    <span class="usage-cell usage-cpu">
                      <span class="cell-label">CPU</span>
                      <span class="usage-value" data-test-container-cpu>
                        {{row.cpu}}
                      </span>
                      <Meter
                        class="usage-meter"
                        @label="{{row.label}}の CPU（全コアに対する割合）"
                        @percent={{row.cpuShare}}
                      />
                    </span>
                  </Row>
                {{/each}}
              </:rows>
            </RowList>
            {{#let (containerTotals resources) as |totals|}}
              <p class="system-note" data-test-containers-total>
                コンテナ全体で、メモリ
                {{totals.memory}}（ホストの
                {{totals.memoryPercent}}）、CPU
                {{totals.cpu}}。CPU は Docker と同じく 1 コアを 100%
                とした値で、このホストは
                {{totals.cpuCount}}
                コアなので最大
                {{totals.cpuMax}}
                です。計測した瞬間の値です。メーターはホスト全体（メモリ
                {{totals.hostMemory}}、CPU
                {{totals.cpuMax}}）に対する割合です。「その他」は koroku
                のアプリではないコンテナです。
              </p>
            {{/let}}
          {{else}}
            <p class="system-note" data-test-containers-empty>
              稼働中のコンテナはありません。
            </p>
          {{/if}}
        </:default>
      </AsyncView>
    </section>

    {{#if this.system.value}}
      {{#let (reservationView this.system.value) as |reservation|}}
        {{#if reservation}}
          <section
            class="pane"
            aria-labelledby="reservation-title"
            data-test-reservation
          >
            <div class="pane-header">
              <h2
                class="pane-title"
                id="reservation-title"
              >本番アプリのメモリの予約</h2>
            </div>
            {{#if reservation.warning}}
              <div class="pane-notices">
                <Banner
                  @tone="warning"
                  @title="予約がホストのメモリに対して多すぎます"
                  data-test-reservation-warning
                >
                  本番アプリの予約の合計
                  {{reservation.total}}
                  に余白
                  {{reservation.margin}}
                  を足すと、ホストのメモリ
                  {{reservation.host}}
                  を超えます。予約どうしで回収を押し付け合い、本番を守る効き目が薄れます。アプリのリソースタブで予約を減らしてください。
                </Banner>
              </div>
            {{/if}}
            <ul class="box resource-rows" data-test-reservation-total>
              <li
                class="resource"
                data-tone={{if reservation.warning "warn" "ok"}}
              >
                <span class="resource-name">予約の合計</span>
                {{#if reservation.host}}
                  <span class="resource-value">
                    {{formatPercent reservation.percent}}
                  </span>
                  <Meter
                    class="resource-meter"
                    @label="本番アプリのメモリの予約（ホストのメモリに対する割合）"
                    @percent={{reservation.percent}}
                    @mark={{reservation.mark}}
                    @tone={{if reservation.warning "warn" "ok"}}
                  />
                  <span class="resource-detail">
                    {{reservation.total}}
                    /
                    {{reservation.host}}{{#if reservation.room}}、あと
                      {{reservation.room}}
                      まで{{/if}}
                    {{#if reservation.next}}
                      <span
                        class="reservation-next"
                        data-test-reservation-next-total
                      >すべてデプロイすると {{reservation.next}}</span>
                    {{/if}}
                  </span>
                  <span class="resource-flag">
                    {{#if reservation.warning}}
                      <StatusIndicator
                        @kind="pending"
                        @label="余白不足"
                        data-test-reservation-flag
                      />
                    {{/if}}
                  </span>
                {{else}}
                  <span class="resource-missing">
                    {{reservation.total}}（ホストのメモリを取得できません）
                  </span>
                {{/if}}
              </li>
            </ul>
            {{#if reservation.apps.length}}
              <ul class="box reservation-apps" data-test-reservation-apps>
                {{#each reservation.apps key="slug" as |app|}}
                  <li
                    class="reservation-app"
                    data-test-reservation-app={{app.slug}}
                  >
                    <span class="usage-app">
                      <LinkTo
                        @route="app.resources"
                        @model={{app.slug}}
                        @query={{hash section="memory"}}
                        class="row-link usage-name"
                      >{{app.slug}}</LinkTo>
                      <span class="usage-breakdown">{{app.breakdown}}</span>
                      {{#if app.switched}}
                        <span
                          class="usage-breakdown"
                          data-test-reservation-switched
                        >ステージングに切り替え済み。次のデプロイで外れます。</span>
                      {{/if}}
                      {{#if app.next}}
                        <span
                          class="usage-breakdown reservation-next"
                          data-test-reservation-next
                        >設定を変更済み。次のデプロイから {{app.next}}</span>
                      {{/if}}
                    </span>
                    <span class="usage-value">{{app.total}}</span>
                  </li>
                {{/each}}
              </ul>
            {{/if}}
            <p class="system-note" data-test-reservation-note>
              {{#if reservation.apps.length}}
                停止していない本番アプリが、稼働中のリリースで予約している量です。保存した予約の設定は次のデプロイから反映され、それまでは「次のデプロイから」として並べます。ホストのメモリが足りなくなったとき、カーネルは予約した量までを後回しにして、ステージングのアプリから先に回収します。合計にホストやビルド、ステージングのための余白
                {{reservation.margin}}
                を足して、ホストのメモリに収まるようにします。
              {{else}}
                予約している本番アプリはありません。本番のアプリをデプロイすると、各サービスのメモリ上限の半分（リソースタブで変えられます）を予約します。
              {{/if}}
            </p>
          </section>
        {{/if}}
      {{/let}}
    {{/if}}

    <section class="pane" aria-labelledby="docker-title">
      <div class="pane-header">
        <h2 class="pane-title" id="docker-title">Docker のディスク使用量</h2>
        <Button
          @busy={{this.pruning}}
          @busyLabel="削除中…"
          data-test-prune-open
          {{on "click" this.requestPrune}}
        >不要なデータを削除…</Button>
      </div>

      <div class="pane-notices" aria-live="polite">
        {{#if this.pruneUnavailable}}
          <Banner @tone="warning" data-test-prune-unavailable>
            このサーバーは、不要なデータの削除にまだ対応していません。
          </Banner>
        {{else if this.pruneError}}
          <ErrorMessage @error={{this.pruneError}} />
        {{else if this.pruned}}
          <Banner data-test-pruned>
            不要なデータを削除しました。{{#if this.prunedSize}}Docker の報告では
              {{this.prunedSize}}
              空きました。{{/if}}
          </Banner>
        {{/if}}
        {{#if this.pruneOutput}}
          <pre
            class="code-block"
            aria-label="コマンドの出力"
            data-test-prune-output
          >{{this.pruneOutput}}</pre>
        {{/if}}
      </div>

      <AsyncView
        @data={{this.docker}}
        @errorTitle="Docker のディスク使用量を取得できませんでした"
      >
        <:loading>
          <div
            class="row-list docker-rows"
            role="status"
            aria-label="読み込み中"
            data-test-docker-loading
          >
            {{#each SKELETON_ROWS as |n|}}
              <div class="row" aria-hidden="true" data-skeleton={{n}}>
                <span class="skeleton system-skeleton"></span>
              </div>
            {{/each}}
          </div>
        </:loading>

        <:default as |items|>
          <RowList
            class="docker-rows"
            @label="Docker のディスク使用量"
            data-test-docker
          >
            <:header>
              <span>種類</span>
              <span class="docker-number">件数</span>
              <span class="docker-number">使用量</span>
              <span class="docker-number">空く見込み</span>
            </:header>
            <:rows as |Row|>
              {{#each (dockerRows items) key="type" as |item|}}
                <Row data-test-docker-item={{item.type}}>
                  <span class="docker-type">{{dockerLabel item}}</span>
                  <span class="docker-number">
                    <span class="cell-label">件数</span>
                    {{item.count}}
                  </span>
                  <span class="docker-number">
                    <span class="cell-label">使用量</span>
                    {{formatBytes item.total_bytes}}
                  </span>
                  <span class="docker-number docker-reclaimable">
                    <span class="cell-label">空く見込み</span>
                    {{reclaimable item}}
                  </span>
                </Row>
              {{/each}}
            </:rows>
          </RowList>
          <p class="system-note" data-test-docker-total>
            合計
            {{dockerTotal
              items
            }}。「空く見込み」は「不要なデータを削除」で空く量の目安で、少なめに出ます。消えるのは、どのコンテナにも使われていないタグのないイメージと、1
            週間使われていないビルドキャッシュです。ロールバック用に残している直近のリリースや停止中のアプリのイメージ、volume
            は消しません。
          </p>
        </:default>
      </AsyncView>
    </section>

    {{#if this.confirmingPrune}}
      <ConfirmDialog
        @title="不要なデータを削除しますか？"
        @confirmLabel="削除する"
        @danger={{true}}
        @busy={{this.pruning}}
        @onConfirm={{this.prune}}
        @onCancel={{this.cancelPrune}}
      >
        <p>
          使われていないデータを Docker
          から消して、ディスクを空けます。稼働中のアプリには影響しません。消したものは戻せませんが、必要になれば次のビルドで作り直されます。
        </p>
        <fieldset class="option-group dialog-options">
          <legend class="visually-hidden">削除するもの</legend>
          <label class="option">
            <input
              type="checkbox"
              checked={{this.pruneImages}}
              disabled={{this.pruning}}
              data-test-prune-images
              {{on "change" (setChecked this "pruneImages")}}
            />
            <span class="option-body">
              <span class="option-label">タグのないイメージ</span>
              <span class="option-hint">
                ビルドで置き換えられ、どこからも参照されなくなったイメージです。
              </span>
            </span>
          </label>
          <label class="option">
            <input
              type="checkbox"
              checked={{this.pruneBuilder}}
              disabled={{this.pruning}}
              data-test-prune-builder
              {{on "change" (setChecked this "pruneBuilder")}}
            />
            <span class="option-body">
              <span class="option-label">ビルドキャッシュ</span>
              <span class="option-hint">
                1 週間使われていないものだけを消します。
              </span>
            </span>
          </label>
        </fieldset>
        {{#if this.nothingToPrune}}
          <p role="alert" data-test-prune-nothing>
            削除するものを 1 つ以上選んでください。
          </p>
        {{/if}}
      </ConfirmDialog>
    {{/if}}
  </template>
}
