import AppTabRoute from 'koroku-console/utils/app-tab-route';

const REPLACE = { replace: true };

export default class AppLogsRoute extends AppTabRoute {
  // Changing a filter rewrites the current history entry: typing a keyword
  // must not leave one entry per character behind.
  queryParams = {
    service: REPLACE,
    period: REPLACE,
    from: REPLACE,
    to: REPLACE,
    q: REPLACE,
  };
}
