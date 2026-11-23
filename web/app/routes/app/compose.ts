import SectionRedirectRoute from 'koroku-console/utils/section-redirect-route';

export default class AppComposeRoute extends SectionRedirectRoute {
  target = { route: 'app.resources', section: 'compose' };
}
