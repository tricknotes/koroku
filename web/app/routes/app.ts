import Route from '@ember/routing/route';
import { service } from '@ember/service';
import type ApiService from 'koroku-console/services/api';
import AppContext from 'koroku-console/utils/app-context';

export default class AppRoute extends Route {
  @service declare api: ApiService;

  async model(params: { slug: string }): Promise<AppContext> {
    const context = new AppContext(this.api, params.slug);
    // A failure is kept on the context and rendered by the template.
    await context.load();
    return context;
  }
}
