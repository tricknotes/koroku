import { module, test } from 'qunit';
import {
  GROUP_SERVICE_NAME,
  groupChangeEffect,
  groupHostname,
  joinBlocker,
  looksLikeDatabase,
} from 'koroku-console/utils/groups';
import { makeGroup } from 'koroku-console/tests/helpers/fakes';

module('Unit | Utility | groups', function () {
  test('groupHostname is the name other members connect to', function (assert) {
    assert.strictEqual(
      groupHostname('api', 'shop-api'),
      'api.shop-api.koroku.internal',
    );
  });

  test('GROUP_SERVICE_NAME accepts DNS labels only', function (assert) {
    assert.true(GROUP_SERVICE_NAME.test('api'));
    assert.true(GROUP_SERVICE_NAME.test('auth-db'));
    assert.false(GROUP_SERVICE_NAME.test('my_api'));
    assert.false(GROUP_SERVICE_NAME.test('-api'));
    assert.false(GROUP_SERVICE_NAME.test('Api'));
  });

  test('looksLikeDatabase spots the usual database names', function (assert) {
    for (const name of [
      'db',
      'auth-db',
      'postgres',
      'postgresql',
      'mysql',
      'mariadb',
      'redis',
      'mongo',
      'cache_redis',
    ]) {
      assert.true(looksLikeDatabase(name), name);
    }
    for (const name of ['web', 'api', 'worker', 'dbadmin-ui', 'pgadmin']) {
      assert.false(looksLikeDatabase(name), name);
    }
  });

  test('joinBlocker refuses groups of the other environment', function (assert) {
    const production = makeGroup({ environment: 'production' });
    assert.strictEqual(joinBlocker(production, 'production'), null);
    assert.strictEqual(
      joinBlocker(production, 'staging'),
      '本番のグループです。ステージングのアプリは入れません。',
    );
  });

  test('groupChangeEffect tells widening from narrowing', function (assert) {
    const none = { groupId: null, services: [] };
    const api = { groupId: 3, services: ['api'] };
    assert.deepEqual(groupChangeEffect(none, api), {
      widens: true,
      narrows: false,
    });
    assert.deepEqual(groupChangeEffect(api, none), {
      widens: false,
      narrows: true,
    });
    assert.deepEqual(
      groupChangeEffect(api, { groupId: 3, services: ['api', 'web'] }),
      { widens: true, narrows: false },
    );
    assert.deepEqual(groupChangeEffect(api, { groupId: 3, services: [] }), {
      widens: false,
      narrows: true,
    });
    assert.deepEqual(
      groupChangeEffect(api, { groupId: 4, services: ['api'] }),
      {
        widens: true,
        narrows: true,
      },
    );
    assert.deepEqual(groupChangeEffect(api, api), {
      widens: false,
      narrows: false,
    });
  });
});
