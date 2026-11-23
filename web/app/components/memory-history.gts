import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import { guidFor } from '@ember/object/internals';
import type Owner from '@ember/owner';
import AsyncView from 'koroku-console/components/async-view';
import Badge from 'koroku-console/components/badge';
import { LinkTo } from '@ember/routing';
import Button, { buttonClass } from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import EmptyState from 'koroku-console/components/empty-state';
import ErrorMessage from 'koroku-console/components/error-message';
import Icon from 'koroku-console/components/icon';
import DeployMarkers from 'koroku-console/components/deploy-markers';
import MemoryChart from 'koroku-console/components/memory-chart';
import type ApiService from 'koroku-console/services/api';
import { formatBytes } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import { can } from 'koroku-console/utils/roles';
import {
  MEMORY_RANGES,
  READY_HOURS,
  formatHours,
  shareOfLimit,
} from 'koroku-console/utils/memory-history';
import {
  MARKER_LIMIT,
  deployMarkers,
  liveDeploymentId,
  rangeStart,
  type DeployMarker,
} from 'koroku-console/utils/deploy-markers';
import type {
  App,
  Deployment,
  MemoryHistory as History,
  MemoryRange,
  MemoryServiceHistory,
} from 'koroku-console/utils/types';

function same(a: unknown, b: unknown): boolean {
  return a === b;
}

function isNearLimit(service: MemoryServiceHistory): boolean {
  return service.suggestion.flag === 'near_limit';
}

function isGenerous(service: MemoryServiceHistory): boolean {
  return service.suggestion.flag === 'generous';
}

/** The suggestion is the limit the service already runs with. */
function isCurrent(service: MemoryServiceHistory): boolean {
  const { limit_bytes } = service.suggestion;
  return limit_bytes !== null && limit_bytes === service.limit_bytes;
}

/** `直近 14 日のピーク 485.4 MB × 1.5`; the basis is shorter after a limit change. */
function basisText(service: MemoryServiceHistory): string {
  const { basis_hours, peak_bytes } = service.suggestion;
  return `直近 ${formatHours(basis_hours)}のピーク ${formatBytes(peak_bytes)} × 1.5`;
}

function collectingText(service: MemoryServiceHistory): string {
  const { basis_hours } = service.suggestion;
  const left = formatHours(Math.max(0, READY_HOURS - basis_hours));
  return `${formatHours(basis_hours)}分を記録しました。目安はあと ${left}で出ます。`;
}

interface Signature {
  Element: HTMLElement;
  Args: {
    app: App;
    /** The app as the server has it after a limit was saved. */
    onSaved: (app: App) => void;
  };
}

/**
 * How much memory each service used over time, against its limit, with a
 * suggested limit (ARCHITECTURE 9.5). It sits in the resources tab; a
 * suggestion is saved as the service's limit from here, after a
 * confirmation, and applies from the next deployment.
 */
export default class MemoryHistory extends Component<Signature> {
  @service declare api: ApiService;

  @tracked range: MemoryRange = '24h';
  name = `memory-range-${guidFor(this)}`;

  /** The service whose suggestion is about to be saved. */
  @tracked confirming: MemoryServiceHistory | null = null;
  @tracked saving = false;
  @tracked error: unknown = null;

  history = new Loadable<History>(() =>
    this.api.getMemoryHistory(this.args.app.slug, this.range),
  );

  /**
   * The deployments of the range, read alongside the history. Without
   * them the charts show no markers and are otherwise the same.
   */
  deployments = new Loadable<Deployment[]>(() =>
    this.api.listDeploymentsFinishedSince(
      this.args.app.slug,
      rangeStart(this.range, Date.now()),
      MARKER_LIMIT,
    ),
  );

