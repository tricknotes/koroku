import { module, test } from 'qunit';
import {
  click,
  currentURL,
  fillIn,
  find,
  findAll,
  settled,
  triggerEvent,
  triggerKeyEvent,
  visit,
  waitFor,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  installFakes,
  makeApp,
  makeDeployment,
  type FakeCall,
  type FakeStream,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';
import type { App, LogLine } from 'paas-console/utils/types';

const LOGS = '/api/apps/blog/logs';
const HISTORY = '/api/apps/blog/logs/history';
const ARCHIVES = '/api/apps/blog/logs/archives';
const ROW = '[data-test-log-row]';
const VIEWPORT = '[data-test-log-viewport]';

function logLine(line: string, overrides: Partial<LogLine> = {}): LogLine {
  return {
    service: 'web',
    time: '2026-11-18T03:00:00.123Z',
    line,
    ...overrides,
  };
}

function emit(stream: FakeStream, ...lines: LogLine[]): void {
  for (const line of lines) stream.emit(JSON.stringify(line));
}

function rowTexts(): string[] {
  return findAll(`${ROW} .log-text`).map((node) => node.textContent ?? '');
}

function viewport(): HTMLElement {
  return find(VIEWPORT) as HTMLElement;
}

/** How far the list is from showing its last row. */
function distanceFromEnd(): number {
  const element = viewport();
  return element.scrollHeight - element.scrollTop - element.clientHeight;
}

module('Acceptance | logs', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const deployment = makeDeployment();
    app = makeApp({
      current_deployment: deployment,
      latest_deployment: deployment,
    });
    fakes.api
      .on('GET', '/api/apps/blog', () => app)
      .on('GET', '/api/apps/blog/services', {
        services: [
          { name: 'web', state: 'running', status: '', health: '' },
          { name: 'db', state: 'running', status: '', health: '' },
        ],
      })
      .on('GET', HISTORY, { lines: [], truncated: false })
      .on('GET', ARCHIVES, {
        archives: [
          {
            key: 'logs/blog/web/2026/11/18/04.log.gz',
            service: 'web',
            hour: '2026-11-18T04:00:00+09:00',
            size: 12345,
          },
        ],
      });
  });

  function historyCalls(): FakeCall[] {
    return fakes.api.callsTo('GET', HISTORY);
  }

  module('live tail', function () {
    test('it shows the newest lines in JST and follows the service switch', async function (assert) {
      await visit('/apps/blog/logs');

      const first = fakes.eventSource.last;
      assert.strictEqual(first.url, `${LOGS}?tail=1000`);
      assert.dom('[data-test-log-live]').hasAria('checked', 'true');

      emit(first, logLine('Listening on 0.0.0.0:3000'));
      first.emit('not json');
      await settled();

      assert.dom(ROW).exists({ count: 2 });
      assert.dom(`${ROW} .log-time`).hasText('11/18 12:00:00');
      assert.dom(`${ROW} .log-service`).hasText('web');
      assert.deepEqual(rowTexts(), ['Listening on 0.0.0.0:3000', 'not json']);
      assert.dom('[data-test-log-state]').hasText('ライブ受信中');
      assert.dom('[data-test-log-count]').hasText('2 行');

      assert.deepEqual(
        findAll('[data-test-log-service] option').map((option) =>
          option.textContent.trim(),
        ),
        ['すべてのサービス', 'web', 'db', 'アクセスログ（_access）'],
        'the access log is offered as a service of its own',
      );

      await fillIn('[data-test-log-service]', 'db');

      assert.true(first.closed, 'the previous stream is closed');
      assert.strictEqual(
        fakes.eventSource.last.url,
        `${LOGS}?service=db&tail=1000`,
      );
      assert.dom(ROW).doesNotExist('the list starts over');
      assert.strictEqual(currentURL(), '/apps/blog/logs?service=db');

      emit(fakes.eventSource.last, logLine('ready', { service: 'db' }));
      await settled();
      assert
        .dom(`${ROW} .log-service`)
        .doesNotExist('one service needs no service column');
    });

    test('the live switch stops and restarts the stream', async function (assert) {
      await visit('/apps/blog/logs');
      emit(fakes.eventSource.last, logLine('boot'));
      await settled();

      await click('[data-test-log-live]');
      assert.true(fakes.eventSource.last.closed);
      assert.dom('[data-test-log-live]').hasAria('checked', 'false');
      assert.dom('[data-test-log-state]').hasText('ライブ停止中');
      assert.dom(ROW).exists({ count: 1 }, 'what was shown stays');

      await click('[data-test-log-live]');
      assert.strictEqual(fakes.eventSource.streams.length, 2);
      assert.false(fakes.eventSource.last.closed);
      assert.dom('[data-test-log-state]').hasText('接続中…');
    });

    test('while switched off, another service does not keep the old rows', async function (assert) {
      await visit('/apps/blog/logs');
      emit(fakes.eventSource.last, logLine('boot'));
      await settled();
      await click('[data-test-log-live]');

      await fillIn('[data-test-log-service]', 'db');

      assert.dom(ROW).doesNotExist();
      assert.strictEqual(fakes.eventSource.streams.length, 1, 'still off');
      assert
        .dom('[data-test-log-empty]')
        .includesText('ライブ tail は停止中です');

      await click('[data-test-empty-action]');
      assert.strictEqual(
        fakes.eventSource.last.url,
        `${LOGS}?service=db&tail=1000`,
      );
      assert.dom('[data-test-log-live]').hasAria('checked', 'true');
    });

    test('the stream is closed when leaving the tab', async function (assert) {
      await visit('/apps/blog/logs');
      const stream = fakes.eventSource.last;

      await click('[data-test-tab="env"]');

      assert.true(stream.closed);
    });

    test('a refused stream explains itself and can be retried', async function (assert) {
      await visit('/apps/blog/logs');
      const refused = fakes.eventSource.last;
      refused.error(true);
      await settled();

      assert
        .dom('[data-test-log-error]')
        .includesText('ライブログに接続できませんでした');
      assert.true(refused.closed);
      assert.dom('[data-test-log-empty]').doesNotExist();

      await click('[data-test-log-retry]');
      assert.strictEqual(fakes.eventSource.streams.length, 2);
      assert.dom('[data-test-log-error]').doesNotExist();
    });

    test('a reconnect does not duplicate the tail', async function (assert) {
      await visit('/apps/blog/logs');
      const stream = fakes.eventSource.last;

      stream.open();
      emit(stream, logLine('boot'));
      await settled();
      assert.deepEqual(rowTexts(), ['boot']);

      stream.error(false);
      await settled();
      assert.dom('[data-test-log-state]').includesText('再接続中');
      assert.false(stream.closed, 'the browser reconnects by itself');

      // The server replays the last lines on the new connection.
      stream.open();
      emit(stream, logLine('boot'));
      await settled();
      assert.deepEqual(rowTexts(), ['boot']);
    });

    test('it follows the end until the reader scrolls up, and resumes on request', async function (assert) {
      await visit('/apps/blog/logs');
      const stream = fakes.eventSource.last;
      emit(
        stream,
        ...Array.from({ length: 200 }, (_, n) => logLine(`line ${n}`)),
      );
      await settled();

      assert.dom(ROW).exists({ count: 200 });
      assert.true(distanceFromEnd() < 2, 'the newest line is in view');
      assert.dom('[data-test-log-resume]').doesNotExist();

      viewport().scrollTop = 0;
      await triggerEvent(VIEWPORT, 'scroll');

      assert.dom('[data-test-log-state]').hasText('追従を一時停止中');
      assert.dom('[data-test-log-resume]').exists();

      emit(stream, logLine('new 1'), logLine('new 2'), logLine('new 3'));
      await settled();

      assert.dom(ROW).exists({ count: 200 }, 'nothing moves under the reader');
      assert.strictEqual(viewport().scrollTop, 0);
      assert.dom('[data-test-log-waiting]').hasText('新着 3 行');

      await click('[data-test-log-resume]');

      assert.dom(ROW).exists({ count: 203 });
      assert.true(distanceFromEnd() < 2, 'back at the newest line');
      assert.dom('[data-test-log-resume]').doesNotExist();
      assert.dom('[data-test-log-state]').hasText('ライブ受信中');
    });

    test('scrolling back to the end resumes by itself', async function (assert) {
      await visit('/apps/blog/logs');
      const stream = fakes.eventSource.last;
      emit(
        stream,
        ...Array.from({ length: 200 }, (_, n) => logLine(`line ${n}`)),
      );
      await settled();

      viewport().scrollTop = 0;
      await triggerEvent(VIEWPORT, 'scroll');
      emit(stream, logLine('new'));
      await settled();
      assert.dom(ROW).exists({ count: 200 });

      viewport().scrollTop = viewport().scrollHeight;
      await triggerEvent(VIEWPORT, 'scroll');

      assert.dom(ROW).exists({ count: 201 });
      assert.dom('[data-test-log-resume]').doesNotExist();
    });

    test('the list is capped and says when old rows were dropped', async function (assert) {
      await visit('/apps/blog/logs');
      emit(
        fakes.eventSource.last,
        ...Array.from({ length: 2003 }, (_, n) => logLine(`line ${n}`)),
      );
      await settled();

      assert.dom(ROW).exists({ count: 2000 });
      assert.strictEqual(rowTexts()[0], 'line 3');
      assert.dom('[data-test-log-count]').includesText('2,000 行');
      assert
        .dom('[data-test-log-dropped]')
        .includesText('古い行は表示から外しました');
    });

    test('the keyword filters what is on screen and marks the matches', async function (assert) {
      await visit('/apps/blog/logs');
      emit(
        fakes.eventSource.last,
        logLine('GET /up 200'),
        logLine('checkpoint complete', { service: 'db' }),
        logLine('GET /posts 500'),
      );
      await settled();

      await fillIn('[data-test-log-keyword]', 'GET');

      assert.deepEqual(rowTexts(), ['GET /up 200', 'GET /posts 500']);
      assert.dom(`${ROW} .log-match`).exists({ count: 2 }).hasText('GET');
      assert.dom('[data-test-log-count]').hasText('2 / 3 行が一致');
      assert.strictEqual(historyCalls().length, 0, 'no request was needed');
      assert.strictEqual(currentURL(), '/apps/blog/logs?q=GET');

      emit(fakes.eventSource.last, logLine('GET /feed 200'), logLine('idle'));
      await settled();
      assert.dom(ROW).exists({ count: 3 }, 'new lines are filtered as well');

      await fillIn('[data-test-log-keyword]', 'get');
      assert
        .dom('[data-test-log-empty]')
        .includesText('「get」に一致する行はありません')
        .includesText('大文字と小文字は区別されます');

      // The one next step: look further back with the same keyword.
      await click('[data-test-empty-action]');
      assert.strictEqual(historyCalls()[0]?.query['q'], 'get');
      assert.strictEqual(currentURL(), '/apps/blog/logs?period=24h&q=get');
    });

    test('long lines wrap by default and the choice is remembered', async function (assert) {
      await visit('/apps/blog/logs');
      emit(fakes.eventSource.last, logLine('x'.repeat(500)));
      await settled();

      assert.dom('[data-test-log-stream]').hasClass('is-wrapped');
      assert.dom('[data-test-log-wrap]').hasAria('checked', 'true');

      await click('[data-test-log-wrap]');

      assert.dom('[data-test-log-stream]').doesNotHaveClass('is-wrapped');
      assert.dom('[data-test-log-wrap]').hasAria('checked', 'false');
      assert.strictEqual(fakes.storage.get('paasd.logs.wrap'), '0');

      await click('[data-test-log-wrap]');
      assert.dom('[data-test-log-stream]').hasClass('is-wrapped');
      assert.strictEqual(fakes.storage.get('paasd.logs.wrap'), '1');
    });

    test('a remembered "no wrap" is applied on arrival', async function (assert) {
      fakes.storage.set('paasd.logs.wrap', '0');

      await visit('/apps/blog/logs');
      emit(fakes.eventSource.last, logLine('boot'));
      await settled();

      assert.dom('[data-test-log-stream]').doesNotHaveClass('is-wrapped');
      assert.dom('[data-test-log-wrap]').hasAria('checked', 'false');
    });

    test('with nothing logged yet it says so and offers one next step', async function (assert) {
      await visit('/apps/blog/logs');
      assert.dom('[data-test-loading]').includesText('接続中');

      fakes.eventSource.last.open();
      await settled();

      assert.dom('[data-test-log-empty]').includesText('まだログがありません');
      assert.dom('[data-test-empty-action]').exists({ count: 1 });

      await click('[data-test-empty-action]');
      assert.strictEqual(currentURL(), '/apps/blog/logs?period=24h');
    });

    test('an app that was never deployed is pointed to the deploy tab', async function (assert) {
      app = makeApp();

      await visit('/apps/blog/logs');
      fakes.eventSource.last.open();
      await settled();

      assert
        .dom('[data-test-log-empty]')
        .includesText('アプリをデプロイすると');
      assert
        .dom('a[data-test-empty-action]')
        .hasClass('button')
        .hasAttribute('href', '/apps/blog/deploy');
    });
  });

  module('row detail', function () {
    const LINES = [
      logLine('first'),
      logLine('{"level":"info","msg":"handled request"}', {
        service: '_access',
        time: '2026-11-18T03:00:01.500Z',
      }),
      logLine('last', { time: '2026-11-18T03:00:02Z' }),
    ];

    test('clicking a row shows its full text and metadata', async function (assert) {
      await visit('/apps/blog/logs');
      emit(fakes.eventSource.last, ...LINES);
      await settled();
      assert.dom('[data-test-log-detail]').doesNotExist();

      await click(`${ROW}:nth-child(2)`);

      assert.dom(`${ROW}:nth-child(2)`).hasAria('selected', 'true');
      assert
        .dom('[data-test-log-detail-service]')
        .hasText('アクセスログ（_access）');
      assert
        .dom('[data-test-log-detail-time]')
        .hasText('2026/11/18 12:00:01.500 JST');
      assert
        .dom('[data-test-log-detail-text]')
        .hasText('{"level":"info","msg":"handled request"}');

      await click('[data-test-log-detail] [data-test-copy]');
      assert.deepEqual(fakes.browser.copied, [
        '{"level":"info","msg":"handled request"}',
      ]);

      // Reading a row keeps the list still, like scrolling up does.
      emit(fakes.eventSource.last, logLine('later'));
      await settled();
      assert.dom(ROW).exists({ count: 3 });
      assert.dom('[data-test-log-waiting]').hasText('新着 1 行');

      await click('[data-test-log-detail-close]');

      assert.dom('[data-test-log-detail]').doesNotExist();
      assert.dom(`${ROW}[aria-selected="true"]`).doesNotExist();
      assert.dom(VIEWPORT).isFocused('focus goes back to the list');
      assert.dom(ROW).exists({ count: 4 }, 'and the tail flows again');
    });

    test('clicking the selected row again closes the detail', async function (assert) {
      await visit('/apps/blog/logs');
      emit(fakes.eventSource.last, ...LINES);
      await settled();

      await click(`${ROW}:nth-child(1)`);
      assert.dom('[data-test-log-detail-text]').hasText('first');

      await click(`${ROW}:nth-child(1)`);
      assert.dom('[data-test-log-detail]').doesNotExist();
    });

    test('the arrow keys move the selection and Escape clears it', async function (assert) {
      await visit('/apps/blog/logs');
      emit(fakes.eventSource.last, ...LINES);
      await settled();

      await triggerKeyEvent(VIEWPORT, 'keydown', 'ArrowUp');
      assert.dom('[data-test-log-detail-text]').hasText('last');
      assert
        .dom(VIEWPORT)
        .hasAttribute(
          'aria-activedescendant',
          find(`${ROW}:nth-child(3)`)?.id ?? 'missing',
        );

      await triggerKeyEvent(VIEWPORT, 'keydown', 'ArrowUp');
      await triggerKeyEvent(VIEWPORT, 'keydown', 'ArrowUp');
      await triggerKeyEvent(VIEWPORT, 'keydown', 'ArrowUp');
      assert
        .dom('[data-test-log-detail-text]')
        .hasText('first', 'it stops at the first row');

      await triggerKeyEvent(VIEWPORT, 'keydown', 'ArrowDown');
      assert.dom(`${ROW}:nth-child(2)`).hasAria('selected', 'true');

      await triggerKeyEvent(VIEWPORT, 'keydown', 'Escape');
      assert.dom('[data-test-log-detail]').doesNotExist();
      assert.dom(VIEWPORT).doesNotHaveAttribute('aria-activedescendant');
    });
  });

  module('search', function () {
    const RESULT = {
      lines: [
        logLine('GET /up 200', { time: '2026-11-17T01:00:00Z' }),
        logLine('GET /posts 500', { time: '2026-11-17T01:00:05Z' }),
      ],
      truncated: false,
    };

    test('a period searches what the host keeps, with the filters as the query', async function (assert) {
      fakes.api.on('GET', HISTORY, RESULT);
      await visit('/apps/blog/logs');
      const stream = fakes.eventSource.last;

      const before = Date.now();
      await fillIn('[data-test-log-period]', '1h');
      const after = Date.now();

      assert.true(stream.closed, 'the live tail stops');
      assert.dom('[data-test-log-live]').hasAria('checked', 'false');

      const first = historyCalls()[0]?.query ?? {};
      assert.deepEqual(Object.keys(first).sort(), ['from', 'limit']);
      assert.strictEqual(first['limit'], '1000');
      const from = Date.parse(first['from'] ?? '');
      const hour = 60 * 60 * 1000;
      assert.true(from >= before - hour, 'from is one hour ago');
      assert.true(from <= after - hour, 'and not later');

      assert.deepEqual(rowTexts(), ['GET /up 200', 'GET /posts 500']);
      assert.dom(`${ROW} .log-time`).hasText('11/17 10:00:00');
      assert.dom('[data-test-log-state]').hasText('直近 1 時間');
      assert.dom('[data-test-log-count]').hasText('2 行');
      assert.dom('[data-test-log-truncated]').doesNotExist();
      assert.dom('[data-test-log-resume]').doesNotExist();

      await fillIn('[data-test-log-service]', 'web');
      assert.strictEqual(historyCalls()[1]?.query['service'], 'web');

      await fillIn('[data-test-log-keyword]', 'GET');
      const third = historyCalls()[2]?.query ?? {};
      assert.strictEqual(third['q'], 'GET');
      assert.strictEqual(third['service'], 'web');
      assert.strictEqual(historyCalls().length, 3);
      assert.strictEqual(
        currentURL(),
        '/apps/blog/logs?period=1h&q=GET&service=web',
      );
      assert.strictEqual(
        fakes.eventSource.streams.length,
        1,
        'no stream is opened for a search',
      );
    });

    test('the filters are restored from the URL', async function (assert) {
      fakes.api.on('GET', HISTORY, RESULT);

      await visit('/apps/blog/logs?period=7d&q=GET&service=web');

      assert.strictEqual(fakes.eventSource.streams.length, 0);
      assert.dom('[data-test-log-period]').hasValue('7d');
      assert.dom('[data-test-log-service]').hasValue('web');
      assert.dom('[data-test-log-keyword]').hasValue('GET');
      const query = historyCalls()[0]?.query ?? {};
      assert.strictEqual(query['q'], 'GET');
      assert.strictEqual(query['service'], 'web');
      assert.dom(ROW).exists({ count: 2 });
    });

    test('a custom range is sent as JST and checked', async function (assert) {
      await visit(
        '/apps/blog/logs?period=custom&from=2026-11-17T00:00&to=2026-11-17T06:00',
      );

      assert.dom('[data-test-log-from]').hasValue('2026-11-17T00:00');
      assert.deepEqual(historyCalls()[0]?.query, {
        from: '2026-11-16T15:00:00.000Z',
        to: '2026-11-16T21:00:00.000Z',
        limit: '1000',
      });

      await fillIn('[data-test-log-to]', '2026-11-16T23:00');

      assert
        .dom('[data-test-log-range-error]')
        .includesText('終了は開始より後');
      assert.strictEqual(historyCalls().length, 1, 'nothing was searched');

      await fillIn('[data-test-log-to]', '');
      assert.dom('[data-test-log-range-error]').doesNotExist();
      assert.deepEqual(historyCalls()[1]?.query, {
        from: '2026-11-16T15:00:00.000Z',
        limit: '1000',
      });
    });

    test('choosing a custom range starts from the last hour', async function (assert) {
      await visit('/apps/blog/logs');
      assert.dom('[data-test-log-range]').doesNotExist();

      await fillIn('[data-test-log-period]', 'custom');

      assert.dom('[data-test-log-range]').includesText('日本時間');
      const query = historyCalls()[0]?.query ?? {};
      const span =
        Date.parse(query['to'] ?? '') - Date.parse(query['from'] ?? '');
      assert.strictEqual(span, 60 * 60 * 1000);
    });

    test('a range before what the host keeps points to the archives', async function (assert) {
      await visit('/apps/blog/logs?period=custom&from=2020-01-01T00:00');

      assert.dom('[data-test-log-range-old]').includesText('14 日より前');

      await click('[data-test-log-range-old] button');
      assert.strictEqual(
        currentURL(),
        '/apps/blog/logs?from=2020-01-01T00%3A00&period=archive',
      );
      assert.dom('[data-test-log-archives]').exists();
    });

    test('a truncated result says so and loads the next page', async function (assert) {
      const last = '2026-11-17T01:00:05.123456789Z';
      fakes.api.on('GET', HISTORY, (call: FakeCall) =>
        call.query['from'] === last
          ? {
              // The next page starts at the last row, inclusive.
              lines: [
                logLine('b', { time: last }),
                logLine('c', { time: '2026-11-17T01:00:09Z' }),
              ],
              truncated: false,
            }
          : {
              lines: [
                logLine('a', { time: '2026-11-17T01:00:00Z' }),
                logLine('b', { time: last }),
              ],
              truncated: true,
            },
      );

      await visit('/apps/blog/logs?period=24h&q=x');

      assert
        .dom('[data-test-log-truncated]')
        .includesText('古い方から途中までを表示しています');
      assert.deepEqual(rowTexts(), ['a', 'b']);

      await click('[data-test-log-more]');

      const next = historyCalls()[1]?.query ?? {};
      assert.strictEqual(next['from'], last, 'it continues from the last row');
      assert.strictEqual(next['q'], 'x');
      assert.deepEqual(rowTexts(), ['a', 'b', 'c'], 'without repeating it');
      assert.dom('[data-test-log-truncated]').doesNotExist();
    });

    test('an empty result offers one way to widen the search', async function (assert) {
      await visit('/apps/blog/logs?period=1h&q=nothing');

      assert
        .dom('[data-test-log-empty]')
        .includesText('この条件に一致するログはありません');
      assert.dom('[data-test-empty-action]').hasText('キーワードを消す');

      await click('[data-test-empty-action]');
      assert.dom('[data-test-log-keyword]').hasValue('');
      assert.dom('[data-test-empty-action]').hasText('直近 7 日に広げる');

      await click('[data-test-empty-action]');
      assert.dom('[data-test-log-period]').hasValue('7d');
      assert.dom('[data-test-empty-action]').hasText('アーカイブを見る');

      await click('[data-test-empty-action]');
      assert.dom('[data-test-log-archives]').exists();
    });

    test('a failed search shows the error and can be retried', async function (assert) {
      fakes.api.fail('GET', HISTORY, 500, 'internal');

      await visit('/apps/blog/logs?period=1h');

      assert
        .dom('[data-test-log-error]')
        .includesText('ログを取得できませんでした')
        .includesText('サーバーでエラーが発生しました');
      assert.dom('[data-test-log-empty]').doesNotExist();

      fakes.api.on('GET', HISTORY, RESULT);
      await click('[data-test-log-retry]');

      assert.dom('[data-test-log-error]').doesNotExist();
      assert.dom(ROW).exists({ count: 2 });
    });

    test('it shows that a search is running', async function (assert) {
      let answer: (value: unknown) => void = () => undefined;
      fakes.api.on(
        'GET',
        HISTORY,
        () => new Promise((resolve) => (answer = resolve)),
      );

      const visiting = visit('/apps/blog/logs?period=1h');
      await waitFor('[data-test-loading]');
      assert.dom('[data-test-loading]').includesText('検索中');
      assert.dom('[data-test-log-empty]').doesNotExist();

      answer(RESULT);
      await visiting;
      assert.dom('[data-test-loading]').doesNotExist();
      assert.dom(ROW).exists({ count: 2 });
    });

    test('turning the live switch on goes back to the newest lines', async function (assert) {
      fakes.api.on('GET', HISTORY, RESULT);
      await visit('/apps/blog/logs?period=1h');

      await click('[data-test-log-live]');

      assert.dom('[data-test-log-period]').hasValue('tail');
      assert.strictEqual(fakes.eventSource.streams.length, 1);
      assert.strictEqual(currentURL(), '/apps/blog/logs');
    });
  });

  module('archives', function () {
    test('they are listed and downloaded through a presigned URL', async function (assert) {
      fakes.api.on('GET', `${ARCHIVES}/url`, {
        url: 'https://storage.example.org/logs/blog/web/04.log.gz?signature=abc',
      });

      await visit('/apps/blog/logs');
      await fillIn('[data-test-log-period]', 'archive');

      assert.true(fakes.eventSource.last.closed, 'the live tail stops');
      assert.dom('[data-test-log-keyword]').doesNotExist();
      assert.dom('[data-test-log-live]').doesNotExist();
      assert
        .dom('[data-test-archive="logs/blog/web/2026/11/18/04.log.gz"]')
        .includesText('2026/11/18 04:00 JST')
        .includesText('web')
        .includesText('12.1 KB');

      await click('[data-test-archive-download]');

      assert.deepEqual(fakes.api.callsTo('GET', `${ARCHIVES}/url`)[0]?.query, {
        key: 'logs/blog/web/2026/11/18/04.log.gz',
      });
      assert.deepEqual(fakes.browser.assigned, [
        'https://storage.example.org/logs/blog/web/04.log.gz?signature=abc',
      ]);

      await fillIn('[data-test-log-service]', 'db');
      assert.strictEqual(
        fakes.api.callsTo('GET', ARCHIVES).pop()?.query['service'],
        'db',
      );
      assert.strictEqual(
        currentURL(),
        '/apps/blog/logs?period=archive&service=db',
      );
    });

    test('a failed download says why', async function (assert) {
      fakes.api.fail('GET', `${ARCHIVES}/url`, 500, 'internal');

      await visit('/apps/blog/logs?period=archive');
      await click('[data-test-archive-download]');

      assert
        .dom('[data-test-log-archives] [data-test-error]')
        .includesText('サーバーでエラーが発生しました');
      assert.deepEqual(fakes.browser.assigned, []);
    });

    test('a long list is shown a page at a time', async function (assert) {
      fakes.api.on('GET', ARCHIVES, {
        archives: Array.from({ length: 130 }, (_, n) => ({
          key: `logs/blog/web/2026/11/18/${n}.log.gz`,
          service: 'web',
          hour: '2026-11-18T04:00:00+09:00',
          size: 1000 + n,
        })),
      });

      await visit('/apps/blog/logs?period=archive');

      assert.dom('[data-test-archive]').exists({ count: 100 });
      assert.dom('[data-test-archives-more]').includesText('残り 30 件');

      await click('[data-test-archives-more]');
      assert.dom('[data-test-archive]').exists({ count: 130 });
      assert.dom('[data-test-archives-more]').doesNotExist();
    });

    test('without object storage it says so plainly', async function (assert) {
      fakes.api.fail('GET', ARCHIVES, 503, 'not_configured');

      await visit('/apps/blog/logs?period=archive');

      assert
        .dom('[data-test-archives-not-configured]')
        .includesText('ログのアーカイブは設定されていません')
        .includesText('オブジェクトストレージが未設定');
      assert.dom('[data-test-archive]').doesNotExist();

      // The rest of the screen still works.
      await fillIn('[data-test-log-period]', 'tail');
      assert.strictEqual(fakes.eventSource.streams.length, 1);
      assert.dom('[data-test-log-archives]').doesNotExist();
    });

    test('a failed list can be reloaded', async function (assert) {
      fakes.api.fail('GET', ARCHIVES, 500, 'internal');

      await visit('/apps/blog/logs?period=archive');

      assert
        .dom('[data-test-log-archives] [data-test-error]')
        .includesText('アーカイブの一覧を取得できませんでした');
    });

    test('with nothing archived yet it offers the recent logs', async function (assert) {
      fakes.api.on('GET', ARCHIVES, { archives: [] });

      await visit('/apps/blog/logs?period=archive');

      assert
        .dom('[data-test-no-archives]')
        .includesText('アーカイブはまだありません');

      await click('[data-test-empty-action]');
      assert.strictEqual(currentURL(), '/apps/blog/logs?period=24h');
    });
  });
});
