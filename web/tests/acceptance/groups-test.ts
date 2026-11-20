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
import type {
  App,
  AppLink,
  Group,
  NewAppLink,
} from 'koroku-console/utils/types';

const AUTH_LINK: AppLink = {
  id: 7,
  name: 'auth',
  target_app: 'accounts',
  target_service: 'auth',
  services: ['api', 'worker'],
};

const API_LINK: AppLink = {
  id: 8,
  name: 'api',
  target_app: 'shop-api',
  target_service: 'api',
  services: ['web'],
};

/** accounts exposes auth; shop-api exposes api and links to auth. */
function shopGroup(
  shopApi: { services?: string[]; links?: AppLink[] } = {},
): Group {
  return makeGroup({
    id: 3,
    name: 'shop',
    members: [
      { slug: 'accounts', services: ['auth'], links: [] },
      {
        slug: 'shop-api',
        services: shopApi.services ?? ['api'],
        links: shopApi.links ?? [AUTH_LINK],
      },
      { slug: 'shop-web', services: [], links: [API_LINK] },
    ],
  });
}

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
    groups = [LIVE, shopGroup()];
    fakes.api
      .on('GET', '/api/system', makeSystem())
      .on('GET', '/api/groups', () => ({ groups }));
  });

  test('it lists the groups with what each member exposes and links', async function (assert) {
    await visit('/system');

    assert.dom('[data-test-group]').exists({ count: 2 });
    const shop = '[data-test-group="shop"]';
    assert.dom(`${shop} [data-test-group-environment]`).hasText('ステージング');
    assert
      .dom(
        `${shop} [data-test-group-member="accounts"] [data-test-group-exposed-service]`,
      )
      .hasText('auth');
    assert
      .dom(
        `${shop} [data-test-group-member="accounts"] [data-test-group-no-links]`,
      )
      .exists();
    assert
      .dom(
        `${shop} [data-test-group-member="shop-api"] [data-test-group-link="auth"]`,
      )
      .includesText('shop-api の auth → accounts / auth')
      .includesText('api、worker');
    assert
      .dom(
        `${shop} [data-test-group-member="shop-web"] [data-test-group-no-services]`,
      )
      .exists('nothing exposed');
    assert
      .dom(`${shop} [data-test-group-delete]`)
      .isDisabled('a group with members cannot be deleted');
    assert.dom(`${shop} [data-test-group-delete-note]`).exists();
    assert.dom('[data-test-groups]').doesNotIncludeText('koroku.internal');

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
    assert.dom('[data-test-dialog]').includesText('live');
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

/** The settings tab of shop-api against a fake API that keeps state. */
function setUpApp(hooks: NestedHooks) {
  const context = {} as {
    fakes: Fakes;
    app: App;
    groups: Group[];
    links: AppLink[];
    serve: (overrides?: Partial<App>) => void;
  };

  context.serve = (overrides = {}) => {
    const running = makeDeployment();
    context.app = makeApp({
      slug: 'shop-api',
      current_deployment: running,
      latest_deployment: running,
      group_id: null,
      group_services: [],
      ...overrides,
    });
  };

  hooks.beforeEach(function () {
    const fakes = installFakes(this.owner);
    context.fakes = fakes;
    context.serve();
    context.groups = [LIVE, shopGroup()];
    context.links = [];
    fakes.api
      .on('GET', '/api/apps', () => ({ apps: [context.app] }))
      .on('GET', '/api/apps/shop-api', () => context.app)
      .on('GET', '/api/groups', () => ({ groups: context.groups }))
      .on('GET', '/api/apps/shop-api/links', () => ({ links: context.links }))
      .on('GET', '/api/apps/shop-api/services', {
        services: [
          { name: 'api', state: 'running', status: 'Up', health: '' },
          { name: 'db', state: 'running', status: 'Up', health: '' },
          { name: 'my_worker', state: 'running', status: 'Up', health: '' },
          { name: 'worker', state: 'running', status: 'Up', health: '' },
        ],
      })
      .on('PUT', '/api/apps/shop-api/group', (call: FakeCall) => {
        const body = call.body as {
          group_id: number | null;
          services: string[];
        };
        context.app = {
          ...context.app,
          group_id: body.group_id,
          group_services: body.services,
        };
        if (body.group_id === null) context.links = [];
        return context.app;
      })
      .on('POST', '/api/apps/shop-api/links', (call: FakeCall) => {
        const created = { id: 9, ...(call.body as NewAppLink) };
        context.links = [...context.links, created];
        return created;
      })
      .on('DELETE', '/api/apps/shop-api/links/7', () => {
        context.links = context.links.filter((l) => l.id !== 7);
        return undefined;
      });
  });

  return context;
}

module('Acceptance | group of an app', function (hooks) {
  setupApplicationTest(hooks);
  const ctx = setUpApp(hooks);

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
    assert.dom('[data-test-links-no-group]').exists();
    assert
      .dom('[data-test-group-settings]')
      .doesNotIncludeText('koroku.internal');
  });

  test('joining exposes the chosen services and connects nothing', async function (assert) {
    ctx.groups = [LIVE, shopGroup({ services: [], links: [] })];
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-option="shop"]');
    assert.dom('[data-test-group-services] legend').hasText('公開するサービス');
    assert
      .dom('[data-test-group-reach]')
      .includesText('双方向に、すべてのポートで');
    assert
      .dom('[data-test-group-service-checkbox="my_worker"]')
      .isDisabled('a name that is no DNS label cannot be exposed');

    await click('[data-test-group-service-checkbox="api"]');
    assert.dom('[data-test-group-database-warning]').doesNotExist();
    assert.dom('[data-test-group-removed]').doesNotExist();

    await click('[data-test-group-save]');

    assert.dom('[data-test-dialog]').doesNotExist('nothing to confirm');
    assert.deepEqual(
      ctx.fakes.api.callsTo('PUT', '/api/apps/shop-api/group')[0]?.body,
      { group_id: 3, services: ['api'] },
    );
    assert
      .dom('[data-test-group-saved]')
      .includesText('ほかのアプリが接続を作るとつながります');
    assert
      .dom('[data-test-links-empty]')
      .exists('the links section opens once the app is in the group');
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

  test('leaving names the links it deletes and asks first', async function (assert) {
    ctx.serve({ group_id: 3, group_services: ['api'] });
    ctx.links = [AUTH_LINK];
    await visit('/apps/shop-api/settings');

    assert.dom('[data-test-group-option="shop"]').isChecked();
    assert.dom('[data-test-group-service-checkbox="api"]').isChecked();
    assert
      .dom('[data-test-group-service="api"] [data-test-group-service-used-by]')
      .includesText('shop-web の api');
    assert.dom('[data-test-app-group]').hasText('グループ shop');

    await click('[data-test-group-option="none"]');
    assert.dom('[data-test-group-removed-link]').exists({ count: 2 });
    assert
      .dom('[data-test-group-removed]')
      .includesText('shop-api の auth → accounts / auth')
      .includesText('shop-web の api → shop-api / api');

    await click('[data-test-group-save]');

    assert
      .dom('[data-test-group-confirm-removed]')
      .includesText('shop-api の auth → accounts / auth')
      .includesText('shop-web の api → shop-api / api');
    assert.deepEqual(
      ctx.fakes.api.callsTo('PUT', '/api/apps/shop-api/group'),
      [],
      'nothing is sent before confirming',
    );

    await click('[data-test-dialog-confirm]');

    assert.deepEqual(
      ctx.fakes.api.callsTo('PUT', '/api/apps/shop-api/group')[0]?.body,
      { group_id: null, services: [] },
    );
    assert.dom('[data-test-group-saved]').includesText('2 件の接続を削除');
    assert.dom('[data-test-links-no-group]').exists();
  });

  test('un-exposing a service deletes only the links to it', async function (assert) {
    ctx.serve({ group_id: 3, group_services: ['api'] });
    ctx.links = [AUTH_LINK];
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-service-checkbox="api"]');
    assert.dom('[data-test-group-removed-link]').exists({ count: 1 });
    assert
      .dom('[data-test-group-removed-link]')
      .hasText('shop-web の api → shop-api / api');

    await click('[data-test-group-save]');
    await click('[data-test-dialog-confirm]');

    assert.deepEqual(
      ctx.fakes.api.callsTo('PUT', '/api/apps/shop-api/group')[0]?.body,
      { group_id: 3, services: [] },
    );
    assert.dom('[data-test-group-saved]').includesText('1 件の接続を削除');
  });

  test('nothing is sent without a change', async function (assert) {
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-save]');

    assert.dom('[data-test-group-unchanged]').exists();
    assert.deepEqual(
      ctx.fakes.api.callsTo('PUT', '/api/apps/shop-api/group'),
      [],
    );
  });

  test('a refused service is said next to the services', async function (assert) {
    ctx.fakes.api.fail(
      'PUT',
      '/api/apps/shop-api/group',
      422,
      'validation_failed',
      { services: '"api" sets container_name and cannot be exposed' },
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
    ctx.serve({ environment: 'production' });
    ctx.fakes.api.fail(
      'PUT',
      '/api/apps/shop-api/group',
      403,
      'reauth_required',
    );
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-option="live"]');
    await click('[data-test-group-service-checkbox="db"]');
    await click('[data-test-group-save]');

    assert.dom('[data-test-group-confirm]').includesText('live');
    assert.dom('[data-test-group-confirm-database]').exists();
    assert.dom('[data-test-group-confirm-removed]').doesNotExist();
    assert.deepEqual(
      ctx.fakes.api.callsTo('PUT', '/api/apps/shop-api/group'),
      [],
    );

    await click('[data-test-dialog-confirm]');

    assert.strictEqual(
      ctx.fakes.api.callsTo('PUT', '/api/apps/shop-api/group').length,
      1,
    );
    assert.dom('[data-test-reauth-dialog]').exists();
  });

  test('the environment stays while the app is in a group', async function (assert) {
    ctx.serve({ group_id: 3, group_services: [] });
    await visit('/apps/shop-api/settings');

    assert.dom('[data-test-environment-option="production"]').isDisabled();
    assert.dom('[data-test-environment-group-note]').exists();
  });

  test('without a running service there is nothing to tick yet', async function (assert) {
    ctx.serve({ current_deployment: null, latest_deployment: null });
    ctx.fakes.api.on('GET', '/api/apps/shop-api/services', { services: [] });
    await visit('/apps/shop-api/settings');

    await click('[data-test-group-option="shop"]');
    assert.dom('[data-test-group-services-empty]').exists();
  });
});

