import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';

/**
 * `?section=env-requirements` leads to where the requirements check reads
 * from (ARCHITECTURE 9.1, 9.4).
 */
export default class AppEnvController extends Controller {
  queryParams = ['section'];

  @tracked section = '';
}
