import { module, test } from 'qunit';
import {
  deriveAppState,
  isActiveDeployment,
} from 'koroku-console/utils/status';
import { makeApp, makeDeployment } from 'koroku-console/tests/helpers/fakes';

module('Unit | Utility | status', function () {
  test('deriveAppState', function (assert) {
    const ok = makeDeployment({ id: 1 });

    assert.strictEqual(deriveAppState(makeApp()), 'undeployed');
    assert.strictEqual(
      deriveAppState(
        makeApp({ current_deployment: ok, latest_deployment: ok }),
      ),
      'running',
    );
    assert.strictEqual(
      deriveAppState(
        makeApp({
          desired_state: 'stopped',
          current_deployment: ok,
          latest_deployment: ok,
        }),
      ),
      'stopped',
    );
    assert.strictEqual(
      deriveAppState(
        makeApp({
          latest_deployment: makeDeployment({ id: 2, status: 'queued' }),
        }),
      ),
      'deploying',
      'the first deployment is already "deploying"',
    );
    assert.strictEqual(
      deriveAppState(
        makeApp({
          desired_state: 'stopped',
          current_deployment: ok,
          latest_deployment: makeDeployment({ id: 2, status: 'running' }),
        }),
      ),
      'deploying',
      'a deployment in progress wins over everything else',
    );
    assert.strictEqual(
      deriveAppState(
        makeApp({
          current_deployment: ok,
          latest_deployment: makeDeployment({ id: 2, status: 'failed' }),
        }),
      ),
      'failed',
      'a failed latest deployment is flagged even though the old one serves',
    );
    assert.strictEqual(
      deriveAppState(
        makeApp({
          latest_deployment: makeDeployment({ id: 2, status: 'failed' }),
        }),
      ),
      'failed',
    );
    assert.strictEqual(
      deriveAppState(
        makeApp({
          current_deployment: ok,
          latest_deployment: makeDeployment({ id: 2, status: 'canceled' }),
        }),
      ),
      'running',
      'a canceled deployment leaves the app as it was',
    );
    assert.strictEqual(
      deriveAppState(
        makeApp({
          latest_deployment: makeDeployment({ id: 2, status: 'canceled' }),
        }),
      ),
      'undeployed',
    );
  });

  test('isActiveDeployment', function (assert) {
    assert.true(isActiveDeployment({ status: 'queued' }));
    assert.true(isActiveDeployment({ status: 'running' }));
    assert.false(isActiveDeployment({ status: 'succeeded' }));
    assert.false(isActiveDeployment({ status: 'failed' }));
    assert.false(isActiveDeployment({ status: 'canceled' }));
    assert.false(isActiveDeployment(null));
  });
});
