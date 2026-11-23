import AppTabRoute from 'koroku-console/utils/app-tab-route';

export default class AppSettingsRoute extends AppTabRoute {
  // Jumping between sections rewrites the current history entry.
  queryParams = { section: { replace: true } };
}
