import type { TOC } from '@ember/component/template-only';

/** As many tab placeholders as the app detail has tabs. */
const TABS = [1, 2, 3, 4, 5, 6, 7, 8];
const LINES = [1, 2, 3];

/**
 * The loading substate of `app`: shown by the router while an app loads
 * (routes/app.ts waits for it before the screen changes). It has the shape
 * of the app detail (header, tabs, a card) so the screen that is coming is
 * recognisable, and a thin bar along the top edge. Both appear after a
 * short delay (app-detail.css), so a quick load does not flash.
 */
const AppLoading: TOC<object> = <template>
  <div
    class="app-loading"
    role="status"
    aria-label="アプリを読み込み中"
    data-test-app-loading
  >
    <div class="route-progress" aria-hidden="true">
      <div class="route-progress-bar"></div>
    </div>

    <div aria-hidden="true">
      <div class="app-header">
        <div class="app-header-main">
          <div class="app-header-title">
            <span class="skeleton app-loading-title"></span>
          </div>
          <div class="app-header-meta">
            <span class="skeleton app-loading-meta"></span>
          </div>
        </div>
      </div>

      <div class="tabs app-tabs app-loading-tabs">
        {{#each TABS}}
          <span class="skeleton app-loading-tab"></span>
        {{/each}}
      </div>

      <div class="app-loading-card">
        {{#each LINES}}
          <span class="skeleton app-loading-line"></span>
        {{/each}}
      </div>
    </div>
  </div>
</template>;

export default AppLoading;
