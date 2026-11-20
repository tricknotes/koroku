import Application from '@ember/application';
import compatModules from '@embroider/virtual/compat-modules';
import Resolver from 'ember-resolver';
import loadInitializers from 'ember-load-initializers';
import config from 'koroku-console/config/environment';
import { importSync, isDevelopingApp, macroCondition } from '@embroider/macros';
import setupInspector from '@embroider/legacy-inspector-support/ember-source-4.12';

// Stylesheets, in cascade order. Later files may override earlier ones.
import './styles/tokens.css';
import './styles/base.css';
import './styles/components.css';
import './styles/shell.css';
import './styles/app-list.css';
import './styles/app-detail.css';
import './styles/deploy.css';
import './styles/logs.css';
import './styles/panes.css';
import './styles/tab-section.css';
import './styles/overview.css';
import './styles/env.css';
import './styles/domains.css';
import './styles/backups.css';
import './styles/db-copy.css';
import './styles/settings.css';
import './styles/system.css';
import './styles/security.css';
import './styles/new-app.css';

if (macroCondition(isDevelopingApp())) {
  importSync('./deprecation-workflow');
}

export default class App extends Application {
  modulePrefix = config.modulePrefix;
  podModulePrefix = config.podModulePrefix;
  Resolver = Resolver.withModules(compatModules);
  inspector = setupInspector(this);
}

loadInitializers(App, config.modulePrefix, compatModules);
