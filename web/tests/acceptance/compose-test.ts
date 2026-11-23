import { module, test } from 'qunit';
import { click, currentURL, fillIn, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import { serveConsole } from 'koroku-console/tests/helpers/console';
import {
  CONFIG,
  OVERRIDE,
  makeComposeView,
  makeContainer,
  makeContainerView,
} from 'koroku-console/tests/helpers/compose';
import { installFakes, type Fakes } from 'koroku-console/tests/helpers/fakes';

const MB = 1024 ** 2;
const APP = '/api/apps/blog';

// The Compose section of the resources tab (ARCHITECTURE 9.10).
module('Acceptance | compose', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    serveConsole(fakes);
  });

  test('it shows the containers, the override and the merged configuration', async function (assert) {
    await visit('/apps/blog/resources?section=compose');

    assert.strictEqual(
      currentURL(),
      '/apps/blog/resources?section=compose',
      'the URL of the section stands for it',
    );
    assert
      .dom('[data-test-tab="resources"]')
      .hasAttribute('aria-current', 'page');
    assert
      .dom('[data-test-compose-toggle]')
      .hasAttribute('aria-expanded', 'true');
    assert.dom('[data-test-compose-section]').isFocused();
    assert
      .dom('[data-test-container="web"]')
      .includesText('稼働中')
      .includesText('稼働中のリリース')
      .includesText('#42 のデプロイ（イメージのタグから）')
      .includesText('127.0.0.1:20000 → 3000/tcp');
    assert
      .dom('[data-test-container="web"] [data-test-mismatch]')
      .doesNotExist();
    assert.dom('[data-test-compose-trouble]').doesNotExist();
    assert
      .dom('[data-test-container-limit]')
      .hasText('512.0 MB', 'no expected value when it matches');

    assert.dom('[data-test-compose-override] pre').hasText(OVERRIDE.trim());
    assert.dom('[data-test-compose-config] pre').hasText(CONFIG.trim());
    assert.dom('[data-test-compose-release] option').exists({ count: 2 });
    assert
      .dom('[data-test-compose-release] option:checked')
      .hasText(`#42 ${'a'.repeat(7)} Fix the header（稼働中）`);
    assert.strictEqual(fakes.api.callsTo('GET', `${APP}/compose`).length, 1);
    assert.strictEqual(fakes.api.callsTo('GET', `${APP}/containers`).length, 1);
  });

  test('the section asks Docker only once it is opened', async function (assert) {
    await visit('/apps/blog/resources');

    assert.dom('[data-test-compose-section] h2').hasText('構成');
    assert
      .dom('[data-test-compose-toggle]')
      .hasAttribute('aria-expanded', 'false')
      .hasText('表示');
    assert.dom('[data-test-compose]').doesNotExist();
    assert.strictEqual(fakes.api.callsTo('GET', `${APP}/compose`).length, 0);
    assert.strictEqual(fakes.api.callsTo('GET', `${APP}/containers`).length, 0);

    await click('[data-test-compose-toggle]');
    assert
      .dom('[data-test-compose-toggle]')
      .hasAttribute('aria-expanded', 'true');
    assert.dom('[data-test-compose-override] pre').hasText(OVERRIDE.trim());
    assert.strictEqual(fakes.api.callsTo('GET', `${APP}/compose`).length, 1);

    await click('[data-test-compose-toggle]');
    assert.dom('[data-test-compose]').doesNotExist();
    await click('[data-test-compose-toggle]');
    assert.strictEqual(
      fakes.api.callsTo('GET', `${APP}/compose`).length,
      1,
      'opening again shows what was read',
    );
  });

  test('it marks containers that differ from the release', async function (assert) {
    fakes.api.on(
      'GET',
      `${APP}/containers`,
      makeContainerView([
        makeContainer({
          oom_killed: true,
          restart_count: 3,
          mismatches: ['restarted', 'oom_killed'],
        }),
        makeContainer({
          name: 'koroku-blog-worker-1',
          service: 'worker',
          current: false,
          deployment_id: 40,
          matched_by: 'created_at',
          memory_limit_bytes: 384 * MB,
          mismatches: ['outdated', 'memory_limit'],
        }),
      ]),
    );
    await visit('/apps/blog/resources?section=compose');

    assert
      .dom('[data-test-compose-trouble]')
      .includesText('2 個のコンテナのうち 2 個');
    const web = '[data-test-container="web"]';
    assert.dom(`${web} [data-test-mismatch]`).exists({ count: 2 });
    assert
      .dom(`${web} [data-test-mismatch="oom_killed"]`)
      .includesText('メモリ不足で停止');
    assert
      .dom(`${web} [data-test-mismatch="restarted"]`)
      .includesText('自動の再起動 3 回');
    const worker = '[data-test-container="worker"]';
    assert.dom(`${worker} [data-test-container-current]`).doesNotExist();
    assert
      .dom(`${worker} [data-test-mismatch="outdated"]`)
      .includesText('古い設定');
    assert
      .dom(`${worker} [data-test-container-limit]`)
      .includesText('384.0 MB')
      .includesText('リリースでは 512.0 MB');
    assert
      .dom(`${worker} [data-test-container-created-by]`)
      .hasText('#40 のデプロイ（作成時刻からの推定）');
  });

  test('it reads a kept release when one is chosen', async function (assert) {
    fakes.api.on(
      'GET',
      `${APP}/compose`,
      (call: { query: Record<string, string> }) =>
        call.query['deployment'] === '41'
          ? makeComposeView({ deployment_id: 41, override: 'older: true\n' })
          : makeComposeView(),
    );
    await visit('/apps/blog/resources?section=compose');
    await fillIn('[data-test-compose-release]', '41');

    assert.dom('[data-test-compose-override] pre').hasText('older: true');
    assert.deepEqual(
      fakes.api.callsTo('GET', `${APP}/compose`).map((c) => c.query),
      [{}, { deployment: '41' }],
    );
    // The containers are always compared with the current release.
    assert.strictEqual(fakes.api.callsTo('GET', `${APP}/containers`).length, 1);
  });

  test('it explains a release that is no longer kept', async function (assert) {
    fakes.api.fail('GET', `${APP}/compose`, 409, 'release_unavailable');
    await visit('/apps/blog/resources?section=compose');

    assert
      .dom('[data-test-compose-release-gone]')
      .includesText('この版のファイルはもう残っていません');
    fakes.api.on('GET', `${APP}/compose`, makeComposeView());
    await click('[data-test-compose-release-gone] button');
    assert.dom('[data-test-compose-override]').exists();
  });

  test('it shows a failing configuration next to the override', async function (assert) {
    fakes.api.on(
      'GET',
      `${APP}/compose`,
      makeComposeView({
        config: null,
        config_error: 'docker compose config: exit status 15: invalid project',
      }),
    );
    await visit('/apps/blog/resources?section=compose');

    assert.dom('[data-test-compose-override] pre').exists();
    assert
      .dom('[data-test-compose-config-error]')
      .includesText('docker compose config が失敗しました')
      .includesText('invalid project');
  });

  test('an app that was never deployed', async function (assert) {
    fakes.api.fail('GET', `${APP}/compose`, 409, 'not_deployed').on(
      'GET',
      `${APP}/containers`,
      makeContainerView([], {
        compared: false,
        current_deployment_id: null,
      }),
    );
    await visit('/apps/blog/resources?section=compose');

    assert
      .dom('[data-test-compose-not-deployed]')
      .includesText('まだデプロイされていません');
    assert.dom('[data-test-compose-containers-empty]').exists();
    assert.dom('[data-test-error]').doesNotExist();
  });

  test('errors are shown with a way to retry', async function (assert) {
    fakes.api
      .fail('GET', `${APP}/compose`, 500, 'internal')
      .fail('GET', `${APP}/containers`, 500, 'internal');
    await visit('/apps/blog/resources?section=compose');

    assert.dom('[data-test-compose-containers] [data-test-error]').exists();
    assert.dom('[data-test-compose-files] [data-test-error]').exists();
    fakes.api
      .on('GET', `${APP}/compose`, makeComposeView())
      .on('GET', `${APP}/containers`, makeContainerView());
    await click('[data-test-compose-reload]');
    assert.dom('[data-test-container="web"]').exists();
    assert.dom('[data-test-compose-override]').exists();
  });

  test('a comparison that failed leaves the containers without markers', async function (assert) {
    fakes.api.on(
      'GET',
      `${APP}/containers`,
      makeContainerView([makeContainer({ current: null, expected: null })], {
        compared: false,
        comparison_error: 'docker compose config --hash: exit status 1',
      }),
    );
    await visit('/apps/blog/resources?section=compose');

    assert
      .dom('[data-test-compose-comparison-error]')
      .includesText('稼働中のリリースと比べられませんでした');
    assert.dom('[data-test-container-current]').doesNotExist();
  });
});
