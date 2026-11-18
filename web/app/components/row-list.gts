import type { TOC } from '@ember/component/template-only';

/**
 * One record. Put a link with class `row-link` inside to make the whole row
 * navigate there (the link is stretched over the row, so it stays one tab
 * stop and a real link); give any other control the class `row-action` so
 * it stays clickable above it.
 */
const Row: TOC<{
  Element: HTMLLIElement;
  Blocks: { default: [] };
}> = <template>
  <li class="row" ...attributes>{{yield}}</li>
</template>;

/**
 * A bordered list with one row per record. Header and rows share one grid:
 * set `--row-columns` (a `grid-template-columns` value) on the list.
 *
 *   <RowList class="app-rows" @label="アプリ">
 *     <:header><span>状態</span>...</:header>
 *     <:rows as |Row|>
 *       <Row>...</Row>
 *     </:rows>
 *   </RowList>
 *
 * The header is purely visual (hidden from assistive technology, and when
 * the rows stack on a narrow screen): each cell names itself with a
 * `.cell-label` instead.
 */
const RowList: TOC<{
  Element: HTMLDivElement;
  Args: { label: string };
  Blocks: { header: []; rows: [typeof Row] };
}> = <template>
  <div class="row-list" ...attributes>
    {{#if (has-block "header")}}
      <div class="row-list-header" aria-hidden="true">
        {{yield to="header"}}
      </div>
    {{/if}}
    <ul class="row-list-rows" aria-label={{@label}}>
      {{yield Row to="rows"}}
    </ul>
  </div>
</template>;

export default RowList;
