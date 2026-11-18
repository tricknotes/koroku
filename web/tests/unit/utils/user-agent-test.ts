import { module, test } from 'qunit';
import { describeUserAgent } from 'koroku-console/utils/user-agent';

module('Unit | Utility | user-agent', function () {
  test('names the browser and the system', function (assert) {
    const cases: [string, string][] = [
      [
        'Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36',
        'Chrome / macOS',
      ],
      [
        'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0',
        'Edge / Windows',
      ],
      [
        'Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0',
        'Firefox / Linux',
      ],
      [
        'Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1',
        'Safari / iOS',
      ],
      [
        'Mozilla/5.0 (Linux; Android 15) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36',
        'Chrome / Android',
      ],
      ['curl/8.5.0', 'curl'],
      ['', '不明なブラウザー'],
      ['something else', '不明なブラウザー'],
    ];
    for (const [ua, want] of cases) {
      assert.strictEqual(describeUserAgent(ua), want, ua);
    }
  });
});
