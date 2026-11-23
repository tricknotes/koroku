import AppTabRoute from 'koroku-console/utils/app-tab-route';

export default class AppEnvRoute extends AppTabRoute {
  // Following a link to a section does not add a history entry of its own.
  queryParams = { section: { replace: true } };
}
