import { module, test } from 'qunit';
import {
  fillIn,
  find,
  settled,
  visit,
  waitFor,
  waitUntil,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  serveConsole,
  type Console,
} from 'koroku-console/tests/helpers/console';
import {
  installFakes,
  makeBackups,
  makeSystem,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';

// The loading state of the screens whose own tests start from loaded data:
// the system page, the backups tab and the form to create an app. Each read
// is held open, looked at, and then answered.
module('Acceptance | loading states', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let state: Console;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    state = serveConsole(fakes);
  });

  test('system: each part stands in for itself until its own answer arrives', async function (assert) {
    const system = fakes.api.hold('GET', '/api/system');
    const docker = fakes.api.hold('GET', '/api/system/docker');
    const containers = fakes.api.hold('GET', '/api/system/containers');

    const visiting = visit('/system');
    await waitFor('[data-test-loading]');

    assert.dom('h1').hasText('システム', 'the page frame is there at once');
    assert.dom('[data-test-system-tab]').exists({ count: 3 });
    assert
      .dom('[data-test-loading]')
      .hasAttribute('role', 'status')
      .hasAria('label', '読み込み中');
    assert.dom('[data-test-loading] .skeleton').exists();
    assert.dom('[data-test-docker-loading] .skeleton').exists();
    assert.dom('[data-test-containers-loading]').hasAttribute('role', 'status');
    assert.dom('[data-test-containers-loading] .skeleton').exists();
    assert.dom('[data-test-resource]').doesNotExist();
    assert.dom('[data-test-docker-item]').doesNotExist();
    assert.dom('[data-test-error]').doesNotExist();
    assert.dom('[data-test-stale]').doesNotExist();

    // One answer does not wait for the others.
    docker.release({
      items: [
        {
          type: 'images',
          total_bytes: 1024 ** 3,
          reclaimable_bytes: 0,
          count: 3,
        },
      ],
    });
    await waitFor('[data-test-docker-item]');

    assert.dom('[data-test-docker-loading]').doesNotExist();
    assert.dom('[data-test-loading]').exists('the host is still loading');

    system.release(makeSystem());
    containers.release({ cpu_count: 2, memory_total_bytes: 1024, groups: [] });
    await visiting;

    assert.dom('[data-test-loading]').doesNotExist();
    assert.dom('[data-test-resource="disk"]').exists();
    assert.dom('[data-test-containers-empty]').exists();
    assert.dom('.skeleton').doesNotExist('no placeholder is left behind');
  });

  test('system jobs: the backup stands in for itself until it arrives', async function (assert) {
    fakes.api.on('GET', '/api/system', makeSystem());
    const backups = fakes.api.hold('GET', '/api/backups');

    const visiting = visit('/system/jobs');
    await waitFor('[data-test-job="backup"] .skeleton');

    assert.dom('[data-test-job="backup"]').includesText('読み込み中');
    assert
      .dom('[data-test-backup-run]')
      .doesNotExist('nothing to run before the state is known');

    backups.release(makeBackups());
    await visiting;

    assert.dom('[data-test-job="backup"] .skeleton').doesNotExist();
    assert.dom('[data-test-backup-run]').exists();
  });

  test('backups: the status first, then snapshots and volumes', async function (assert) {
    const overview = fakes.api.hold('GET', '/api/backups');
    const snapshots = fakes.api.hold('GET', '/api/apps/blog/backups');

    const visiting = visit('/apps/blog/backups');
    await waitFor('[data-test-loading]');

    assert
      .dom('[data-test-loading]')
      .hasText('読み込み中…', 'one plain line while nothing is known');
    assert.dom('[data-test-backup-summary]').doesNotExist();
    assert.dom('[data-test-snapshot]').doesNotExist();
    assert.dom('[data-test-backup-run]').doesNotExist();
    assert.deepEqual(
      fakes.api.callsTo('GET', '/api/apps/blog/backups'),
      [],
      'snapshots are not asked for before backups are known to exist',
    );

    overview.release(state.backups);
    await waitFor('[data-test-backup-summary]');

    assert.dom('[data-test-loading]').hasAttribute('role', 'status');
    assert.dom('[data-test-loading] .skeleton').exists();
    assert.dom('[data-test-snapshot]').doesNotExist();
    assert
      .dom('[data-test-no-snapshots]')
      .doesNotExist('not "empty" while the list is still loading');

    snapshots.release({ snapshots: state.snapshots });
    await visiting;

    assert.dom('[data-test-loading]').doesNotExist();
    assert.dom('[data-test-snapshot]').exists({ count: 2 });
    assert.dom('[data-test-error]').doesNotExist();
  });

  test('create app: the repository list, then the Compose detection', async function (assert) {
    const repos = fakes.api.hold('GET', '/api/repos');
    const compose = fakes.api.hold('GET', '/api/repos/octo/blog/compose');

    const visiting = visit('/apps/new');
    await waitFor('[data-test-loading]');

    assert
      .dom('[data-test-loading]')
      .includesText('リポジトリを読み込み中')
      .hasAttribute('aria-live', 'polite');
    assert.dom('[data-test-field="repo_full_name"]').doesNotExist();
    assert.dom('[data-test-error]').doesNotExist();

    repos.release({
      repos: [
        {
          id: 1,
          full_name: 'octo/blog',
          private: true,
          default_branch: 'main',
        },
      ],
    });
    await visiting;

    assert.dom('[data-test-loading]').doesNotExist();
    assert.dom('[data-test-compose-loading]').doesNotExist();

    const choosing = fillIn('[data-test-field="repo_full_name"]', 'octo/blog');
    await waitFor('[data-test-compose-loading]');

    assert
      .dom('[data-test-compose-loading]')
      .includesText('Compose ファイルを検出中');
    assert
      .dom('[data-test-field="slug"]')
      .hasValue('blog', 'the form does not wait for the detection');

    compose.release({
      files: ['compose.yaml'],
      path: 'compose.yaml',
      services: [{ name: 'web', ports: [3000] }],
    });
    await choosing;

    assert.dom('[data-test-compose-loading]').doesNotExist();
    assert.dom('[data-test-field="web_port"]').hasValue('3000');
  });

  test('create app: a detection that answers late does not fill in another repository', async function (assert) {
    fakes.api.on('GET', '/api/repos', {
      repos: [
        {
          id: 1,
          full_name: 'octo/blog',
          private: true,
          default_branch: 'main',
        },
        {
          id: 2,
          full_name: 'octo/shop',
          private: true,
          default_branch: 'main',
        },
      ],
    });
    const slow = fakes.api.hold('GET', '/api/repos/octo/blog/compose');
    fakes.api.on('GET', '/api/repos/octo/shop/compose', {
      files: ['compose.yaml'],
      path: 'compose.yaml',
      services: [{ name: 'storefront', ports: [8080] }],
    });
    await visit('/apps/new');

    const first = fillIn('[data-test-field="repo_full_name"]', 'octo/blog');
    await waitFor('[data-test-compose-loading]');
    // The reader changes their mind before the first answer is in.
    const select = find(
      '[data-test-field="repo_full_name"]',
    ) as HTMLSelectElement;
    select.value = 'octo/shop';
    select.dispatchEvent(new Event('change', { bubbles: true }));
    await waitUntil(
      () =>
        (find('[data-test-field="web_port"]') as HTMLInputElement | null)
          ?.value === '8080',
    );

    slow.release({
      files: ['compose.yaml'],
      path: 'compose.yaml',
      services: [{ name: 'web', ports: [3000] }],
    });
    await first;
    await settled();

    assert.dom('[data-test-field="repo_full_name"]').hasValue('octo/shop');
    assert.dom('[data-test-field="slug"]').hasValue('shop');
    assert
      .dom('[data-test-field="web_service"]')
      .hasValue('storefront', 'the service of the repository on screen');
    assert.dom('[data-test-field="web_port"]').hasValue('8080');
  });
});
