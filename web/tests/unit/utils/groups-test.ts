import { module, test } from 'qunit';
import {
  DNS_LABEL,
  joinBlocker,
  linkLabel,
  linkNameProblem,
  linkTargets,
  looksLikeDatabase,
  removedLinks,
} from 'koroku-console/utils/groups';
import { makeGroup } from 'koroku-console/tests/helpers/fakes';
import type { AppLink } from 'koroku-console/utils/types';

const AUTH: AppLink = {
  id: 7,
  name: 'auth',
  target_app: 'accounts',
  target_service: 'auth',
  services: ['web'],
};
const MAILER: AppLink = {
  id: 8,
  name: 'mail',
  target_app: 'accounts',
  target_service: 'mailer',
  services: ['worker'],
};
const API: AppLink = {
  id: 9,
  name: 'shop',
  target_app: 'shop',
  target_service: 'api',
  services: ['web'],
};

const GROUP = makeGroup({
  id: 3,
  members: [
    { slug: 'accounts', services: ['mailer', 'auth'], links: [] },
    { slug: 'blog', services: [], links: [API] },
    { slug: 'shop', services: ['api'], links: [AUTH, MAILER] },
  ],
});

module('Unit | Utility | groups', function () {
  test('DNS_LABEL accepts DNS labels only', function (assert) {
    assert.true(DNS_LABEL.test('api'));
    assert.true(DNS_LABEL.test('auth-db'));
    assert.false(DNS_LABEL.test('my_api'));
    assert.false(DNS_LABEL.test('-api'));
    assert.false(DNS_LABEL.test('Api'));
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

  test('linkLabel names the consumer, the name and the target', function (assert) {
    assert.strictEqual(
      linkLabel('shop', AUTH),
      'shop の auth → accounts / auth',
    );
  });

  test('linkTargets are the exposed services of the other members', function (assert) {
    assert.deepEqual(
      linkTargets(GROUP, 'shop').map((target) => target.key),
      ['accounts/auth', 'accounts/mailer'],
    );
    assert.deepEqual(
      linkTargets(GROUP, 'blog').map((target) => target.key),
      ['accounts/auth', 'accounts/mailer', 'shop/api'],
    );
    assert.deepEqual(linkTargets(null, 'shop'), []);
  });

  test('removedLinks: leaving takes the own links and the links to the app', function (assert) {
    const labels = removedLinks(GROUP, 'shop', {
      groupId: null,
      services: [],
    }).map((link) => link.label);
    assert.deepEqual(labels, [
      'blog の shop → shop / api',
      'shop の auth → accounts / auth',
      'shop の mail → accounts / mailer',
    ]);
    assert.strictEqual(
      removedLinks(GROUP, 'shop', { groupId: 5, services: ['api'] }).length,
      3,
      'moving to another group is leaving this one',
    );
  });

  test('removedLinks: un-exposing takes the links to that service only', function (assert) {
    assert.deepEqual(
      removedLinks(GROUP, 'accounts', {
        groupId: 3,
        services: ['auth'],
      }).map((link) => link.label),
      ['shop の mail → accounts / mailer'],
    );
    assert.deepEqual(
      removedLinks(GROUP, 'accounts', {
        groupId: 3,
        services: ['auth', 'mailer', 'auth-db'],
      }),
      [],
      'exposing more removes nothing',
    );
    assert.deepEqual(
      removedLinks(null, 'accounts', { groupId: 3, services: [] }),
      [],
      'an app in no group has no links',
    );
  });

  test('linkNameProblem says why a name cannot be used', function (assert) {
    const own = ['web', 'db'];
    const taken = ['auth'];
    assert.strictEqual(linkNameProblem('accounts', own, taken), null);
    assert.strictEqual(
      linkNameProblem('', own, taken),
      '名前を入力してください。',
    );
    assert.true(
      linkNameProblem('Auth', own, taken)?.startsWith(
        '英小文字・数字・ハイフン',
      ),
    );
    assert.true(
      linkNameProblem('db', own, taken)?.startsWith(
        'このアプリのサービスと同じ名前',
      ),
    );
    assert.strictEqual(
      linkNameProblem('auth', own, taken),
      'このアプリには、同じ名前の接続がすでにあります。',
    );
    assert.true(
      linkNameProblem('a'.repeat(64), own, taken)?.includes('63 文字'),
    );
  });
});
