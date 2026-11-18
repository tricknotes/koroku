import Route from '@ember/routing/route';
import type AppContext from 'koroku-console/utils/app-context';

/** Base of the app detail tabs: they all share the parent's context. */
export default class AppTabRoute extends Route {
  model(): AppContext {
    return this.modelFor('app') as AppContext;
  }
}
