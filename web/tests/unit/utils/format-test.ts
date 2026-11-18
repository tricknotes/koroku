import { module, test } from 'qunit';
import {
  firstLine,
  formatBytes,
  formatDateTimeJst,
  formatDuration,
  formatPercent,
  hostOf,
  isFullSha,
  isValidEnvKey,
  isValidSlug,
  relativeTime,
  shortSha,
  suggestSlug,
} from 'koroku-console/utils/format';

const NOW = Date.parse('2026-11-18T12:00:00Z');

module('Unit | Utility | format', function () {
  test('relativeTime', function (assert) {
    assert.strictEqual(relativeTime(null, NOW), '');
    assert.strictEqual(relativeTime('not a date', NOW), '');
    assert.strictEqual(relativeTime('2026-11-18T11:59:30Z', NOW), 'たった今');
    assert.strictEqual(
      relativeTime('2026-11-18T12:00:20Z', NOW),
      'たった今',
      'a clock that is slightly ahead does not show a negative time',
    );
    assert.strictEqual(relativeTime('2026-11-18T11:57:00Z', NOW), '3 分前');
    assert.strictEqual(relativeTime('2026-11-18T11:00:01Z', NOW), '59 分前');
    assert.strictEqual(relativeTime('2026-11-18T07:00:00Z', NOW), '5 時間前');
    assert.strictEqual(relativeTime('2026-11-16T11:00:00Z', NOW), '2 日前');
    assert.true(
      /^2026\/0[67]\/\d\d$/.test(relativeTime('2026-07-01T00:00:00Z', NOW)),
      'older than 30 days falls back to the date',
    );
  });

  test('formatDateTimeJst is JST whatever the local time zone', function (assert) {
    assert.strictEqual(
      formatDateTimeJst('2026-11-18T03:04:05Z'),
      '2026/11/18 12:04 JST',
    );
    assert.strictEqual(
      formatDateTimeJst('2026-11-18T18:30:00Z'),
      '2026/11/19 03:30 JST',
      'the date rolls over with the zone',
    );
    assert.strictEqual(
      formatDateTimeJst('2026-11-17T15:00:00Z'),
      '2026/11/18 00:00 JST',
      'midnight is 00, not 24',
    );
    assert.strictEqual(formatDateTimeJst(null), '');
    assert.strictEqual(formatDateTimeJst('not a date'), '');
  });

  test('hostOf', function (assert) {
    assert.strictEqual(hostOf('https://www.example.org'), 'www.example.org');
    assert.strictEqual(
      hostOf('https://blog.dev.example.com/'),
      'blog.dev.example.com',
    );
    assert.strictEqual(hostOf('http://localhost:20000'), 'localhost:20000');
    assert.strictEqual(hostOf('not a url'), 'not a url');
    assert.strictEqual(hostOf(''), '');
    assert.strictEqual(hostOf(undefined), '');
  });

  test('formatDuration', function (assert) {
    const start = '2026-11-18T03:00:01Z';

    assert.strictEqual(formatDuration(start, '2026-11-18T03:00:10Z'), '9 秒');
    assert.strictEqual(
      formatDuration(start, '2026-11-18T03:01:10Z'),
      '1 分 9 秒',
    );
    assert.strictEqual(
      formatDuration(start, '2026-11-18T05:05:01Z'),
      '2 時間 5 分',
    );
    assert.strictEqual(formatDuration(start, null), '');
    assert.strictEqual(formatDuration(null, start), '');
  });

  test('formatBytes', function (assert) {
    assert.strictEqual(formatBytes(0), '0 B');
    assert.strictEqual(formatBytes(512), '512 B');
    assert.strictEqual(formatBytes(12345), '12.1 KB');
    assert.strictEqual(formatBytes(8589934592), '8.0 GB');
    assert.strictEqual(formatBytes(42949672960), '40.0 GB');
    assert.strictEqual(formatBytes(5 * 1024 ** 5), '5120.0 TB');
    assert.strictEqual(formatBytes(null), '');
    assert.strictEqual(formatBytes(-1), '');
  });

  test('formatPercent', function (assert) {
    assert.strictEqual(formatPercent(21.5), '21.5%');
    assert.strictEqual(formatPercent(21.54), '21.5%');
    assert.strictEqual(formatPercent(50), '50%');
    assert.strictEqual(formatPercent(null), '');
  });

  test('shortSha, firstLine and isFullSha', function (assert) {
    const sha = '9fceb02d0ae598e95dc970b74767f19372d61af8';

    assert.strictEqual(shortSha(sha), '9fceb02');
    assert.strictEqual(shortSha(null), '');
    assert.strictEqual(firstLine('Fix login\n\nDetails'), 'Fix login');
    assert.strictEqual(firstLine(null), '');
    assert.true(isFullSha(sha));
    assert.false(isFullSha('9fceb02'));
    assert.false(isFullSha('main'));
  });

  test('suggestSlug follows the slug rules of the API', function (assert) {
    assert.strictEqual(suggestSlug('octo/blog'), 'blog');
    assert.strictEqual(suggestSlug('octo/Docs.Site'), 'docs-site');
    assert.strictEqual(suggestSlug('octo/2048_game'), 'game');
    assert.strictEqual(
      suggestSlug('https://github.com/octo/sample.git'),
      'sample',
    );
    assert.strictEqual(suggestSlug('octo/blog/'), 'blog');
    assert.strictEqual(suggestSlug(''), '');

    const long = suggestSlug(`octo/${'a'.repeat(39)}-${'b'.repeat(10)}`);
    assert.strictEqual(long.length, 39, 'cut at 40 without a trailing hyphen');
    assert.true(isValidSlug(long));
  });

  test('isValidSlug and isValidEnvKey', function (assert) {
    assert.true(isValidSlug('blog-staging2'));
    assert.false(isValidSlug('Blog'));
    assert.false(isValidSlug('1blog'));
    assert.false(isValidSlug('blog_1'));
    assert.false(isValidSlug(''));
    assert.false(isValidSlug('a'.repeat(41)));

    assert.true(isValidEnvKey('RAILS_ENV'));
    assert.true(isValidEnvKey('_private1'));
    assert.false(isValidEnvKey('1BAD'));
    assert.false(isValidEnvKey('WITH-DASH'));
    assert.false(isValidEnvKey(''));
  });
});
