import { module, test } from 'qunit';
import SettingsForm from 'koroku-console/utils/settings-form';
import {
  APP_STATE_KINDS,
  APP_STATE_LABELS,
  deriveAppState,
} from 'koroku-console/utils/status';
import { makeApp, makeDeployment } from 'koroku-console/tests/helpers/fakes';

module('Unit | Utility | idle sleep', function () {
  test('a sleeping app is 休止中, grey like a stopped one', function (assert) {
    const ok = makeDeployment({ id: 1 });
    const asleep = makeApp({
      current_deployment: ok,
      latest_deployment: ok,
      sleeping_since: '2026-11-22T01:00:00Z',
    });
    assert.strictEqual(deriveAppState(asleep), 'sleeping');
    assert.strictEqual(APP_STATE_LABELS.sleeping, '休止中');
    assert.strictEqual(APP_STATE_KINDS.sleeping, 'stopped');

    assert.strictEqual(
      deriveAppState({ ...asleep, desired_state: 'stopped' }),
      'stopped',
      'stopped wins over sleeping',
    );
    assert.strictEqual(
      deriveAppState({
        ...asleep,
        latest_deployment: makeDeployment({ id: 2, status: 'running' }),
      }),
      'deploying',
      'a deployment in progress wins',
    );
  });

  test('the settings patch the idle sleep only when changed', function (assert) {
    const app = makeApp({ idle_sleep: false, idle_sleep_minutes: 30 });
    const form = new SettingsForm(app);
    assert.deepEqual(form.patch(app), {});

    form.idleSleep = true;
    form.idleSleepMinutes = 60;
    assert.deepEqual(form.patch(app), {
      idle_sleep: true,
      idle_sleep_minutes: 60,
    });

    // Older backends do not send the fields.
    const old = makeApp();
    assert.deepEqual(new SettingsForm(old).patch(old), {});
  });
});
