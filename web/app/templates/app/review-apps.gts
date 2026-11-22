import type { TOC } from '@ember/component/template-only';
import { pageTitle } from 'ember-page-title';
import ReviewAppsPanel from 'koroku-console/components/review-apps-panel';
import ReviewAppsPermission from 'koroku-console/components/review-apps-permission';
import type AppContext from 'koroku-console/utils/app-context';

/**
 * Review apps of pull requests (ARCHITECTURE 9.9): whether the owner allows
 * them, the ones there are, and creating one.
 */
const AppReviewAppsTemplate: TOC<{ Args: { model: AppContext } }> = <template>
  {{pageTitle "レビューアプリ"}}
  <div class="review-tab" data-test-review-tab>
    <ReviewAppsPermission @context={{@model}} />
    <ReviewAppsPanel @parent={{@model.app}} @onCreated={{@model.reload}} />
  </div>
</template>;

export default AppReviewAppsTemplate;
