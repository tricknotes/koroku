import SectionRedirectRoute from 'koroku-console/utils/section-redirect-route';

export default class AppDomainsRoute extends SectionRedirectRoute {
  target = { route: 'app.settings', section: 'domains' };
}
