import { module, test } from 'qunit';
import { click, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  makeGroup,
  makeSystem,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, Group } from 'koroku-console/utils/types';

const SHOP = makeGroup({
  id: 3,
  name: 'shop',
  members: [
    {
      slug: 'shop-api',
      services: ['api'],
      hostnames: ['api.shop-api.koroku.internal'],
    },
    { slug: 'shop-web', services: [], hostnames: [] },
  ],
});

const LIVE = makeGroup({
  id: 4,
  name: 'live',
  environment: 'production',
  members: [],
});

module('Acceptance | groups on the system screen', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let groups: Group[];

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    groups = [LIVE, SHOP];
    fakes.api
      .on('GET', '/api/system', makeSystem())
      .on('GET', '/api/groups', () => ({ groups }));
  });

  test('it lists the groups with their members and hostnames', async function (assert) {
    await visit('/system');

    assert.dom('[data-test-group]').exists({ count: 2 });
    const shop = '[data-test-group="shop"]';
    assert.dom(`${shop} [data-test-group-environment]`).hasText('ステージング');
    assert.dom(`${shop} [data-test-group-network]`).hasText('koroku_group_3');
    assert
      .dom(
        `${shop} [data-test-group-member="shop-api"] [data-test-group-hostname]`,
      )
      .hasText('api.shop-api.koroku.internal');
    assert
      .dom(
        `${shop} [data-test-group-member="shop-web"] [data-test-group-no-services]`,
      )
      .exists();
    assert
      .dom(`${shop} [data-test-group-delete]`)
      .isDisabled('a group with members cannot be deleted');
    assert.dom(`${shop} [data-test-group-delete-note]`).exists();

    assert
      .dom('[data-test-group="live"] [data-test-group-environment]')
      .hasText('本番');
    assert.dom('[data-test-group="live"] [data-test-group-empty]').exists();
    assert.dom('[data-test-group="live"] [data-test-group-delete]').isEnabled();
  });

  test('without groups there is one way to make one', async function (assert) {
    groups = [];
    await visit('/system');

    assert.dom('[data-test-groups-empty]').exists();
    assert.dom('[data-test-group-create]').exists({ count: 1 });
  });

  test('a group is created with a name and an environment', async function (assert) {
    fakes.api.on('POST', '/api/groups', (call: FakeCall) => {
      const body = call.body as { name: string; environment: 'production' };
      const created = makeGroup({ id: 5, ...body });
      groups = [...groups, created];
      return created;
    });
    await visit('/system');

    await click('[data-test-group-create]');
    await click('[data-test-dialog-confirm]');
    assert
      .dom('[data-test-field-error]')
      .hasText('名前を入力してください。', 'an empty name is not sent');
    assert.deepEqual(fakes.api.callsTo('POST', '/api/groups'), []);

    await fillIn('[data-test-group-name-input]', '  accounts  ');
    await click('[data-test-group-environment-option="production"]');
    await click('[data-test-dialog-confirm]');

    assert.deepEqual(fakes.api.callsTo('POST', '/api/groups')[0]?.body, {
      name: 'accounts',
      environment: 'production',
    });
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.dom('[data-test-group-notice]').includesText('accounts');
    assert.dom('[data-test-group="accounts"]').exists();
  });

  test('a taken name is said next to the field', async function (assert) {
    fakes.api.fail('POST', '/api/groups', 409, 'group_name_taken');
    await visit('/system');

    await click('[data-test-group-create]');
    await fillIn('[data-test-group-name-input]', 'shop');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-dialog]').exists('the dialog stays open');
    assert
      .dom('[data-test-field-group="group-name"] [data-test-field-error]')
      .hasText('同じ名前のグループがすでにあります。');
  });

  test('a group can be renamed', async function (assert) {
    fakes.api.on('PATCH', '/api/groups/3', (call: FakeCall) => {
      const { name } = call.body as { name: string };
      groups = groups.map((g) => (g.id === 3 ? { ...g, name } : g));
      return groups.find((g) => g.id === 3);
    });
    await visit('/system');

    await click('[data-test-group="shop"] [data-test-group-rename]');
    assert.dom('[data-test-group-name-input]').hasValue('shop');
    await fillIn('[data-test-group-name-input]', 'store');
    await click('[data-test-dialog-confirm]');

    assert.deepEqual(fakes.api.callsTo('PATCH', '/api/groups/3')[0]?.body, {
      name: 'store',
    });
    assert.dom('[data-test-group="store"]').exists();
  });

  test('an empty group can be deleted', async function (assert) {
    fakes.api.on('DELETE', '/api/groups/4', () => {
      groups = groups.filter((g) => g.id !== 4);
      return undefined;
    });
    await visit('/system');

    await click('[data-test-group="live"] [data-test-group-delete]');
    assert.dom('[data-test-dialog]').includesText('koroku_group_4');
    await click('[data-test-dialog-confirm]');

    assert.strictEqual(fakes.api.callsTo('DELETE', '/api/groups/4').length, 1);
    assert.dom('[data-test-group="live"]').doesNotExist();
  });

  test('a group that got members meanwhile is not deleted', async function (assert) {
    fakes.api.fail('DELETE', '/api/groups/4', 409, 'group_not_empty');
    await visit('/system');

    await click('[data-test-group="live"] [data-test-group-delete]');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.dom('[data-test-error]').includesText('メンバーのアプリがいるため');
  });

  test('a backend without groups says so', async function (assert) {
    fakes.api.fail('GET', '/api/groups', 404, 'not_found');
    await visit('/system');

    assert
      .dom('[data-test-groups]')
      .doesNotExist('the section is left out, not a placeholder');
  });
});

