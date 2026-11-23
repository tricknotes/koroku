import Route from '@ember/routing/route';
import { service } from '@ember/service';
import type RouterService from '@ember/routing/router-service';

/**
 * A URL with no screen of its own that stands for a section of a tab
 * (ARCHITECTURE 9.1). It is kept for bookmarks and links that point to
 * it, and replaces itself with the tab and its section.
 */
export default class SectionRedirectRoute extends Route {
  @service declare router: RouterService;

  /** The tab's route and the section's ID. */
  target = { route: 'app.index', section: '' };

  beforeModel(): void {
    const { slug } = this.paramsFor('app') as { slug: string };
    void this.router.replaceWith(this.target.route, slug, {
      queryParams: { section: this.target.section },
    });
  }
}
