import { module, test } from 'qunit';
import {
  isoToJstInput,
  jstInputToIso,
  targetProblem,
} from 'koroku-console/utils/pitr';

module('Unit | Utility | pitr', function () {
  test('times are entered in JST', function (assert) {
    assert.strictEqual(
      jstInputToIso('2026-11-27T10:15:30'),
      '2026-11-27T01:15:30.000Z',
    );
    assert.strictEqual(
      jstInputToIso('2026-11-27T08:00'),
      '2026-11-26T23:00:00.000Z',
    );
    assert.strictEqual(jstInputToIso('yesterday'), '');
    assert.strictEqual(
      isoToJstInput('2026-11-26T23:00:00Z'),
      '2026-11-27T08:00:00',
    );
  });

  test('targetProblem refuses what the server would', function (assert) {
    const now = new Date('2026-11-28T03:00:00Z');
    const earliest = '2026-11-21T18:00:00Z';
    assert.strictEqual(
      targetProblem('2026-11-27T01:00:00Z', earliest, now),
      '',
    );
    assert.true(targetProblem('', earliest, now).includes('入力'));
    assert.true(
      targetProblem('2026-11-28T04:00:00Z', earliest, now).includes('未来'),
    );
    assert.true(
      targetProblem('2026-11-20T00:00:00Z', earliest, now).includes('より前'),
    );
    assert.true(
      targetProblem('2026-11-27T01:00:00Z', null, now).includes(
        'ベースバックアップ',
      ),
    );
  });
});
