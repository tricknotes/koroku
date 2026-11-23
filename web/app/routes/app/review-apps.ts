import SectionRedirectRoute from 'koroku-console/utils/section-redirect-route';

export default class AppReviewAppsRoute extends SectionRedirectRoute {
  target = { route: 'app.deploy', section: 'review-apps' };
}
