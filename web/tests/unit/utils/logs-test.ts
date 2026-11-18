import { module, test } from 'qunit';
import LogBuffer from 'paas-console/utils/log-buffer';
import {
  formatLogStamp,
  formatLogTime,
  isBeyondRetention,
  isPeriod,
  jstInputToIso,
  parseLogLine,
  periodSpan,
  serviceLabel,
  splitMatches,
  toJstInput,
} from 'paas-console/utils/logs';
import type { LogLine } from 'paas-console/utils/types';

function line(text: string, service = 'web', time = ''): LogLine {
  return { service, time, line: text };
}

function texts(buffer: LogBuffer): string[] {
  return buffer.rows.map((row) => row.line);
}

module('Unit | Utility | logs', function () {
  test('timestamps are shown in JST whatever the browser zone', function (assert) {
    assert.strictEqual(
      formatLogStamp('2026-11-18T03:04:05.678Z'),
      '11/18 12:04:05',
    );
    assert.strictEqual(
      formatLogStamp('2026-12-31T15:00:00Z'),
      '01/01 00:00:00',
      'the date rolls over with the zone',
    );
    assert.strictEqual(
      formatLogTime('2026-11-18T03:04:05.678901234Z'),
      '2026/11/18 12:04:05.678 JST',
    );
    assert.strictEqual(formatLogStamp(''), '');
    assert.strictEqual(formatLogTime('not a time'), '');
  });

  test('a custom range is read as JST', function (assert) {
    assert.strictEqual(
      jstInputToIso('2026-11-17T00:00'),
      '2026-11-16T15:00:00.000Z',
    );
    assert.strictEqual(
      jstInputToIso('2026-11-17T09:30:15'),
      '2026-11-17T00:30:15.000Z',
    );
    assert.strictEqual(jstInputToIso(''), '');
    assert.strictEqual(jstInputToIso('yesterday'), '');
    assert.strictEqual(
      toJstInput(Date.parse('2026-11-16T15:00:00Z')),
      '2026-11-17T00:00',
    );
  });

  test('periods', function (assert) {
    assert.true(isPeriod('tail'));
    assert.true(isPeriod('archive'));
    assert.false(isPeriod('forever'));
    assert.strictEqual(periodSpan('15m'), 15 * 60 * 1000);
    assert.strictEqual(periodSpan('custom'), undefined);

    const now = Date.parse('2026-11-18T00:00:00Z');
    assert.true(isBeyondRetention('2026-11-03T00:00:00Z', now));
    assert.false(isBeyondRetention('2026-11-05T00:00:00Z', now));
    assert.false(isBeyondRetention('', now));
  });

  test('the pseudo-services get a readable name', function (assert) {
    assert.strictEqual(serviceLabel('web'), 'web');
    assert.strictEqual(serviceLabel('_access'), 'アクセスログ（_access）');
    assert.strictEqual(serviceLabel('_deploy'), 'デプロイログ（_deploy）');
  });

  test('a payload that is not a log line is kept as text', function (assert) {
    assert.deepEqual(
      parseLogLine(
        '{"service":"web","time":"2026-11-18T03:00:00Z","line":"hi"}',
      ),
      { service: 'web', time: '2026-11-18T03:00:00Z', line: 'hi' },
    );
    assert.deepEqual(parseLogLine('{"line":"only text"}'), {
      service: '',
      time: '',
      line: 'only text',
    });
    assert.deepEqual(parseLogLine('plain'), {
      service: '',
      time: '',
      line: 'plain',
    });
    assert.deepEqual(parseLogLine('{"other":1}'), {
      service: '',
      time: '',
      line: '{"other":1}',
    });
  });

  test('keyword matches are split out, case-sensitively', function (assert) {
    assert.deepEqual(splitMatches('GET /a GET', 'GET'), [
      { text: 'GET', match: true },
      { text: ' /a ', match: false },
      { text: 'GET', match: true },
    ]);
    assert.deepEqual(splitMatches('get /a', 'GET'), [
      { text: 'get /a', match: false },
    ]);
    assert.deepEqual(splitMatches('anything', ''), [
      { text: 'anything', match: false },
    ]);
    assert.deepEqual(splitMatches('', 'x'), [{ text: '', match: false }]);
  });
});

module('Unit | Utility | log-buffer', function () {
  test('it keeps only the newest rows', function (assert) {
    const buffer = new LogBuffer(3);
    for (const text of ['a', 'b', 'c']) buffer.push(line(text));
    buffer.flush();
    assert.deepEqual(texts(buffer), ['a', 'b', 'c']);
    assert.false(buffer.dropped);

    buffer.push(line('d'));
    buffer.flush();
    assert.deepEqual(texts(buffer), ['b', 'c', 'd']);
    assert.true(buffer.dropped);

    const ids = buffer.rows.map((row) => row.id);
    assert.deepEqual(
      ids,
      [...ids].sort((a, b) => a - b),
      'ids keep the order',
    );
    assert.strictEqual(new Set(ids).size, 3, 'and stay unique');
  });

  test('lines wait while it is held', function (assert) {
    const buffer = new LogBuffer(10);
    buffer.push(line('a'));
    buffer.flush();

    buffer.hold();
    buffer.push(line('b'));
    buffer.push(line('c'));
    buffer.flush();
    assert.deepEqual(texts(buffer), ['a'], 'the rows stay as they are');
    assert.strictEqual(buffer.waiting, 2);

    buffer.release();
    assert.deepEqual(texts(buffer), ['a', 'b', 'c']);
    assert.strictEqual(buffer.waiting, 0);
  });

  test('a result replaces the rows and a page is appended', function (assert) {
    const buffer = new LogBuffer(10);
    buffer.replace([line('a', 'web', '2026-11-18T03:00:00Z'), line('b', 'db')]);
    assert.deepEqual(texts(buffer), ['a', 'b']);
    assert.strictEqual(buffer.rows[0]?.stamp, '11/18 12:00:00');
    assert.deepEqual(buffer.services, ['web', 'db']);

    buffer.append([line('c', 'worker')]);
    assert.deepEqual(texts(buffer), ['a', 'b', 'c']);
    assert.deepEqual(buffer.services, ['web', 'db', 'worker']);

    buffer.replace([line('z')]);
    assert.deepEqual(texts(buffer), ['z']);

    buffer.clear();
    assert.deepEqual(texts(buffer), []);
    assert.false(buffer.dropped);
  });
});
