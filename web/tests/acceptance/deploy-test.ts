import { module, test } from 'qunit';
import {
  click,
  fillIn,
  find,
  settled,
  triggerEvent,
  visit,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  SHA_A,
  SHA_B,
  installFakes,
  makeApp,
  makeDeployment,
  type FakeStream,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App, Deployment } from 'koroku-console/utils/types';

/** A step headline `seconds` after 03:00:00, as koroku writes it. */
function head(seconds: number, key: string, title: string): string {
  const time = new Date(Date.UTC(2026, 10, 18, 3, 0, seconds))
    .toISOString()
    .replace('.000', '');
  return `==> ${time} ${key} ${title}`;
}

const UNTIL_CHECK = [
  `Queued deployment of ${SHA_A}.`,
  head(1, 'fetch', `Fetching ${SHA_A}`),
  'Checked out 9fceb02 Fix login',
  head(3, 'prepare', 'Preparing environment and Compose override'),
  'Compose file: compose.yaml',
  head(4, 'validate', 'Validating the Compose project'),
  'Services: [db web]',
  head(6, 'build', 'Building images'),
  '#1 [web 1/6] FROM ruby:3.4-slim',
  '#9 exporting to image',
  head(50, 'start', 'Starting containers'),
  ' Container koroku-blog-web-1  Started',
  head(59, 'check', 'Checking http://127.0.0.1:20000/'),
];

const SUCCEEDED_LOG = [
  ...UNTIL_CHECK,
  'The app is answering.',
  head(69, 'done', 'Deployment succeeded'),
  head(69, 'cleanup', 'Pruning dangling images'),
  'Total reclaimed space: 0B',
];

const ROLLED_BACK_LOG = [
  ...UNTIL_CHECK,
  'GET / returned 200 OK',
  'GET / returned 500 Internal Server Error',
  '',
  head(63, 'failed', 'Deployment failed: web did not become healthy'),
  head(63, 'rollback', 'Rolling back to deployment #42'),
  ' Container koroku-blog-web-1  Started',
  'Rolled back: deployment #42 is running again.',
];

function emitAll(stream: FakeStream, lines: string[]): void {
  for (const line of lines) stream.emit(line);
}

function stepStates(): string[] {
  return [...document.querySelectorAll('[data-test-step]')].map(
    (step) =>
      `${step.getAttribute('data-test-step')}:${step.getAttribute('data-state')}`,
  );
}

function isAtEnd(element: Element): boolean {
  return element.scrollHeight - element.scrollTop - element.clientHeight <= 2;
}

