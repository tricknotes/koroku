import { module, test } from 'qunit';
import {
  colorSummary,
  drainSecondsProblem,
  formatClockJst,
  memoryShortage,
  memoryShortageDetail,
  switchText,
} from 'koroku-console/utils/blue-green';
import { containerLabel } from 'koroku-console/utils/container-resources';
import { serviceRows } from 'koroku-console/utils/services';
import SettingsForm from 'koroku-console/utils/settings-form';
import { makeApp } from 'koroku-console/tests/helpers/fakes';
import type { ServiceStatus } from 'koroku-console/utils/types';

function svc(name: string, color?: 'blue' | 'green'): ServiceStatus {
  return {
    name,
    state: 'running',
    status: 'Up',
    health: '',
    ...(color ? { color } : {}),
  };
}

module('Unit | Utility | blue-green', function () {
  test('formatClockJst is the JST wall clock with seconds', function (assert) {
    assert.strictEqual(formatClockJst('2026-11-18T03:03:41Z'), '12:03:41');
    assert.strictEqual(formatClockJst('2026-11-18T15:00:05Z'), '00:00:05');
    assert.strictEqual(formatClockJst(null), '');
    assert.strictEqual(formatClockJst('not a time'), '');
  });

  test('switchText names the colour and the time of the switch', function (assert) {
    assert.strictEqual(
      switchText({ web_color: 'green', switched_at: '2026-11-18T03:03:41Z' }),
      '12:03:41 に green へ切り替え',
    );
    assert.strictEqual(
      switchText({ web_color: 'green', switched_at: null }),
      '',
    );
    assert.strictEqual(
      switchText({ web_color: null, switched_at: '2026-11-18T03:03:41Z' }),
      '',
    );
  });

  test('colorSummary says what became of the colour it started', function (assert) {
    const at = '2026-11-18T03:03:41Z';
    assert.strictEqual(
      colorSummary({ web_color: 'blue', switched_at: at, status: 'succeeded' }),
      '12:03:41 に blue へ切り替え',
    );
    assert.strictEqual(
      colorSummary({
        web_color: 'blue',
        switched_at: null,
        status: 'succeeded',
      }),
      'blue で起動',
    );
    assert.strictEqual(
      colorSummary({ web_color: 'blue', switched_at: null, status: 'running' }),
      'blue を起動中',
    );
    assert.strictEqual(
      colorSummary({ web_color: 'blue', switched_at: null, status: 'failed' }),
      'blue を起動、切り替えなし',
    );
    assert.strictEqual(
      colorSummary({ web_color: null, switched_at: null, status: 'failed' }),
      '',
    );
    assert.strictEqual(
      colorSummary({ status: 'succeeded' }),
      '',
      'an older backend has no colour',
    );
  });

  test('memoryShortage reads the failure for want of memory', function (assert) {
    const error =
      'Not enough memory to run two web containers (312 MiB available, 512 MiB needed).';
    const shortage = memoryShortage(error);
    assert.deepEqual(shortage, {
      service: 'web',
      available: '312 MiB',
      needed: '512 MiB',
    });
    assert.strictEqual(
      memoryShortageDetail(shortage!),
      '空き 312 MiB、必要 512 MiB',
    );
    assert.strictEqual(memoryShortage('health check did not pass'), null);
    assert.strictEqual(memoryShortage(''), null);
    assert.strictEqual(memoryShortage(undefined), null);
  });

  test('drainSecondsProblem takes 0 to 300 whole seconds', function (assert) {
    for (const ok of ['0', '10', ' 300 ']) {
      assert.strictEqual(drainSecondsProblem(ok), null, ok);
    }
    for (const bad of ['', '301', '-1', '1.5', '1e2', 'ten']) {
      assert.ok(drainSecondsProblem(bad)?.includes('0〜300'), bad);
    }
  });

  test('the settings form patches the setting and the wait only when changed', function (assert) {
    const app = makeApp({ blue_green: false, blue_green_drain_seconds: 10 });
    const form = new SettingsForm(app);
    assert.deepEqual(form.patch(app), {});

    form.blueGreen = true;
    form.blueGreenDrainSeconds = ' 45 ';
    assert.deepEqual(form.patch(app), {
      blue_green: true,
      blue_green_drain_seconds: 45,
    });
    assert.deepEqual(form.validate(), {});

    form.blueGreenDrainSeconds = '500';
    assert.ok(form.validate()['blue_green_drain_seconds']);
    assert.ok(
      'blue_green_drain_seconds' in form.patch(app),
      'an invalid value still counts as a change',
    );
  });

  test('an app from an older backend reads as off with the default wait', function (assert) {
    const app = makeApp();
    const form = new SettingsForm(app);
    assert.false(form.blueGreen);
    assert.strictEqual(form.blueGreenDrainSeconds, '10');
    assert.deepEqual(form.patch(app), {});
  });

  test('serviceRows keys each copy and tells which one receives the requests', function (assert) {
    const rows = serviceRows(
      [svc('web', 'blue'), svc('web', 'green'), svc('db')],
      'green',
    );
    assert.deepEqual(
      rows.map((row) => [row.key, row.color, row.live, row.standby]),
      [
        ['web:blue', 'blue', false, true],
        ['web:green', 'green', true, false],
        ['db', null, false, false],
      ],
    );

    const alone = serviceRows([svc('web', 'green'), svc('db')], 'green');
    assert.deepEqual(
      alone.map((row) => [row.key, row.live, row.standby]),
      [
        ['web:green', false, false],
        ['db', false, false],
      ],
      'one copy: nothing to tell apart',
    );
  });

  test('containerLabel adds the colour of a copy', function (assert) {
    const group = {
      app: 'blog',
      memory_bytes: 0,
      memory_limit_bytes: null,
      cpu_percent: 0,
      containers: [],
    };
    const container = {
      name: 'koroku-blog-web-green-1',
      project: 'koroku-blog',
      service: 'web',
      memory_bytes: 0,
      memory_limit_bytes: null,
      cpu_percent: 0,
    };
    assert.strictEqual(
      containerLabel(group, { ...container, color: 'green' }),
      'web（green）',
    );
    assert.strictEqual(containerLabel(group, container), 'web');
  });
});
