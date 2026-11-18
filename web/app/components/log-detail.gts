import type { TOC } from '@ember/component/template-only';
import { on } from '@ember/modifier';
import type { LogRow } from 'paas-console/utils/log-buffer';
import { formatLogTime, serviceLabel } from 'paas-console/utils/logs';
import CopyButton from './copy-button';

/**
 * The selected log row in full: the whole text (wrapped, however long),
 * which service wrote it and exactly when. Docked under the log list.
 */
const LogDetail: TOC<{
  Element: HTMLElement;
  Args: { row: LogRow; onClose: () => unknown };
}> = <template>
  <aside
    class="log-detail"
    aria-label="選択した行の詳細"
    data-test-log-detail
    ...attributes
  >
    <div class="log-detail-header">
      <dl class="log-detail-meta">
        <div>
          <dt>サービス</dt>
          <dd data-test-log-detail-service>
            {{#if @row.service}}{{serviceLabel @row.service}}{{else}}—{{/if}}
          </dd>
        </div>
        <div>
          <dt>時刻</dt>
          <dd data-test-log-detail-time>
            {{#if @row.time}}
              <time datetime={{@row.time}} title={{@row.time}}>
                {{formatLogTime @row.time}}
              </time>
            {{else}}
              —
            {{/if}}
          </dd>
        </div>
      </dl>
      <div class="log-detail-actions">
        <CopyButton @text={{@row.line}} @label="本文" />
        <button
          type="button"
          class="button button-ghost button-icon"
          aria-label="詳細を閉じる"
          title="詳細を閉じる（Esc）"
          data-test-log-detail-close
          {{on "click" @onClose}}
        >
          <svg class="icon" viewBox="0 0 16 16" aria-hidden="true">
            <path d="M4 4l8 8M12 4l-8 8" />
          </svg>
        </button>
      </div>
    </div>
    {{! Focusable, so a long line can be scrolled with the keyboard. }}
    <pre
      class="log-detail-text"
      tabindex="0"
      role="region"
      aria-label="本文"
      data-test-log-detail-text
    >{{@row.line}}</pre>
  </aside>
</template>;

export default LogDetail;