module('Acceptance | deploy', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let deployments: Deployment[];

  /** Serves one app whose history is the mutable `deployments` array. */
  function serveApp(overrides: Partial<App> = {}): void {
    const app = makeApp(overrides);
    fakes.api
      .on('GET', `/api/apps/${app.slug}`, () => ({
        ...app,
        latest_deployment: deployments[0] ?? null,
      }))
      .on('GET', `/api/apps/${app.slug}/deployments`, () => ({ deployments }))
      .on('POST', `/api/apps/${app.slug}/deployments`, () => {
        const created = makeDeployment({
          id: 43,
          app_slug: app.slug,
          status: 'queued',
          started_at: null,
          finished_at: null,
        });
        deployments = [created, ...deployments];
        return created;
      });
  }

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    deployments = [makeDeployment({ id: 42 })];
    fakes.api
      .on('GET', '/api/repos/octo/blog/branches', {
        branches: [
          { name: 'main', sha: SHA_A },
          { name: 'feature/search', sha: SHA_B },
        ],
      })
      .on('GET', '/api/repos/octo/blog/commits', {
        commits: [
          {
            sha: SHA_A,
            message: 'Fix login',
            author: 'octo',
            date: '2026-11-18T03:00:00Z',
          },
          {
            sha: SHA_B,
            message: 'Add search',
            author: 'octo',
            date: '2026-11-17T03:00:00Z',
          },
        ],
      });
  });

  module('deploying', function () {
    test('a staging app deploys the branch head without a dialog', async function (assert) {
      serveApp({ slug: 'blog-staging' });

      await visit('/apps/blog-staging/deploy');
      assert.dom('[data-test-deploy-branch]').hasValue('main');
      assert
        .dom('[data-test-deploy-target]')
        .includesText(SHA_A.slice(0, 7))
        .includesText('Fix login', 'the form says what the head is');
      assert.dom('[data-test-production-hint]').doesNotExist();

      await click('[data-test-deploy-submit]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog-staging/deployments')[0]
          ?.body,
        { ref: 'main' },
      );
      assert.dom('[data-test-deployment="43"]').exists();
    });

    test('a production app deploys only after confirming the dialog', async function (assert) {
      serveApp({ environment: 'production' });

      await visit('/apps/blog/deploy');
      assert.dom('[data-test-production-hint]').exists();
      await fillIn('[data-test-deploy-branch]', 'feature/search');
      assert.strictEqual(
        fakes.api.callsTo('GET', '/api/repos/octo/blog/commits').pop()?.query[
          'ref'
        ],
        'feature/search',
        'commits are listed for the chosen branch',
      );
      await fillIn('[data-test-deploy-commit]', SHA_B);
      assert.dom('[data-test-deploy-target]').includesText('Add search');
      await click('[data-test-deploy-submit]');

      assert.dom('[data-test-dialog]').exists();
      assert.dom('[data-test-dialog]').includesText('本番アプリ');
      assert
        .dom('[data-test-dialog-target]')
        .includesText('feature/search')
        .includesText(SHA_B.slice(0, 7));
      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments'),
        [],
        'nothing is deployed while the dialog is open',
      );

      await click('[data-test-dialog-cancel]');
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments'),
        [],
      );

      await click('[data-test-deploy-submit]');
      await click('[data-test-dialog-confirm]');

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments')[0]?.body,
        { ref: 'feature/search', sha: SHA_B },
      );
    });

    test('a history row redeploys its commit (rollback)', async function (assert) {
      deployments = [
        makeDeployment({
          id: 44,
          commit_sha: SHA_B,
          commit_message: 'Add search',
        }),
        makeDeployment({ id: 42 }),
      ];
      serveApp({ environment: 'production' });

      await visit('/apps/blog/deploy');
      assert
        .dom('[data-test-deployment="42"] [data-test-redeploy]')
        .hasText('このコミットを再デプロイ');
      await click('[data-test-deployment="42"] [data-test-redeploy]');

      assert
        .dom('[data-test-dialog-target]')
        .includesText(SHA_A.slice(0, 7), 'production still asks first');
      assert
        .dom('[data-test-log]')
        .doesNotExist('the button does not open the row');

      await click('[data-test-dialog-confirm]');

      assert.deepEqual(
        fakes.api.callsTo('POST', '/api/apps/blog/deployments')[0]?.body,
        { ref: 'main', sha: SHA_A },
      );
    });

    test('a refused deployment explains why', async function (assert) {
      serveApp();
      fakes.api.fail('POST', '/api/apps/blog/deployments', 507, 'disk_full', {
        used_percent: 91.2,
        limit_percent: 85,
      });

      await visit('/apps/blog/deploy');
      await click('[data-test-deploy-submit]');

      assert
        .dom('[data-test-error]')
        .includesText('ディスク使用率が上限を超えている')
        .includesText('91.2%')
        .includesText('85%');
    });

    test('without branch lists a ref or SHA is typed by hand', async function (assert) {
      serveApp();
      fakes.api
        .fail('GET', '/api/repos/octo/blog/branches', 503, 'not_configured')
        .fail('GET', '/api/repos/octo/blog/commits', 503, 'not_configured');

      await visit('/apps/blog/deploy');

      assert.dom('[data-test-deploy-branch]').doesNotExist();
      assert.dom('[data-test-deploy-ref]').hasValue('main');

      await click('[data-test-deploy-submit]');
      await fillIn('[data-test-deploy-ref]', SHA_B);
      await click('[data-test-deploy-submit]');

      const bodies = fakes.api
        .callsTo('POST', '/api/apps/blog/deployments')
        .map((call) => call.body);
      assert.deepEqual(bodies, [{ ref: 'main' }, { sha: SHA_B }]);
    });

    test('a queued or running deployment can be canceled', async function (assert) {
      deployments = [
        makeDeployment({ id: 50, status: 'running', finished_at: null }),
      ];
      serveApp();
      fakes.api.on('POST', '/api/deployments/50/cancel', () => {
        deployments = [makeDeployment({ id: 50, status: 'canceled' })];
        return deployments[0];
      });

      await visit('/apps/blog/deploy');
      await click('[data-test-deployment="50"] [data-test-cancel]');

      assert.strictEqual(
        fakes.api.callsTo('POST', '/api/deployments/50/cancel').length,
        1,
      );
      assert
        .dom('[data-test-deployment="50"] [data-test-deployment-status]')
        .hasText('キャンセル');
      assert.dom('[data-test-cancel]').doesNotExist();
    });
  });

  module('history', function () {
    test('rows show state, commit, duration and start; the serving release is Live', async function (assert) {
      const current = makeDeployment({ id: 42 });
      deployments = [
        makeDeployment({
          id: 44,
          status: 'canceled',
          commit_sha: SHA_B,
          commit_message: 'Add search\n\nWith a body.',
          error: 'canceled by user',
          started_at: '2026-11-18T04:00:00Z',
          finished_at: '2026-11-18T04:00:20Z',
        }),
        current,
      ];
      serveApp({ current_deployment: current });

      await visit('/apps/blog/deploy');

      assert.dom('[data-test-deployment]').exists({ count: 2 });
      assert
        .dom('[data-test-target-running]')
        .exists('the branch head is what is already running');
      assert.strictEqual(
        find('[data-test-deployment]')?.getAttribute('data-test-deployment'),
        '44',
        'newest first',
      );

      const row = '[data-test-deployment="42"]';
      assert
        .dom(`${row} [data-test-deployment-status]`)
        .hasText('成功')
        .hasAttribute('data-status', 'running');
      assert
        .dom(`${row} [data-test-deployment-status] svg`)
        .exists('an icon, not colour alone');
      assert
        .dom(row)
        .includesText(SHA_A.slice(0, 7))
        .includesText('Fix login')
        .includesText('main');
      assert.dom(`${row} [data-test-duration]`).hasText('1 分 9 秒');
      assert
        .dom(`${row} [data-test-started]`)
        .hasAttribute('datetime', '2026-11-18T03:00:01Z')
        .hasAttribute('title', '2026/11/18 12:00 JST');
      assert.dom(`${row} [data-test-live]`).hasText('Live');

      const canceled = '[data-test-deployment="44"]';
      assert.dom(`${canceled} [data-test-live]`).doesNotExist();
      assert
        .dom(`${canceled} [data-test-deployment-status]`)
        .hasText('キャンセル');
      assert
        .dom(`${canceled} [data-test-show-log]`)
        .hasText('Add search', 'only the first line of the message');
      assert
        .dom(`${canceled} [data-test-row-error]`)
        .hasText('canceled by user');
      assert.dom(`${canceled} [data-test-duration]`).hasText('20 秒');

      assert
        .dom('[data-test-log]')
        .doesNotExist('nothing is opened when all is well');
      assert.strictEqual(fakes.eventSource.streams.length, 0);
    });

    test('a stopped app has no Live release', async function (assert) {
      const current = makeDeployment({ id: 42 });
      serveApp({ current_deployment: current, desired_state: 'stopped' });

      await visit('/apps/blog/deploy');

      assert.dom('[data-test-deployment="42"]').exists();
      assert.dom('[data-test-live]').doesNotExist();
    });

    test('an app that was never deployed offers exactly one action', async function (assert) {
      deployments = [];
      serveApp();

      await visit('/apps/blog/deploy');

      assert
        .dom('[data-test-no-deployments]')
        .includesText('まだデプロイしていません')
        .includesText('blog.dev.example.com');
      assert.dom('#history-title').doesNotExist('no empty history list');
      assert
        .dom('[data-test-deploy-panel] button')
        .exists({ count: 1 }, 'the form holds the one next step')
        .hasText('デプロイ');

      await click('[data-test-deploy-submit]');

      assert.dom('[data-test-no-deployments]').doesNotExist();
      assert.dom('[data-test-deployment="43"]').exists();
    });

    test('the history shows a skeleton while loading', async function (assert) {
      serveApp();
      let release: (value: unknown) => void = () => {};
      fakes.api.on(
        'GET',
        '/api/apps/blog/deployments',
        () => new Promise((resolve) => (release = resolve)),
      );

      // `visit` would wait for the request that is held open.
      void visit('/apps/blog/deploy');
      await new Promise<void>((resolve) => {
        const check = () =>
          find('[data-test-loading]') ? resolve() : setTimeout(check, 10);
        check();
      });

      assert.dom('[data-test-loading]').hasAttribute('role', 'status');
      assert.dom('[data-test-deploy-submit]').exists('deploying does not wait');

      release({ deployments });
      await settled();

      assert.dom('[data-test-loading]').doesNotExist();
      assert.dom('[data-test-deployment="42"]').exists();
    });

    test('a failed history can be retried', async function (assert) {
      serveApp();
      fakes.api.fail('GET', '/api/apps/blog/deployments', 500, 'internal');

      await visit('/apps/blog/deploy');

      assert
        .dom('[data-test-error]')
        .hasAttribute('role', 'alert')
        .includesText('デプロイの履歴を取得できませんでした');
      assert
        .dom('[data-test-no-deployments]')
        .doesNotExist('an error is not "nothing deployed yet"');

      fakes.api.on('GET', '/api/apps/blog/deployments', () => ({
        deployments,
      }));
      await click('[data-test-error] button');

      assert.dom('[data-test-deployment="42"]').exists();
    });

    test('a deployment started elsewhere shows up without a reload', async function (assert) {
      serveApp();
      await visit('/apps/blog/deploy');

      deployments = [
        makeDeployment({ id: 51, status: 'running', finished_at: null }),
        ...deployments,
      ];
      // The header notices first; the history follows on the next tick.
      await fakes.poller.tick();
      await settled();
      await fakes.poller.tick();
      await settled();

      assert.dom('[data-test-app-state]').hasText('デプロイ中');
      assert
        .dom('[data-test-deployment="51"] [data-test-deployment-status]')
        .hasText('実行中');
    });
  });

  module('deployment detail', function () {
    test('a row opens into the steps, each with its duration and a folded log', async function (assert) {
      serveApp();

      await visit('/apps/blog/deploy');
      assert.dom('[data-test-show-log]').hasAttribute('aria-expanded', 'false');

      await click('[data-test-deployment="42"] [data-test-show-log]');

      const stream = fakes.eventSource.last;
      assert.strictEqual(stream.url, '/api/deployments/42/logs');
      assert.dom('[data-test-show-log]').hasAttribute('aria-expanded', 'true');

      emitAll(stream, SUCCEEDED_LOG);
      stream.end('succeeded');
      await settled();

      assert.true(stream.closed);
      assert.deepEqual(stepStates(), [
        'queue:done',
        'fetch:done',
        'prepare:done',
        'validate:done',
        'build:done',
        'start:done',
        'check:done',
        'done:done',
        'cleanup:done',
      ]);
      assert
        .dom('[data-test-step="build"]')
        .includesText('ビルド')
        .includesText('Building images', "koroku's own title");
      assert
        .dom('[data-test-step="build"] [data-test-step-duration]')
        .hasText('44 秒');
      assert
        .dom('[data-test-step="check"] [data-test-step-duration]')
        .hasText('10 秒');
      assert
        .dom('[data-test-step="done"] [data-test-step-duration]')
        .doesNotExist();
      assert
        .dom('[data-test-step-log]')
        .doesNotExist('a deployment that went well stays folded');
      assert.dom('[data-test-rollback-notice]').doesNotExist();
      assert
        .dom('[data-test-log-text-link]')
        .hasAttribute('href', '/api/deployments/42/logs?format=text');

      const toggle = '[data-test-step="build"] [data-test-step-toggle]';
      assert.dom(toggle).hasAttribute('aria-expanded', 'false');
      await click(toggle);

      assert.dom(toggle).hasAttribute('aria-expanded', 'true');
      assert
        .dom('[data-test-step="build"] [data-test-step-log]')
        .hasText('#1 [web 1/6] FROM ruby:3.4-slim #9 exporting to image')
        .hasAttribute('tabindex', '0', 'scrollable with the keyboard');
      assert.strictEqual(
        find(toggle)?.getAttribute('aria-controls'),
        find('[data-test-step="build"] .step-log')?.id,
      );
      assert.dom('[data-test-error-line]').doesNotExist();

      await click(toggle);
      assert.dom('[data-test-step-log]').doesNotExist();

      await click('[data-test-deployment="42"] [data-test-show-log]');
      assert.dom('[data-test-log]').doesNotExist('the row closes again');
    });

    test('live log lines appear and the stream is closed on end', async function (assert) {
      serveApp();

      await visit('/apps/blog/deploy');
      assert.strictEqual(
        fakes.eventSource.streams.length,
        0,
        'no stream while nothing is selected or running',
      );

      await click('[data-test-deploy-submit]');

      const stream = fakes.eventSource.last;
      const row = '[data-test-deployment="43"]';
      assert.strictEqual(stream.url, '/api/deployments/43/logs');
      assert.dom(`${row} [data-test-deployment-status]`).hasText('待機中');
      assert.dom('[data-test-app-state]').hasText('デプロイ中');
      assert.dom('[data-test-stream-state]').hasText('ログを受信中…');
      assert.deepEqual(
        [...new Set(stepStates().map((state) => state.split(':')[1]))],
        ['pending'],
        'before the first line, every step is still to come',
      );

      emitAll(stream, UNTIL_CHECK.slice(0, 9));
      await settled();

      assert.deepEqual(stepStates(), [
        'queue:done',
        'fetch:done',
        'prepare:done',
        'validate:done',
        'build:running',
        'start:pending',
        'check:pending',
        'done:pending',
      ]);
      assert
        .dom('[data-test-step="build"] [data-test-step-meta]')
        .hasText('実行中');
      assert
        .dom('[data-test-step="start"] [data-test-step-meta]')
        .hasText('未実行');
      assert
        .dom('[data-test-step-log]')
        .exists({ count: 1 }, 'only the running step is open')
        .includesText('#1 [web 1/6] FROM ruby:3.4-slim');
      assert.false(stream.closed);

      emitAll(stream, UNTIL_CHECK.slice(9, 12));
      await settled();

      assert
        .dom('[data-test-step="build"] [data-test-step-log]')
        .doesNotExist('the finished step folds away');
      assert
        .dom('[data-test-step="start"] [data-test-step-log]')
        .includesText('Container koroku-blog-web-1 Started');

      deployments = [
        makeDeployment({ id: 43, status: 'succeeded' }),
        ...deployments.slice(1),
      ];
      emitAll(stream, SUCCEEDED_LOG.slice(12));
      stream.end('succeeded');
      await settled();

      assert.true(stream.closed, 'close() is called so EventSource stops');
      assert
        .dom('[data-test-stream-state]')
        .hasAttribute('data-state', 'ended')
        .hasText('ログの終わりです。');
      assert
        .dom(`${row} [data-test-deployment-status]`)
        .hasText('成功', 'the status is refreshed when the log ends');
      assert.dom(`${row} [data-test-cancel]`).doesNotExist();
      assert.deepEqual(
        stepStates().filter((state) => !state.endsWith(':done')),
        [],
      );
      assert.dom('[data-test-step-log]').doesNotExist();
    });

    test('a deployment in progress is followed and polled', async function (assert) {
      deployments = [
        makeDeployment({ id: 50, status: 'running', finished_at: null }),
        makeDeployment({ id: 42 }),
      ];
      serveApp();

      await visit('/apps/blog/deploy');

      assert.strictEqual(
        fakes.eventSource.last.url,
        '/api/deployments/50/logs',
        'the running deployment is selected on arrival',
      );
      assert
        .dom('[data-test-deployment="50"] [data-test-deployment-status]')
        .hasText('実行中');

      deployments = [
        makeDeployment({
          id: 50,
          status: 'failed',
          error: 'web did not become healthy',
        }),
        makeDeployment({ id: 42 }),
      ];
      await fakes.poller.tick();

      assert
        .dom('[data-test-deployment="50"] [data-test-deployment-status]')
        .hasText('失敗');
      assert
        .dom('[data-test-deployment-error]')
        .hasText('web did not become healthy');
      assert.dom('[data-test-app-state]').hasText('失敗');
    });

    test('a link with ?deployment= opens that deployment', async function (assert) {
      deployments = [
        makeDeployment({
          id: 45,
          status: 'failed',
          error: 'web did not become healthy',
          finished_at: '2026-11-18T03:01:05Z',
        }),
        makeDeployment({ id: 42 }),
      ];
      serveApp();
      const scrolled: (string | null)[] = [];
      // eslint-disable-next-line @typescript-eslint/unbound-method
      const original = Element.prototype.scrollIntoView;
      Element.prototype.scrollIntoView = function (this: Element) {
        scrolled.push(this.getAttribute('data-test-deployment'));
      };

      try {
        await visit('/apps/blog/deploy?deployment=42');
      } finally {
        Element.prototype.scrollIntoView = original;
      }

      assert.deepEqual(
        scrolled,
        ['42'],
        'the linked deployment is scrolled to',
      );
      assert.strictEqual(
        fakes.eventSource.last.url,
        '/api/deployments/42/logs',
      );
      assert
        .dom('[data-test-deployment="42"] [data-test-show-log]')
        .hasAttribute('aria-expanded', 'true');
      assert
        .dom('[data-test-deployment="45"] [data-test-show-log]')
        .hasAttribute('aria-expanded', 'false');
    });

    test('a link to a deployment that is not listed opens as usual', async function (assert) {
      deployments = [
        makeDeployment({
          id: 45,
          status: 'failed',
          finished_at: '2026-11-18T03:01:05Z',
        }),
      ];
      serveApp();

      await visit('/apps/blog/deploy?deployment=7');

      assert.strictEqual(
        fakes.eventSource.last.url,
        '/api/deployments/45/logs',
      );
    });

    test('a failed deployment opens by itself, at the step that failed', async function (assert) {
      const current = makeDeployment({ id: 42 });
      deployments = [
        makeDeployment({
          id: 45,
          status: 'failed',
          error: 'web did not become healthy',
          finished_at: '2026-11-18T03:01:05Z',
        }),
        current,
      ];
      serveApp({ current_deployment: current });

      await visit('/apps/blog/deploy');

      const stream = fakes.eventSource.last;
      assert.strictEqual(
        stream.url,
        '/api/deployments/45/logs',
        'the latest deployment failed: that is what the visit is about',
      );
      emitAll(stream, ROLLED_BACK_LOG);
      stream.end('failed');
      await settled();

      assert
        .dom('[data-test-log] [role="alert"]')
        .includesText('デプロイに失敗しました')
        .includesText('「確認」の手順で失敗しました。')
        .includesText('web did not become healthy');

      assert.deepEqual(stepStates().slice(-2), [
        'check:failed',
        'rollback:done',
      ]);
      assert
        .dom('[data-test-step="check"] [data-test-step-meta]')
        .includesText('失敗')
        .includesText('4 秒');
      assert
        .dom('[data-test-step="check"] [data-test-step-toggle]')
        .hasAttribute('aria-expanded', 'true');
      assert
        .dom('[data-test-step-log]')
        .exists({ count: 1 }, 'only the failed step is open');
      assert
        .dom('[data-test-step="check"] [data-test-error-line]')
        .exists({ count: 1 })
        .hasText('GET / returned 500 Internal Server Error')
        .hasTagName('mark');
      assert
        .dom('[data-test-step="check"] [data-test-step-log]')
        .includesText('GET / returned 200 OK', 'the other lines stay');

      assert
        .dom('[data-test-rollback-notice]')
        .hasAttribute('data-rollback', 'restored')
        .includesText('旧版に自動で戻しました')
        .includesText('デプロイ #42');
      assert
        .dom('[data-test-deployment="42"] [data-test-live]')
        .exists('the previous release is the one that serves');
    });

    test('a failure before the containers were replaced says the release is untouched', async function (assert) {
      deployments = [
        makeDeployment({
          id: 45,
          status: 'failed',
          error: 'build: exit status 1',
        }),
        makeDeployment({ id: 42 }),
      ];
      serveApp();

      await visit('/apps/blog/deploy');
      emitAll(fakes.eventSource.last, [
        ...UNTIL_CHECK.slice(0, 9),
        'ERROR: process "/bin/sh -c bundle install" did not complete',
        '',
        head(40, 'failed', 'Deployment failed: build: exit status 1'),
      ]);
      fakes.eventSource.last.end('failed');
      await settled();

      assert
        .dom('[data-test-log] [role="alert"]')
        .includesText('「ビルド」の手順で失敗しました。');
      assert
        .dom('[data-test-rollback-notice]')
        .hasAttribute('data-rollback', 'untouched')
        .includesText('稼働中のリリースには影響していません');
      assert.dom('[data-test-step="rollback"]').doesNotExist();
      assert
        .dom('[data-test-step="build"] [data-test-error-line]')
        .includesText('ERROR: process');
    });

    test('a rollback that did not work is an error of its own', async function (assert) {
      deployments = [
        makeDeployment({
          id: 45,
          status: 'failed',
          error: 'start containers: exit status 1',
        }),
        makeDeployment({ id: 42 }),
      ];
      serveApp();

      await visit('/apps/blog/deploy');
      emitAll(fakes.eventSource.last, [
        ...UNTIL_CHECK.slice(0, 12),
        '',
        head(
          58,
          'failed',
          'Deployment failed: start containers: exit status 1',
        ),
        head(58, 'rollback', 'Rolling back to deployment #42'),
        'Rollback failed: exit status 1',
      ]);
      fakes.eventSource.last.end('failed');
      await settled();

      assert
        .dom('[data-test-rollback-notice]')
        .hasAttribute('data-rollback', 'failed')
        .hasAttribute('role', 'alert')
        .includesText('旧版に戻せませんでした')
        .includesText('Rollback failed: exit status 1');
      assert.deepEqual(stepStates().slice(-2), [
        'start:failed',
        'rollback:failed',
      ]);
      assert
        .dom('[data-test-step-log]')
        .exists({ count: 2 }, 'both failed steps are open');
    });

    test('the live log follows its end until the reader scrolls up', async function (assert) {
      deployments = [
        makeDeployment({ id: 50, status: 'running', finished_at: null }),
        makeDeployment({ id: 42 }),
      ];
      serveApp();

      await visit('/apps/blog/deploy');
      const stream = fakes.eventSource.last;
      const many = (from: number) =>
        Array.from({ length: 200 }, (_, n) => `#7 line ${from + n}`);
      emitAll(stream, [...UNTIL_CHECK.slice(0, 8), ...many(0)]);
      await settled();

      const log = find('[data-test-step="build"] [data-test-step-log]');
      if (!log) throw new Error('the running step is not open');
      assert.true(log.scrollHeight > log.clientHeight, 'the log scrolls');
      assert.true(isAtEnd(log), 'it starts at its end');
      assert.dom('[data-test-follow]').doesNotExist();

      emitAll(stream, many(200));
      await settled();
      assert.true(isAtEnd(log), 'and stays there as lines arrive');

      log.scrollTop = 0;
      await triggerEvent(log, 'scroll');

      assert.dom('[data-test-follow]').hasText('末尾を追う');
      assert.dom(log).hasAttribute('data-following', 'false');

      emitAll(stream, many(400));
      await settled();
      assert.strictEqual(log.scrollTop, 0, 'new lines no longer move it');
      assert.dom(log).includesText('#7 line 599');

      // The step ends while the reader is still up there: it stays open.
      emitAll(stream, UNTIL_CHECK.slice(10, 12));
      await settled();
      assert.dom('[data-test-step="build"] [data-test-step-log]').exists();
      assert
        .dom('[data-test-follow]')
        .doesNotExist('nothing left to follow in a finished step');
    });

    test('the button brings a live log back to following', async function (assert) {
      deployments = [
        makeDeployment({ id: 50, status: 'running', finished_at: null }),
      ];
      serveApp();

      await visit('/apps/blog/deploy');
      const stream = fakes.eventSource.last;
      emitAll(stream, [
        ...UNTIL_CHECK.slice(0, 8),
        ...Array.from({ length: 300 }, (_, n) => `#7 line ${n}`),
      ]);
      await settled();

      const log = find('[data-test-step="build"] [data-test-step-log]');
      if (!log) throw new Error('the running step is not open');
      log.scrollTop = 0;
      await triggerEvent(log, 'scroll');
      await click('[data-test-follow]');

      assert.true(isAtEnd(log), 'back at the end');
      assert.dom('[data-test-follow]').doesNotExist();

      stream.emit('#7 the newest line');
      await settled();
      assert.true(isAtEnd(log), 'and following again');
      assert.dom(log).includesText('#7 the newest line');
    });

    test('a log that cannot be reached offers to reconnect', async function (assert) {
      serveApp();

      await visit('/apps/blog/deploy');
      await click('[data-test-deployment="42"] [data-test-show-log]');
      const first = fakes.eventSource.last;
      first.error(false);
      await settled();
      assert
        .dom('[data-test-stream-error]')
        .doesNotExist('the browser is still retrying');

      first.error(true);
      await settled();

      assert.true(first.closed);
      assert
        .dom('[data-test-stream-error]')
        .includesText('ログに接続できませんでした。');

      await click('[data-test-reconnect]');

      assert.strictEqual(fakes.eventSource.streams.length, 2);
      assert.dom('[data-test-stream-error]').doesNotExist();

      fakes.eventSource.last.end('succeeded');
      await settled();
      assert
        .dom('[data-test-no-log]')
        .exists('a log that was cleaned up is said to be gone');
    });
  });
});
