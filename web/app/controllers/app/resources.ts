import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';

/**
 * `?section=memory`, `?section=idle-sleep` or `?section=compose` leads to that section of the
 * resources tab (ARCHITECTURE 9.1); `compose` also opens it.
 */
export default class AppResourcesController extends Controller {
  queryParams = ['section'];

  @tracked section = '';
}
