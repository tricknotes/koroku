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
import EmptyState from 'koroku-console/components/empty-state';
import Icon from 'koroku-console/components/icon';
import MemoryChart from 'koroku-console/components/memory-chart';
import type ApiService from 'koroku-console/services/api';
import { formatBytes } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import {
  MEMORY_RANGES,
  READY_HOURS,
  formatHours,
  shareOfLimit,
} from 'koroku-console/utils/memory-history';
import type {
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
    slug: string;
    /** The per-service limits in the form right now, service → setting. */
    limits: Record<string, string>;
    /** Puts a suggested limit into the form (saving is the form's job). */
    onApply: (service: string, limit: string) => void;
  };
}

/**
 * How much memory each service used over time, against its limit, with a
 * suggested limit (ARCHITECTURE 9.5). It sits in the memory section of the
 * settings, whose form takes the suggestion.
 */
export default class MemoryHistory extends Component<Signature> {
  @service declare api: ApiService;

  @tracked range: MemoryRange = '24h';
  name = `memory-range-${guidFor(this)}`;

  history = new Loadable<History>(() =>
    this.api.getMemoryHistory(this.args.slug, this.range),
  );

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.history.load();
  }

  /** Keeps the charts on screen while the new range loads. */
  selectRange = (range: MemoryRange) => {
    if (range === this.range) return;
    this.range = range;
    void this.history.load({ quiet: true });
  };

  isApplied = (service: MemoryServiceHistory): boolean => {
    const { limit } = service.suggestion;
    return limit !== null && this.args.limits[service.name] === limit;
  };

  apply = (service: MemoryServiceHistory) => {
    if (service.suggestion.limit) {
      this.args.onApply(service.name, service.suggestion.limit);
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
        <span class="field-label" id="{{this.name}}-title">使用量の履歴</span>
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
          <ul class="memory-history-legend" aria-label="凡例">
            <li><span class="memory-chart-key is-usage"></span>使用量（5
              分ごとの最大）</li>
            <li><span class="memory-chart-key is-limit"></span>上限</li>
          </ul>
          <ul
            class="memory-services {{if this.history.loading 'is-refreshing'}}"
            aria-busy={{if this.history.loading "true"}}
          >
            {{#each history.services key="name" as |svc|}}
              <li class="memory-service" data-test-memory-service={{svc.name}}>
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

                <MemoryChart @service={{svc}} @history={{history}} />

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
                    {{else if (this.isApplied svc)}}
                      <span
                        class="memory-suggestion-state"
                        data-test-memory-applied
                      >
                        <Icon @name="check" />
                        設定欄に入れました
                      </span>
                    {{else}}
                      <Button
                        @size="small"
                        aria-label="{{svc.name}} の上限に {{svc.suggestion.limit}} を入れる"
                        data-test-memory-apply={{svc.name}}
                        {{on "click" (fn this.apply svc)}}
                      >設定に入れる</Button>
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
            目安は、いまの上限になってからの直近 14 日のピーク × 1.5 を 64m
            単位に切り上げた値です（128m
            以上）。「設定に入れる」で上のサービス別の欄に入り、「保存」で保存されます。上限の変更は次のデプロイから反映されます。
          </p>
        {{else}}
          <EmptyState
            @title="まだ記録がありません"
            @description="稼働中のサービスのメモリ使用量を 1 分ごとに測り、5 分ごとにまとめて 14 日間残します。24 時間分たまると上限の目安を出します。"
            data-test-memory-empty
          >
            <:action>
              <LinkTo
                @route="app.deploy"
                @model={{@slug}}
                class={{buttonClass "default" "small"}}
              >デプロイを開く</LinkTo>
            </:action>
          </EmptyState>
        {{/if}}
      </AsyncView>
    </section>
  </template>
}