  get canSave(): boolean {
    return can(this.args.app, 'manage');
  }

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.history.load();
    void this.deployments.load();
  }

  /** Keeps the charts on screen while the new range loads. */
  selectRange = (range: MemoryRange) => {
    if (range === this.range) return;
    this.range = range;
    void this.history.load({ quiet: true });
    void this.deployments.load({ quiet: true });
  };

  markersOf = (history: History): DeployMarker[] =>
    deployMarkers(this.deployments.value ?? [], history);

  get liveId(): number | null {
    return liveDeploymentId(
      this.deployments.value ?? [],
      this.args.app.current_deployment,
    );
  }

  /** The API gave as many as it gives at once: older ones may be missing. */
  get truncated(): boolean {
    return (this.deployments.value?.length ?? 0) >= MARKER_LIMIT;
  }

  hasReverted = (markers: DeployMarker[]): boolean =>
    markers.some((m) => m.kind === 'reverted');

  /** Saved as the service's limit, waiting for the next deployment. */
  isSaved = (service: MemoryServiceHistory): boolean => {
    const { limit } = service.suggestion;
    return (
      limit !== null &&
      this.args.app.service_memory_limits?.[service.name] === limit
    );
  };

  confirm = (service: MemoryServiceHistory) => {
    this.error = null;
    this.confirming = service;
  };

  cancel = () => {
    if (!this.saving) this.confirming = null;
  };

  /** Adds or replaces this service's limit; the others stay as they are. */
  save = async () => {
    const service = this.confirming;
    const limit = service?.suggestion.limit;
    if (!service || !limit) return;
    this.saving = true;
    this.error = null;
    try {
      const app = await this.api.updateApp(this.args.app.slug, {
        service_memory_limits: {
          ...(this.args.app.service_memory_limits ?? {}),
          [service.name]: limit,
        },
      });
      this.args.onSaved(app);
      this.confirming = null;
    } catch (error) {
      this.error = error;
    } finally {
      this.saving = false;
    }
  };

  <template>
    <section
      class="memory-history"
      aria-labelledby="{{this.name}}-title"
      data-test-memory-history
      ...attributes
    >
      <div class="memory-history-header">
        <h2 class="pane-title" id="{{this.name}}-title">メモリの履歴</h2>
        <fieldset class="segmented segmented-text" data-test-memory-range>
          <legend class="visually-hidden">期間</legend>
          {{#each MEMORY_RANGES as |option|}}
            <label class="segmented-option">
              <input
                type="radio"
                class="visually-hidden"
                name={{this.name}}
                value={{option.value}}
                checked={{same this.range option.value}}
                data-test-memory-range-option={{option.value}}
                {{on "change" (fn this.selectRange option.value)}}
              />
              {{option.label}}
            </label>
          {{/each}}
        </fieldset>
      </div>

      <AsyncView
        @data={{this.history}}
        @errorTitle="メモリの履歴を読み込めませんでした"
        as |history|
      >
        {{#if history.services.length}}
          {{#let (this.markersOf history) as |markers|}}
            <ul class="memory-history-legend" aria-label="凡例">
              <li><span class="memory-chart-key is-usage"></span>使用量（5
                分ごとの最大）</li>
              <li><span class="memory-chart-key is-limit"></span>上限</li>
              {{#if markers.length}}
                <li data-test-legend-deploy><svg
                    class="deploy-marker-key"
                    viewBox="0 0 10 8"
                    aria-hidden="true"
                  ><path d="M1 1h8L5 7z" /></svg>デプロイ</li>
                {{#if (this.hasReverted markers)}}
                  <li data-test-legend-reverted><svg
                      class="deploy-marker-key is-reverted"
                      viewBox="0 0 10 8"
                      aria-hidden="true"
                    ><path d="M1 1h8L5 7z" /></svg>失敗して戻したデプロイ</li>
                {{/if}}
              {{/if}}
            </ul>
            {{#if markers.length}}
              <DeployMarkers
                @slug={{@app.slug}}
                @markers={{markers}}
                @liveId={{this.liveId}}
              />
              {{#if this.truncated}}
                <p
                  class="field-hint deploy-markers-note"
                  data-test-deploy-markers-truncated
                >
                  新しい 200 件のデプロイだけを表示しています。
                </p>
              {{/if}}
            {{/if}}
            <ul
              class="memory-services
                {{if this.history.loading 'is-refreshing'}}"
              aria-busy={{if this.history.loading "true"}}
            >
              {{#each history.services key="name" as |svc|}}
                <li
                  class="memory-service"
                  data-test-memory-service={{svc.name}}
                >
                  <div class="memory-service-header">
                    <span class="memory-service-name mono">{{svc.name}}</span>
                    {{#if (isNearLimit svc)}}
                      <Badge @tone="warn" data-test-memory-flag="near_limit">
                        <Icon @name="warning" />
                        上限に近い
                      </Badge>
                    {{else if (isGenerous svc)}}
                      <Badge @tone="info" data-test-memory-flag="generous">
                        <Icon @name="info" />
                        余裕が大きい
                      </Badge>
                    {{/if}}
                  </div>
                  <dl class="memory-service-stats">
                    <div>
                      <dt>ピーク</dt>
                      <dd data-test-memory-peak>
                        {{formatBytes svc.peak_bytes}}
                        {{#if svc.limit_bytes}}
                          <span class="muted">（上限の
                            {{shareOfLimit
                              svc.peak_bytes
                              svc.limit_bytes
                            }}）</span>
                        {{/if}}
                      </dd>
                    </div>
                    <div>
                      <dt>上限</dt>
                      <dd data-test-memory-limit>
                        {{#if svc.limit_bytes}}
                          {{formatBytes svc.limit_bytes}}
                        {{else}}
                          なし
                        {{/if}}
                      </dd>
                    </div>
                    {{#if svc.restarts}}
                      <div>
                        <dt>自動の再起動</dt>
                        <dd data-test-memory-restarts>{{svc.restarts}} 回</dd>
                      </div>
                    {{/if}}
                  </dl>

                  <MemoryChart
                    @service={{svc}}
                    @history={{history}}
                    @markers={{markers}}
                  />

                  <div class="memory-suggestion" data-test-memory-suggestion>
                    {{#if svc.suggestion.ready}}
                      <p class="memory-suggestion-text">
                        目安
                        <strong
                          class="mono"
                          data-test-memory-suggested
                        >{{svc.suggestion.limit}}</strong>
                        <span class="muted">（{{basisText svc}}）</span>
                        {{#if (isNearLimit svc)}}
                          <span
                            class="memory-suggestion-why"
                          >上限に当たると、コンテナが止められて再起動します。</span>
                        {{else if (isGenerous svc)}}
                          <span class="memory-suggestion-why">7
                            日以上、ピークが上限の 30%
                            未満です。下げた分をほかのアプリに回せます。</span>
                        {{/if}}
                      </p>
                      {{#if (isCurrent svc)}}
                        <span
                          class="memory-suggestion-state"
                          data-test-memory-current
                        >
                          <Icon @name="check" />
                          いまの上限で足りています
                        </span>
                      {{else if (this.isSaved svc)}}
                        <span
                          class="memory-suggestion-state"
                          data-test-memory-saved
                        >
                          <Icon @name="check" />
                          上限に保存しました。次のデプロイから反映されます
                        </span>
                      {{else if this.canSave}}
                        <Button
                          @size="small"
                          aria-label="{{svc.name}} の上限を {{svc.suggestion.limit}} にする"
                          data-test-memory-apply={{svc.name}}
                          {{on "click" (fn this.confirm svc)}}
                        >上限を {{svc.suggestion.limit}} にする</Button>
                      {{/if}}
                    {{else}}
                      <p
                        class="memory-suggestion-text"
                        data-test-memory-collecting
                      >
                        <Badge>記録中</Badge>
                        {{collectingText svc}}
                        {{#if (isNearLimit svc)}}
                          <span class="memory-suggestion-why">すでに上限の 90%
                            を超えたことがあります。</span>
                        {{/if}}
                      </p>
                    {{/if}}
                  </div>
                </li>
              {{/each}}
            </ul>
            <p class="field-hint">
              目安は直近 14 日のピーク × 1.5 です（64m 単位に切り上げ）。
            </p>
          {{/let}}
        {{else}}
          <EmptyState
            @title="まだ記録がありません"
            @description="メモリ使用量を 1 分ごとに測って 14 日間残します。24 時間分たまると上限の目安を出します。"
            data-test-memory-empty
          >
            <:action>
              <LinkTo
                @route="app.deploy"
                @model={{@app.slug}}
                class={{buttonClass "default" "small"}}
              >デプロイを開く</LinkTo>
            </:action>
          </EmptyState>
        {{/if}}
      </AsyncView>

      {{#if this.confirming}}
        <ConfirmDialog
          @title="{{this.confirming.name}} の上限を {{this.confirming.suggestion.limit}} にしますか？"
          @confirmLabel="上限を変更"
          @busy={{this.saving}}
          @onConfirm={{this.save}}
          @onCancel={{this.cancel}}
        >
          <p>
            設定のサービス別のメモリ上限に保存します。動いているコンテナはそのままで、次のデプロイから反映されます。
          </p>
          <ErrorMessage @error={{this.error}} />
        </ConfirmDialog>
      {{/if}}
    </section>
  </template>
}