module('Acceptance | group of an app', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let groups: Group[];

  function serve(overrides: Partial<App> = {}) {
    const running = makeDeployment();
    app = makeApp({
      slug: 'shop-api',
      current_deployment: running,
      latest_deployment: running,
      group_id: null,
      group_services: [],
      ...overrides,
    });
  }

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    serve();
    groups = [LIVE, SHOP];
    fakes.api
      .on('GET', '/api/apps', () => ({ apps: [app] }))
      .on('GET', '/api/apps/shop-api', () => app)
      .on('GET', '/api/groups', () => ({ groups }))
      .on('GET', '/api/apps/shop-api/services', {
        services: [
          { name: 'api', state: 'running', status: 'Up', health: '' },
          { name: 'db', state: 'running', status: 'Up', health: '' },
          { name: 'my_worker', state: 'running', status: 'Up', health: '' },
        ],
      })
      .on('PUT', '/api/apps/shop-api/group', (call: FakeCall) => {
        const body = call.body as {
          group_id: number | null;
          services: string[];
        };
        app = {
          ...app,
          group_id: body.group_id,
          group_services: body.services,
        };
        return app;
      });
  });

  test('only groups of the same environment can be chosen', async function (assert) {
    await visit('/apps/shop-api/settings');

    assert.dom('[data-test-group-option="none"]').isChecked();
    assert.dom('[data-test-group-option="shop"]').isEnabled();
    assert.dom('[data-test-group-option="live"]').isDisabled();
    assert
      .dom('[data-test-group-option-row="live"] [data-test-group-blocker]')
      .hasText('本番のグループです。ステージングのアプリは入れません。');
    assert
      .dom('[data-test-group-environment-note]')
      .includesText('ステージングのグループにだけ入れます');
    assert
      .dom('[data-test-group-services]')
      .doesNotExist('services are chosen once a group is');
  });

  test('joining sends the group and the chosen services', async function (assert) {
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-option="shop"]');
    assert
      .dom('[data-test-group-reach]')
      .includesText('双方向に、すべてのポートで');
    assert
      .dom('[data-test-group-reach]')
      .includesText('次のデプロイか起動から');
    assert
      .dom('[data-test-group-service-checkbox="my_worker"]')
      .isDisabled('a name that is no DNS label cannot join');

    await click('[data-test-group-service-checkbox="api"]');
    assert
      .dom('[data-test-group-chosen-hostname]')
      .hasText('api.shop-api.koroku.internal');
    assert.dom('[data-test-group-database-warning]').doesNotExist();

    await click('[data-test-group-save]');

    assert.deepEqual(
      fakes.api.callsTo('PUT', '/api/apps/shop-api/group')[0]?.body,
      {
        group_id: 3,
        services: ['api'],
      },
    );
    assert
      .dom('[data-test-group-saved]')
      .includesText('次のデプロイか起動から接続されます');
  });

  test('a service that looks like a database comes with a warning', async function (assert) {
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-option="shop"]');
    assert
      .dom('[data-test-group-service="db"] [data-test-group-service-database]')
      .exists();
    assert
      .dom('[data-test-group-service="api"] [data-test-group-service-database]')
      .doesNotExist();

    await click('[data-test-group-service-checkbox="db"]');
    assert.dom('[data-test-group-database-warning]').includesText('db');
  });

  test('leaving disconnects at once', async function (assert) {
    serve({ group_id: 3, group_services: ['api'] });
    await visit('/apps/shop-api/settings');

    assert.dom('[data-test-group-option="shop"]').isChecked();
    assert.dom('[data-test-group-service-checkbox="api"]').isChecked();
    assert.dom('[data-test-app-group]').hasText('グループ shop');

    await click('[data-test-group-option="none"]');
    await click('[data-test-group-save]');

    assert.deepEqual(
      fakes.api.callsTo('PUT', '/api/apps/shop-api/group')[0]?.body,
      {
        group_id: null,
        services: [],
      },
    );
    assert.dom('[data-test-group-saved]').includesText('切断しました');
  });

  test('nothing is sent without a change', async function (assert) {
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-save]');

    assert.dom('[data-test-group-unchanged]').exists();
    assert.deepEqual(fakes.api.callsTo('PUT', '/api/apps/shop-api/group'), []);
  });

  test('a refused service is said next to the services', async function (assert) {
    fakes.api.fail(
      'PUT',
      '/api/apps/shop-api/group',
      422,
      'validation_failed',
      {
        services: '"api" sets container_name and cannot join a group',
      },
    );
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-option="shop"]');
    await click('[data-test-group-service-checkbox="api"]');
    await click('[data-test-group-save]');

    assert
      .dom('[data-test-field-error="services"]')
      .includesText('container_name か network_mode を指定したサービス')
      .includesText('"api" sets container_name');
  });

  test('a production app asks before saving and may need a fresh login', async function (assert) {
    serve({ environment: 'production' });
    groups = [{ ...LIVE }, SHOP];
    fakes.api.fail('PUT', '/api/apps/shop-api/group', 403, 'reauth_required');
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-option="live"]');
    await click('[data-test-group-service-checkbox="db"]');
    await click('[data-test-group-save]');

    assert.dom('[data-test-group-confirm]').includesText('live');
    assert.dom('[data-test-group-confirm-database]').exists();
    assert.deepEqual(fakes.api.callsTo('PUT', '/api/apps/shop-api/group'), []);

    await click('[data-test-dialog-confirm]');

    assert.strictEqual(
      fakes.api.callsTo('PUT', '/api/apps/shop-api/group').length,
      1,
    );
    assert.dom('[data-test-reauth-dialog]').exists();
  });

  test('the environment stays while the app is in a group', async function (assert) {
    serve({ group_id: 3, group_services: [] });
    await visit('/apps/shop-api/settings');

    assert.dom('[data-test-environment-option="production"]').isDisabled();
    assert.dom('[data-test-environment-group-note]').exists();
  });

  test('without a running service there is nothing to tick yet', async function (assert) {
    serve({ current_deployment: null, latest_deployment: null });
    fakes.api.on('GET', '/api/apps/shop-api/services', { services: [] });
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-option="shop"]');
    assert.dom('[data-test-group-services-empty]').exists();
  });
});
