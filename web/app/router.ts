import EmberRouter from '@embroider/router';
import config from 'koroku-console/config/environment';

export default class Router extends EmberRouter {
  location = config.locationType;
  rootURL = config.rootURL;
}

Router.map(function () {
  this.route('login');
  this.route('new-app', { path: '/apps/new' });
  this.route('app', { path: '/apps/:slug' }, function () {
    this.route('deploy');
    this.route('logs');
    this.route('resources');
    this.route('compose');
    this.route('tasks');
    this.route('env');
    this.route('domains');
    this.route('backups');
    this.route('settings');
  });
  this.route('system');
  this.route('security');
  this.route('members');
  this.route('not-found', { path: '/*path' });
});
