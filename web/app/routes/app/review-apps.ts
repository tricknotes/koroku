import { service } from '@ember/service';
import type RouterService from '@ember/routing/router-service';
import AppTabRoute from 'koroku-console/utils/app-tab-route';
import type AppContext from 'koroku-console/utils/app-context';
import { hasReviewTab } from 'koroku-console/utils/review';

/**
 * The review apps of an app (ARCHITECTURE 9.9). Apps without the tab
 * (production apps, review apps) land on their overview instead.
 */
export default class AppReviewAppsRoute extends AppTabRoute {
  @service declare router: RouterService;

  afterModel(context: AppContext): void {
    if (context.loaded && !hasReviewTab(context.app)) {
      this.router.transitionTo('app.index', context.slug);
    }
  }
}
