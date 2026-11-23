import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';

/**
 * `?deployment=<id>` opens that deployment in the history. GitHub links
 * each deployment it shows to this page (the `log_url` koroku sends).
 * `?section=review-apps` leads to the review apps (ARCHITECTURE 9.9), and
 * `deploy-settings`, `auto-deploy`, `promotion` or `safeguards` to the
 * deploy settings (ARCHITECTURE 9.1).
 */
export default class AppDeployController extends Controller {
  queryParams = ['deployment', 'section'];

  /** A deployment ID, or '' to open what most likely matters. */
  @tracked deployment = '';

  /** The section to bring into view, or ''. */
  @tracked section = '';
}
