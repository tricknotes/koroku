import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';

/**
 * `?deployment=<id>` opens that deployment in the history. GitHub links
 * each deployment it shows to this page (the `log_url` koroku sends).
 */
export default class AppDeployController extends Controller {
  queryParams = ['deployment'];

  /** A deployment ID, or '' to open what most likely matters. */
  @tracked deployment = '';
}
