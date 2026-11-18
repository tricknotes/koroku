import Component from '@glimmer/component';
import { on } from '@ember/modifier';
import { modifier } from 'ember-modifier';
import followTail from 'koroku-console/modifiers/follow-tail';
import type { LogRow } from 'koroku-console/utils/log-buffer';
import { formatCount, splitMatches } from 'koroku-console/utils/logs';

interface LogStreamSignature {
  Element: HTMLDivElement;
  Args: {
    rows: LogRow[];
    /** Names the list for assistive technology. */
    label: string;
    /** Occurrences of this text are marked in the rows. */
    keyword?: string;
    /** Wrap long lines instead of scrolling sideways. */
    wrap: boolean;
    /** Show the service column (pointless when one service is selected). */
    showService: boolean;
    /** Width of the service column, in characters. */
    serviceWidth: number;
    /** The reader is at the end of a tail: keep the newest row in view. */
    following: boolean;
    /** Offer the way back to the newest row (following was interrupted). */
    paused: boolean;
    /** Lines that arrived while paused. */
    waiting: number;
    selected: LogRow | null;
    onSelect: (row: LogRow | null) => unknown;
    /** The reader left the end (false) or came back to it (true). */
    onFollowChange: (atEnd: boolean) => unknown;
    onResume: () => unknown;
  };
}

function isSelected(row: LogRow, selected: LogRow | null): 'true' | 'false' {
  return selected !== null && row.id === selected.id ? 'true' : 'false';
}

function rowDomId(row: LogRow | null): string | undefined {
  return row ? `log-row-${row.id}` : undefined;
}

/**
 * The log list: monospace rows in a scrolling viewport. It is one tab stop;
 * the arrow keys move the selection and Escape clears it. Clicks are
 * handled by the list as a whole, so thousands of rows cost no listeners,
 * and rows are keyed, so new lines only add their own elements.
 */
export default class LogStream extends Component<LogStreamSignature> {
  private get selectedIndex(): number {
    const id = this.args.selected?.id;
    return id === undefined
      ? -1
      : this.args.rows.findIndex((row) => row.id === id);
  }

  /**
   * Changes whenever what is rendered changes, for the follow modifier: the
   * number of rows alone stays the same once the list is capped.
   */
  get revision(): string {
    const { rows, wrap } = this.args;
    return `${rows.length}:${rows[rows.length - 1]?.id ?? 0}:${wrap}`;
  }

  /**
   * Index of the first (or last) row that is in view, where a keyboard
   * selection starts. The rows are in order, so a binary search over their
   * positions finds it without measuring thousands of them.
   */
  private edgeInView(viewport: HTMLElement, edge: 'first' | 'last'): number {
    const elements = viewport.querySelectorAll('[data-row]');
    const frame = viewport.getBoundingClientRect();
    let low = 0;
    let high = elements.length - 1;
    while (low < high) {
      const middle =
        edge === 'first'
          ? Math.floor((low + high) / 2)
          : Math.ceil((low + high) / 2);
      const rect = (elements[middle] as Element).getBoundingClientRect();
      if (edge === 'first') {
        if (rect.bottom > frame.top) high = middle;
        else low = middle + 1;
      } else if (rect.top < frame.bottom) {
        low = middle;
      } else {
        high = middle - 1;
      }
    }
    return low;
  }

  pick = (event: MouseEvent) => {
    // Dragging over rows to copy text is not a click on a row.
    if (window.getSelection()?.toString()) return;
    const element =
      event.target instanceof Element
        ? event.target.closest('[data-row]')
        : null;
    if (!element) return;
    const id = Number(element.getAttribute('data-row'));
    const row = this.args.rows.find((candidate) => candidate.id === id);
    if (!row) return;
    // A second click on the selected row closes its detail.
    void this.args.onSelect(row.id === this.args.selected?.id ? null : row);
  };

  navigate = (event: KeyboardEvent) => {
    const { rows } = this.args;
    if (event.key === 'Escape') {
      if (this.args.selected) void this.args.onSelect(null);
      return;
    }
    if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
    if (rows.length === 0) return;
    event.preventDefault();

    const last = rows.length - 1;
    const index = this.selectedIndex;
    let next: number;
    if (index < 0) {
      // Start where the reader is looking: the edge of what is in view.
      next = this.edgeInView(
        event.currentTarget as HTMLElement,
        event.key === 'ArrowUp' ? 'last' : 'first',
      );
    } else {
      next = Math.min(
        last,
        Math.max(0, index + (event.key === 'ArrowDown' ? 1 : -1)),
      );
    }
    const row = rows[next];
    if (row && row.id !== this.args.selected?.id) void this.args.onSelect(row);
  };

  /** Keeps the selected row in view (the detail opening can cover it). */
  reveal = modifier<{
    Element: HTMLElement;
    Args: { Positional: [selected: LogRow | null] };
  }>((element, [selected]) => {
    if (!selected) return;
    element
      .querySelector(`[data-row="${selected.id}"]`)
      ?.scrollIntoView({ block: 'nearest' });
  });

  /** Sizes the service column through a custom property. */
  columns = modifier<{
    Element: HTMLElement;
    Args: { Positional: [serviceWidth: number] };
  }>((element, [serviceWidth]) => {
    element.style.setProperty('--log-service-width', `${serviceWidth}ch`);
  });

  <template>
    <div
      class="log-stream
        {{if @wrap 'is-wrapped'}}
        {{if @showService 'has-service'}}"
      data-test-log-stream
      ...attributes
    >
      {{! One focusable listbox rather than a tab stop per row. Not
          role="log": a busy stream would be read out line by line. }}
      <div
        class="log-viewport"
        role="listbox"
        tabindex="0"
        aria-label={{@label}}
        aria-activedescendant={{rowDomId @selected}}
        data-test-log-viewport
        {{on "click" this.pick}}
        {{on "keydown" this.navigate}}
        {{followTail
          this.revision
          following=@following
          onChange=@onFollowChange
        }}
        {{this.reveal @selected}}
        {{this.columns @serviceWidth}}
      >
        <div class="log-rows" role="presentation">
          {{#each @rows key="id" as |row|}}
            <div
              class="log-row"
              role="option"
              id={{rowDomId row}}
              aria-selected={{isSelected row @selected}}
              data-row={{row.id}}
              data-test-log-row
            >
              <span class="log-time">{{row.stamp}}</span>
              {{#if @showService}}
                <span class="log-service">{{row.service}}</span>
              {{/if}}
              <span class="log-text">
                {{~#if @keyword~}}
                  {{~#each (splitMatches row.line @keyword) as |part|~}}
                    {{~#if part.match~}}
                      <span class="log-match">{{part.text}}</span>
                    {{~else~}}
                      {{part.text}}
                    {{~/if~}}
                  {{~/each~}}
                {{~else~}}
                  {{row.line}}
                {{~/if~}}
              </span>
            </div>
          {{/each}}
        </div>
      </div>

      {{#if @paused}}
        <button
          type="button"
          class="button log-resume"
          data-test-log-resume
          {{on "click" @onResume}}
        >
          <svg class="icon" viewBox="0 0 16 16" aria-hidden="true">
            <path d="M8 3v10M3.75 8.75L8 13l4.25-4.25" />
          </svg>
          最新の行へ
          {{#if @waiting}}
            <span class="log-resume-count" data-test-log-waiting>
              新着
              {{formatCount @waiting}}
              行
            </span>
          {{/if}}
        </button>
      {{/if}}
    </div>
  </template>
}
