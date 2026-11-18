import Controller from '@ember/controller';
import { tracked } from '@glimmer/tracking';

/**
 * The filters of the log screen live in the URL, so a reload (or a link)
 * shows the same logs: `?service=web&period=1h&q=error`.
 */
export default class AppLogsController extends Controller {
  queryParams = ['service', 'period', 'from', 'to', 'q'];

  /** One service, or '' for all of them. */
  @tracked service = '';
  /** A `Period` (see utils/logs); anything else is read as the live tail. */
  @tracked period = 'tail';
  /** Custom range, as `datetime-local` values in JST. */
  @tracked from = '';
  @tracked to = '';
  /** Keyword. */
  @tracked q = '';
}
