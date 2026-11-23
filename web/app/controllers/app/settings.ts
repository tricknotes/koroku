import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';

/**
 * `?section=<id>` leads to a section of the settings tab (ARCHITECTURE
 * 9.1); the section index rewrites it as it jumps.
 */
export default class AppSettingsController extends Controller {
  queryParams = ['section'];

  @tracked section = '';
}
