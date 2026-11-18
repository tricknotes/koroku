import type { TOC } from '@ember/component/template-only';
import { htmlSafe } from '@ember/template';
import type { SafeString } from '@ember/template';
import { formatPercent } from 'paas-console/utils/format';

export type MeterTone = 'ok' | 'warn' | 'danger';

function clamp(percent: number): number {
  if (!Number.isFinite(percent)) return 0;
  return Math.min(100, Math.max(0, percent));
}

function rounded(percent: number): number {
  return Math.round(clamp(percent) * 10) / 10;
}

// Built from a clamped number only, so marking it safe is sound.
function width(percent: number): SafeString {
  return htmlSafe(`width: ${rounded(percent)}%`);
}

function position(percent: number): SafeString {
  return htmlSafe(`left: ${rounded(percent)}%`);
}

function toneClass(tone: MeterTone | undefined): string {
  return tone && tone !== 'ok' ? `is-${tone}` : '';
}

function valueText(percent: number): string {
  return formatPercent(rounded(percent));
}

/**
 * A thin bar for "how full": a plain number drawn as a length, not a chart.
 * `@mark` draws a tick at a threshold (the disk usage above which
 * deployments are refused). The tone only supports the text next to the
 * bar, which has to say what is wrong.
 */
const Meter: TOC<{
  Element: HTMLDivElement;
  Args: {
    /** Names the quantity for assistive technology, e.g. "ディスク使用率". */
    label: string;
    /** 0 to 100. */
    percent: number;
    mark?: number;
    tone?: MeterTone;
  };
}> = <template>
  <div
    class="meter-bar {{toneClass @tone}}"
    role="meter"
    aria-label={{@label}}
    aria-valuemin="0"
    aria-valuemax="100"
    aria-valuenow={{rounded @percent}}
    aria-valuetext={{valueText @percent}}
    ...attributes
  >
    <span class="meter-bar-fill" style={{width @percent}}></span>
    {{#if @mark}}
      <span class="meter-bar-mark" style={{position @mark}}></span>
    {{/if}}
  </div>
</template>;

export default Meter;
