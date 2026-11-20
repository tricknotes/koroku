import { module, test } from 'qunit';
import {
  axisBytes,
  axisTime,
  axisTop,
  chartGeometry,
  formatHours,
  nearestPoint,
  shareOfLimit,
} from 'koroku-console/utils/memory-history';
import type {
  MemoryPoint,
  MemoryServiceHistory,
} from 'koroku-console/utils/types';

const MIB = 1024 ** 2;
const FROM = '2026-11-20T00:00:00Z';
const TO = '2026-11-20T01:00:00Z';

function point(
  minute: number,
  max: number,
  limit: number | null = 512,
): MemoryPoint {
  return {
    at: new Date(Date.parse(FROM) + minute * 60_000).toISOString(),
    max_bytes: max * MIB,
    avg_bytes: max * MIB,
    limit_bytes: limit === null ? null : limit * MIB,
  };
}

function service(points: MemoryPoint[]): MemoryServiceHistory {
  return {
    name: 'web',
    limit_bytes: 512 * MIB,
    peak_bytes: Math.max(...points.map((p) => p.max_bytes)),
    restarts: 0,
    points,
    suggestion: {
      basis_since: FROM,
      basis_hours: 1,
      peak_bytes: 0,
      p95_bytes: 0,
      ready: false,
      limit_bytes: null,
      limit: null,
      flag: null,
    },
  };
}

module('Unit | Utility | memory-history', function () {
  test('the axis ends on a round number above usage and limit', function (assert) {
    assert.strictEqual(axisTop(512 * MIB), 768 * MIB);
    assert.strictEqual(axisTop(1024 * MIB), 1536 * MIB);
    assert.strictEqual(axisTop(10 * MIB), 64 * MIB, 'never below 64 MB');
    assert.strictEqual(axisBytes(768 * MIB), '768 MB');
    assert.strictEqual(axisBytes(1536 * MIB), '1.5 GB');
  });

  test('points are joined unless a step is missing', function (assert) {
    const geometry = chartGeometry(
      service([point(0, 100), point(5, 200), point(30, 300), point(35, 300)]),
      { from: FROM, to: TO, step_seconds: 300 },
    );
    // The axis tops at 768 MB: 120 units of height.
    assert.strictEqual(
      geometry.usagePath,
      'M0 104.38 L50 88.75 M300 73.13 L350 73.13',
      'two runs: the gap of 25 minutes breaks the line',
    );
    assert.strictEqual(
      (geometry.areaPath.match(/Z/g) ?? []).length,
      2,
      'one wash per run',
    );
    assert.true(geometry.limitPath.startsWith('M0 40 L50 40'));
    assert.strictEqual(geometry.points.length, 4);
    assert.deepEqual(
      geometry.ticks.map((t) => t.label),
      ['0', '384 MB', '768 MB'],
    );
  });

  test('a limit change is drawn as a step', function (assert) {
    const geometry = chartGeometry(
      service([point(0, 100, 256), point(5, 100, 512)]),
      { from: FROM, to: TO, step_seconds: 300 },
    );
    assert.strictEqual(geometry.limitPath, 'M0 80 L50 80 L50 40 L100 40');
  });

  test('the nearest point to the pointer', function (assert) {
    const geometry = chartGeometry(service([point(0, 1), point(30, 1)]), {
      from: FROM,
      to: TO,
      step_seconds: 300,
    });
    assert.strictEqual(nearestPoint(geometry.points, 0.1), 0);
    assert.strictEqual(nearestPoint(geometry.points, 0.9), 1);
    assert.strictEqual(nearestPoint([], 0.5), -1);
  });

  test('wording', function (assert) {
    assert.strictEqual(formatHours(3), '3 時間');
    assert.strictEqual(formatHours(23.5), '23.5 時間');
    assert.strictEqual(formatHours(48), '2 日');
    assert.strictEqual(formatHours(52), '2 日と 4 時間');
    assert.strictEqual(shareOfLimit(461 * MIB, 512 * MIB), '90%');
    assert.strictEqual(shareOfLimit(1, null), '');
    assert.strictEqual(axisTime('2026-11-20T18:05:00Z'), '11/21 03:05');
  });
});