module('Acceptance | links of an app', function (hooks) {
  setupApplicationTest(hooks);
  const ctx = setUpApp(hooks);

  hooks.beforeEach(function () {
    ctx.serve({ group_id: 3, group_services: ['api'] });
  });

  test('it lists the links with their target and services', async function (assert) {
    ctx.links = [{ ...AUTH_LINK, network: 'koroku_link_1a2b3c4d_7' }];
    await visit('/apps/shop-api/settings');

    const row = '[data-test-link="auth"]';
    assert.dom(`${row} [data-test-link-name]`).hasText('auth');
    assert.dom(`${row} [data-test-link-target]`).hasText('accounts / auth');
    assert.dom(`${row} [data-test-link-services]`).includesText('api、worker');
    assert.dom('[data-test-link-add]').isEnabled();
  });

  test('a link is added with a name, a target and the services using it', async function (assert) {
    await visit('/apps/shop-api/settings');

    assert.dom('[data-test-links-empty]').exists();
    await click('[data-test-link-add]');

    assert
      .dom('[data-test-link-target-input]')
      .exists({ count: 1 }, 'only exposed services of other members');
    assert.dom('[data-test-link-target-input="accounts/auth"]').exists();
    assert
      .dom('[data-test-link-consumer-checkbox="my_worker"]')
      .isDisabled('a name that is no DNS label cannot use a link');

    await click('[data-test-dialog-confirm]');
    assert
      .dom('[data-test-field-group="link-name"] [data-test-field-error]')
      .hasText('名前を入力してください。');
    assert.dom('[data-test-field-error="target"]').exists();
    assert.dom('[data-test-field-error="services"]').exists();
    assert.deepEqual(
      ctx.fakes.api.callsTo('POST', '/api/apps/shop-api/links'),
      [],
    );

    await click('[data-test-link-target-input="accounts/auth"]');
    assert
      .dom('[data-test-link-name-input]')
      .hasValue('auth', 'the target service is the suggested name');

    await fillIn('[data-test-link-name-input]', 'db');
    await click('[data-test-link-consumer-checkbox="api"]');
    await click('[data-test-dialog-confirm]');
    assert
      .dom('[data-test-field-group="link-name"] [data-test-field-error]')
      .includesText('このアプリのサービスと同じ名前は使えません');
    assert.deepEqual(
      ctx.fakes.api.callsTo('POST', '/api/apps/shop-api/links'),
      [],
    );

    await fillIn('[data-test-link-name-input]', 'auth');
    assert
      .dom('[data-test-link-preview]')
      .includesText('shop-api の auth → accounts / auth');
    await click('[data-test-dialog-confirm]');

    assert.deepEqual(
      ctx.fakes.api.callsTo('POST', '/api/apps/shop-api/links')[0]?.body,
      {
        name: 'auth',
        target_app: 'accounts',
        target_service: 'auth',
        services: ['api'],
      },
    );
    assert.dom('[data-test-dialog]').doesNotExist();
    assert
      .dom('[data-test-link-notice]')
      .includesText('接続 auth を追加しました');
    assert.dom('[data-test-link="auth"]').exists();
  });

  test('a taken name is said next to the name', async function (assert) {
    ctx.fakes.api.fail(
      'POST',
      '/api/apps/shop-api/links',
      409,
      'link_name_taken',
    );
    await visit('/apps/shop-api/settings');

    await click('[data-test-link-add]');
    await click('[data-test-link-target-input="accounts/auth"]');
    await fillIn('[data-test-link-name-input]', 'sso');
    await click('[data-test-link-consumer-checkbox="api"]');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-dialog]').exists('the dialog stays open');
    assert
      .dom('[data-test-field-group="link-name"] [data-test-field-error]')
      .hasText('このアプリには、同じ名前の接続がすでにあります。');
  });

  test('a refused consumer service is said next to the services', async function (assert) {
    ctx.fakes.api.fail(
      'POST',
      '/api/apps/shop-api/links',
      422,
      'validation_failed',
      { services: '"worker" sets network_mode and cannot use a link' },
    );
    await visit('/apps/shop-api/settings');

    await click('[data-test-link-add]');
    await click('[data-test-link-target-input="accounts/auth"]');
    await click('[data-test-link-consumer-checkbox="worker"]');
    await click('[data-test-dialog-confirm]');

    assert
      .dom('[data-test-field-error="services"]')
      .hasText('"worker" sets network_mode and cannot use a link');
    assert.dom('[data-test-dialog-error]').doesNotExist();
  });

  test('without exposed services of others there is nothing to link to', async function (assert) {
    ctx.groups = [
      makeGroup({
        id: 3,
        name: 'shop',
        members: [
          { slug: 'shop-api', services: ['api'], links: [] },
          { slug: 'shop-web', services: [], links: [] },
        ],
      }),
    ];
    await visit('/apps/shop-api/settings');

    assert.dom('[data-test-links-no-targets]').includesText('shop');
    assert
      .dom('[data-test-links-open-member]')
      .hasText('shop-web の設定を開く');
    assert.dom('[data-test-link-add]').doesNotExist();
  });

  test('a link is deleted after a confirmation', async function (assert) {
    ctx.links = [AUTH_LINK];
    await visit('/apps/shop-api/settings');

    await click('[data-test-link="auth"] [data-test-link-delete]');
    assert
      .dom('[data-test-link-delete-confirm]')
      .includesText('shop-api の auth → accounts / auth');
    await click('[data-test-dialog-confirm]');

    assert.strictEqual(
      ctx.fakes.api.callsTo('DELETE', '/api/apps/shop-api/links/7').length,
      1,
    );
    assert.dom('[data-test-link="auth"]').doesNotExist();
    assert.dom('[data-test-link-notice]').includesText('切断しました');
  });

  test('a production app may need a fresh login to change a link', async function (assert) {
    ctx.serve({
      environment: 'production',
      group_id: 3,
      group_services: ['api'],
    });
    ctx.links = [AUTH_LINK];
    ctx.fakes.api.fail(
      'DELETE',
      '/api/apps/shop-api/links/7',
      403,
      'reauth_required',
    );
    await visit('/apps/shop-api/settings');

    await click('[data-test-link="auth"] [data-test-link-delete]');
    assert
      .dom('[data-test-dialog]')
      .includesText('本番アプリの接続を削除しますか？');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-reauth-dialog]').exists();
    assert.dom('[data-test-link="auth"]').exists('the link stays');
  });

  test('the add dialog of a production app says so', async function (assert) {
    ctx.serve({
      environment: 'production',
      group_id: 3,
      group_services: [],
    });
    await visit('/apps/shop-api/settings');

    await click('[data-test-link-add]');
    assert.dom('[data-test-link-production]').exists();
  });
});
