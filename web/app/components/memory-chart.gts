import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { on } from '@ember/modifier';
import { htmlSafe } from '@ember/template';
import type { SafeString } from '@ember/template';
import { formatBytes, formatDateTimeJst } from 'koroku-console/utils/format';
import {
  PLOT_HEIGHT,
  PLOT_WIDTH,
  axisTime,
  chartGeometry,
  nearestPoint,
  shareOfLimit,
  type ChartGeometry,
  type ChartPoint,
} from 'koroku-console/utils/memory-history';
import type { DeployMarker } from 'koroku-console/utils/deploy-markers';
import type {
  MemoryHistory,
  MemoryServiceHistory,
} from 'koroku-console/utils/types';

// Built from numbers only, so marking them safe is sound.
function at(left: number, top?: number): SafeString {
  const parts = [`left: ${left}%`];
  if (top !== undefined) parts.push(`top: ${top}%`);
  return htmlSafe(parts.join('; '));
}

function topAt(top: number): SafeString {
  return htmlSafe(`top: ${top}%`);
}

/** The readout sits right of the point, or left of it near the right edge. */
function readoutClass(point: ChartPoint): string {
  return point.left > 60
    ? 'memory-chart-readout is-left'
    : 'memory-chart-readout';
}

function isReverted(marker: DeployMarker): boolean {
  return marker.kind === 'reverted';
}

function timeOf(iso: string): string {
  return formatDateTimeJst(iso);
}

const VIEW_BOX = `0 0 ${PLOT_WIDTH} ${PLOT_HEIGHT}`;

interface Signature {
  Element: HTMLElement;
  Args: {
    service: MemoryServiceHistory;
    history: Pick<MemoryHistory, 'from' | 'to' | 'step_seconds'>;
    /**
     * Deployments within the range, drawn as vertical lines; their buttons
     * are in the row above the charts (ARCHITECTURE 9.5).
     */
    markers?: DeployMarker[];
  };
}

/**
 * The memory use of one service over time: the 5-minute maximum as a line
 * over a light wash, the limit as a grey step line, and deployments as
 * thin vertical lines. Pointer and arrow
 * keys show the values at a time; the same figures are in the summary
 * next to the chart.
 */
export default class MemoryChart extends Component<Signature> {
  @tracked active: number | null = null;

  get geometry(): ChartGeometry {
    return chartGeometry(this.args.service, this.args.history);
  }

  get activePoint(): ChartPoint | null {
    if (this.active === null) return null;
    return this.geometry.points[this.active] ?? null;
  }

  get label(): string {
    const { name, peak_bytes, limit_bytes } = this.args.service;
    const limit = limit_bytes ? `、上限 ${formatBytes(limit_bytes)}` : '';
    return `${name} のメモリ使用量。期間内のピーク ${formatBytes(peak_bytes)}${limit}。左右の矢印キーで時刻ごとの値を読めます。`;
  }

  get firstTime(): string {
    return axisTime(this.args.history.from);
  }

  get lastTime(): string {
    return axisTime(this.args.history.to);
  }

  move = (event: PointerEvent) => {
    const element = event.currentTarget as HTMLElement;
    const rect = element.getBoundingClientRect();
    if (rect.width === 0) return;
    const index = nearestPoint(
      this.geometry.points,
      (event.clientX - rect.left) / rect.width,
    );
    this.active = index < 0 ? null : index;
  };

  leave = () => {
    this.active = null;
  };

  focus = () => {
    if (this.active === null && this.geometry.points.length > 0) {
      this.active = this.geometry.points.length - 1;
    }
  };

  key = (event: KeyboardEvent) => {
    const last = this.geometry.points.length - 1;
    if (last < 0) return;
    const current = this.active ?? last;
    let next: number | null = current;
    switch (event.key) {
      case 'ArrowLeft':
        next = Math.max(0, current - 1);
        break;
      case 'ArrowRight':
        next = Math.min(last, current + 1);
        break;
      case 'Home':
        next = 0;
        break;
      case 'End':
        next = last;
        break;
      case 'Escape':
        next = null;
        break;
      default:
        return;
    }
    event.preventDefault();
    this.active = next;
  };

  <template>
    <figure class="memory-chart" ...attributes>
      <div class="memory-chart-frame">
        <div class="memory-chart-ticks" aria-hidden="true">
          {{#each this.geometry.ticks as |tick|}}
            <span class="memory-chart-tick" style={{topAt tick.top}}>
              {{tick.label}}
            </span>
          {{/each}}
        </div>
        <div
          class="memory-chart-plot"
          tabindex="0"
          role="group"
          aria-label={{this.label}}
          data-test-memory-chart-plot
          {{on "pointermove" this.move}}
          {{on "pointerleave" this.leave}}
          {{on "focus" this.focus}}
          {{on "blur" this.leave}}
          {{on "keydown" this.key}}
        >
          {{#each this.geometry.ticks as |tick|}}
            <span
              class="memory-chart-grid"
              style={{topAt tick.top}}
              aria-hidden="true"
            ></span>
          {{/each}}
          {{#each @markers as |marker|}}
            <span
              class="memory-chart-marker
                {{if (isReverted marker) 'is-reverted'}}"
              style={{at marker.left}}
              aria-hidden="true"
              data-test-memory-chart-marker={{marker.deployment.id}}
            ></span>
          {{/each}}
          <svg
            class="memory-chart-svg"
            viewBox={{VIEW_BOX}}
            preserveAspectRatio="none"
            aria-hidden="true"
          >
            <path class="memory-chart-area" d={{this.geometry.areaPath}} />
            <path class="memory-chart-limit" d={{this.geometry.limitPath}} />
            <path class="memory-chart-usage" d={{this.geometry.usagePath}} />
          </svg>
          {{#if this.activePoint}}
            <span
              class="memory-chart-crosshair"
              style={{at this.activePoint.left}}
              aria-hidden="true"
            ></span>
            <span
              class="memory-chart-dot"
              style={{at this.activePoint.left this.activePoint.top}}
              aria-hidden="true"
            ></span>
          {{/if}}
          <div
            class={{if
              this.activePoint
              (readoutClass this.activePoint)
              "visually-hidden"
            }}
            style={{if this.activePoint (at this.activePoint.left)}}
            aria-live="polite"
            data-test-memory-readout
          >
            {{#if this.activePoint}}
              <p class="memory-chart-readout-time">
                {{timeOf this.activePoint.point.at}}
              </p>
              <dl>
                <div>
                  <dt><span class="memory-chart-key is-usage"></span>最大</dt>
                  <dd>{{formatBytes this.activePoint.point.max_bytes}}
                    <span class="memory-chart-share">{{shareOfLimit
                        this.activePoint.point.max_bytes
                        this.activePoint.point.limit_bytes
                      }}</span></dd>
                </div>
                <div>
                  <dt><span class="memory-chart-key is-average"></span>平均</dt>
                  <dd>{{formatBytes this.activePoint.point.avg_bytes}}</dd>
                </div>
                <div>
                  <dt><span class="memory-chart-key is-limit"></span>上限</dt>
                  <dd>
                    {{#if this.activePoint.point.limit_bytes}}
                      {{formatBytes this.activePoint.point.limit_bytes}}
                    {{else}}
                      なし
                    {{/if}}
                  </dd>
                </div>
              </dl>
            {{/if}}
          </div>
        </div>
      </div>
      <div class="memory-chart-times" aria-hidden="true">
        <span>{{this.firstTime}}</span>
        <span>{{this.lastTime}}</span>
      </div>
    </figure>
  </template>
}
