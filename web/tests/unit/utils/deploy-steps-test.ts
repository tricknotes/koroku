import { module, test } from 'qunit';
import {
  isErrorLine,
  logSegments,
  parseDeployLog,
  parseHeadline,
  stepLabel,
} from 'paas-console/utils/deploy-steps';
import type { Deployment } from 'paas-console/utils/types';

type Times = Pick<
  Deployment,
  'status' | 'queued_at' | 'started_at' | 'finished_at'
>;

function times(overrides: Partial<Times> = {}): Times {
  return {
    status: 'succeeded',
    queued_at: '2026-11-18T03:00:00Z',
    started_at: '2026-11-18T03:00:01Z',
    finished_at: '2026-11-18T03:01:10Z',
    ...overrides,
  };
}

/** A headline `seconds` after 03:00:00. */
function head(seconds: number, key: string, title: string): string {
  const time = new Date(Date.UTC(2026, 10, 18, 3, 0, seconds))
    .toISOString()
    .replace('.000', '');
  return `==> ${time} ${key} ${title}`;
}

const UNTIL_START = [
  'Queued deployment of 9fceb02.',
  head(1, 'fetch', 'Fetching 9fceb02'),
  'Checked out 9fceb02 Fix login',
  head(3, 'prepare', 'Preparing environment and Compose override'),
  'Compose file: compose.yaml',
  head(4, 'validate', 'Validating the Compose project'),
  'Services: [db web]',
  head(6, 'build', 'Building images'),
  '#1 [web 1/6] FROM ruby:3.4-slim',
  '#9 exporting to image',
  head(50, 'start', 'Starting containers'),
  ' Container paas-blog-web-1  Started',
];

function summary(lines: string[], deployment: Times): string[] {
  return parseDeployLog(lines, deployment).steps.map(
    (step) => `${step.key}:${step.state}`,
  );
}

module('Unit | Utility | deploy-steps', function () {
  test('a headline is `==> <time> <key> <title>`', function (assert) {
    assert.deepEqual(
      parseHeadline('==> 2026-11-18T03:00:01Z build Building images'),
      {
        time: '2026-11-18T03:00:01Z',
        key: 'build',
        title: 'Building images',
      },
    );
    assert.strictEqual(parseHeadline('==> not-a-time build Building'), null);
    assert.strictEqual(parseHeadline('=> 2026-11-18T03:00:01Z build x'), null);
    assert.strictEqual(parseHeadline('#1 ==> 2026-11-18T03:00:01Z a b'), null);
  });

  test('a successful log becomes steps with their lines and durations', function (assert) {
    const { steps, failure, rollback } = parseDeployLog(
      [
        ...UNTIL_START,
        head(59, 'route', 'Updating routes'),
        head(60, 'check', 'Checking http://127.0.0.1:20000/'),
        'The app is answering.',
        head(69, 'done', 'Deployment succeeded'),
        head(69, 'cleanup', 'Pruning dangling images'),
        'Total reclaimed space: 0B',
      ],
      times(),
    );

    assert.deepEqual(
      steps.map((step) => step.key),
      [
        'queue',
        'fetch',
        'prepare',
        'validate',
        'build',
        'start',
        'route',
        'check',
        'done',
        'cleanup',
      ],
    );
    assert.true(steps.every((step) => step.state === 'done'));
    assert.strictEqual(failure, null);
    assert.strictEqual(rollback, null);

    const build = steps.find((step) => step.key === 'build');
    assert.deepEqual(build?.lines, [
      '#1 [web 1/6] FROM ruby:3.4-slim',
      '#9 exporting to image',
    ]);
    assert.strictEqual(build?.title, 'Building images');
    assert.strictEqual(build?.startedAt, '2026-11-18T03:00:06Z');
    assert.strictEqual(build?.endedAt, '2026-11-18T03:00:50Z');

    const queue = steps[0];
    assert.strictEqual(queue?.startedAt, '2026-11-18T03:00:00Z');
    assert.strictEqual(queue?.endedAt, '2026-11-18T03:00:01Z');
    assert.deepEqual(queue?.lines, ['Queued deployment of 9fceb02.']);

    // Markers written after the result was recorded have no duration.
    assert.strictEqual(steps.at(-2)?.endedAt, null, 'done');
    assert.strictEqual(steps.at(-1)?.endedAt, null, 'cleanup');
  });

  test('while it runs, the last step is running and the rest is pending', function (assert) {
    const deployment = times({ status: 'running', finished_at: null });

    assert.deepEqual(summary(UNTIL_START.slice(0, 10), deployment), [
      'queue:done',
      'fetch:done',
      'prepare:done',
      'validate:done',
      'build:running',
      'start:pending',
      'check:pending',
      'done:pending',
    ]);

    assert.deepEqual(
      summary(
        ['Queued deployment of 9fceb02.'],
        times({ status: 'queued', started_at: null, finished_at: null }),
      ),
      [
        'queue:running',
        'fetch:pending',
        'prepare:pending',
        'validate:pending',
        'start:pending',
        'check:pending',
        'done:pending',
      ],
    );
  });

  test('`done` is never shown as running, even before the status catches up', function (assert) {
    const lines = [
      ...UNTIL_START,
      head(60, 'check', 'Checking http://127.0.0.1:20000/'),
      head(69, 'done', 'Deployment succeeded'),
    ];
    const steps = summary(lines, times({ status: 'running' }));
    assert.strictEqual(steps.at(-1), 'done:done');
    assert.false(steps.some((step) => step.endsWith(':pending')));
  });

  test('a failed start is followed by the rollback to the previous release', function (assert) {
    const { steps, failure, rollback } = parseDeployLog(
      [
        ...UNTIL_START,
        head(60, 'check', 'Checking http://127.0.0.1:20000/'),
        'GET / returned 500',
        '',
        head(63, 'failed', 'Deployment failed: health check did not pass'),
        head(63, 'rollback', 'Rolling back to deployment #41'),
        ' Container paas-blog-web-1  Started',
        'Rolled back: deployment #41 is running again.',
      ],
      times({ status: 'failed', finished_at: '2026-11-18T03:01:09Z' }),
    );

    const check = steps.find((step) => step.key === 'check');
    assert.strictEqual(check?.state, 'failed');
    assert.deepEqual(check?.lines, ['GET / returned 500'], 'no blank tail');
    assert.strictEqual(check?.endedAt, '2026-11-18T03:01:03Z');

    assert.strictEqual(failure?.step, check);
    assert.strictEqual(
      failure?.title,
      'Deployment failed: health check did not pass',
    );
    assert.false(
      steps.some((step) => step.key === 'failed'),
      'the failure is not a step of its own',
    );

    const last = steps.at(-1);
    assert.strictEqual(last?.key, 'rollback');
    assert.strictEqual(last?.state, 'done');
    assert.strictEqual(last?.endedAt, '2026-11-18T03:01:09Z');
    assert.deepEqual(rollback, {
      kind: 'restored',
      deploymentId: 41,
      detail: '',
    });
  });

  test('a rollback that is still going, did not work or was impossible', function (assert) {
    const failedAt = [
      ...UNTIL_START,
      '',
      head(58, 'failed', 'Deployment failed: start containers: exit status 1'),
    ];
    const rolling = head(58, 'rollback', 'Rolling back to deployment #41');

    const going = parseDeployLog(
      [...failedAt, rolling, ' Container paas-blog-web-1  Starting'],
      times({ status: 'running', finished_at: null }),
    );
    assert.strictEqual(going.rollback?.kind, 'restoring');
    assert.strictEqual(going.steps.at(-1)?.state, 'running');
    assert.false(
      going.steps.some((step) => step.state === 'pending'),
      'nothing more is expected after a failure',
    );

    const awaited = parseDeployLog(
      failedAt,
      times({ status: 'running', finished_at: null }),
    );
    assert.strictEqual(
      awaited.rollback?.kind,
      'restoring',
      'the rollback headline is about to follow a failed start',
    );

    const broken = parseDeployLog(
      [...failedAt, rolling, 'Rollback failed: exit status 1'],
      times({ status: 'failed' }),
    );
    assert.deepEqual(broken.rollback, {
      kind: 'failed',
      deploymentId: 41,
      detail: 'Rollback failed: exit status 1',
    });
    assert.strictEqual(broken.steps.at(-1)?.state, 'failed');

    const impossible = parseDeployLog(
      [
        ...failedAt,
        rolling,
        'Rollback is not possible: the previous release is no longer on disk.',
      ],
      times({ status: 'failed' }),
    );
    assert.strictEqual(impossible.rollback?.kind, 'failed');

    const cutShort = parseDeployLog(
      [...failedAt, rolling],
      times({ status: 'failed' }),
    );
    assert.strictEqual(
      cutShort.rollback?.kind,
      'failed',
      'no verdict in a finished log is not a success',
    );
  });

  test('a failure before the containers were touched leaves the release alone', function (assert) {
    const { steps, rollback } = parseDeployLog(
      [
        ...UNTIL_START.slice(0, 10),
        'ERROR: process "/bin/sh -c bundle install" did not complete',
        '',
        head(40, 'failed', 'Deployment failed: build: exit status 1'),
      ],
      times({ status: 'failed' }),
    );

    assert.strictEqual(steps.at(-1)?.key, 'build');
    assert.strictEqual(steps.at(-1)?.state, 'failed');
    assert.deepEqual(rollback, {
      kind: 'untouched',
      deploymentId: null,
      detail: '',
    });
  });

  test('a first deployment has nothing to go back to', function (assert) {
    const { rollback, steps } = parseDeployLog(
      [
        ...UNTIL_START,
        '',
        head(
          58,
          'failed',
          'Deployment failed: start containers: exit status 1',
        ),
        head(
          58,
          'rollback',
          'No previous release; removing the failed containers',
        ),
        ' Container paas-blog-web-1  Removed',
      ],
      times({ status: 'failed' }),
    );
    assert.strictEqual(rollback?.kind, 'removed');
    assert.strictEqual(rollback?.deploymentId, null);
    assert.strictEqual(steps.at(-1)?.state, 'done');
  });

  test('a shutdown skips the rollback, and says so', function (assert) {
    const { rollback } = parseDeployLog(
      [
        ...UNTIL_START,
        '',
        head(58, 'failed', 'Deployment failed: interrupted'),
        'paasd is shutting down; the previous release is not restored automatically.',
      ],
      times({ status: 'failed' }),
    );
    assert.strictEqual(rollback?.kind, 'skipped');
  });

  test('a canceled deployment marks the step it stopped in', function (assert) {
    const byUser = parseDeployLog(
      [
        ...UNTIL_START.slice(0, 10),
        '',
        head(20, 'failed', 'Deployment canceled: canceled by user'),
      ],
      times({ status: 'canceled' }),
    );
    assert.strictEqual(byUser.steps.at(-1)?.state, 'canceled');
    assert.strictEqual(byUser.rollback?.kind, 'untouched');

    const beforeStart = parseDeployLog(
      ['Queued deployment of 9fceb02.', 'Canceled before it started.'],
      times({ status: 'canceled', started_at: null }),
    );
    assert.deepEqual(
      beforeStart.steps.map((step) => `${step.key}:${step.state}`),
      ['queue:canceled'],
    );
    assert.strictEqual(beforeStart.rollback, null);
  });

  test('a deployment interrupted by a restart fails in its last step', function (assert) {
    const { steps, failure, rollback } = parseDeployLog(
      [
        ...UNTIL_START.slice(0, 10),
        'Failed: interrupted: paasd restarted while the deployment was running.',
      ],
      times({ status: 'failed' }),
    );
    assert.strictEqual(steps.at(-1)?.key, 'build');
    assert.strictEqual(steps.at(-1)?.state, 'failed');
    assert.strictEqual(failure, null);
    assert.strictEqual(rollback, null);
  });

  test('an empty log has no steps; a repeated key gets its own id', function (assert) {
    assert.deepEqual(parseDeployLog([], times()).steps, []);

    const { steps } = parseDeployLog(
      [head(1, 'fetch', 'Fetching a'), head(2, 'fetch', 'Fetching b')],
      times(),
    );
    assert.deepEqual(
      steps.map((step) => step.id),
      ['fetch', 'fetch-2'],
    );
  });

  test('steps are named in Japanese, unknown keys as they are', function (assert) {
    assert.strictEqual(stepLabel('fetch'), '取得');
    assert.strictEqual(stepLabel('check'), '確認');
    assert.strictEqual(stepLabel('rollback'), 'ロールバック');
    assert.strictEqual(stepLabel('migrate'), 'migrate');
  });

  test('error lines are recognised and grouped into segments', function (assert) {
    for (const line of [
      'ERROR: failed to solve: process did not complete',
      'Policy violation: service "db" publishes port 5432',
      'GET /up returned 500 Internal Server Error',
      'container blog-web-1 exited with code 1',
      'dial tcp 127.0.0.1:20000: connect: connection refused',
      'Rollback failed: exit status 1',
    ]) {
      assert.true(isErrorLine(line), line);
    }
    for (const line of [
      '#7 [web 5/7] RUN bundle install',
      ' Container paas-blog-web-1  Started',
      'GET /up returned 200 OK',
      'exited with code 0',
    ]) {
      assert.false(isErrorLine(line), line);
    }

    const lines = ['step 1', 'step 2', 'ERROR: boom', 'fatal: again', 'bye'];
    assert.deepEqual(logSegments(lines, true), [
      { text: 'step 1\nstep 2', error: false },
      { text: 'ERROR: boom\nfatal: again', error: true },
      { text: 'bye', error: false },
    ]);
    assert.deepEqual(
      logSegments(lines, false),
      [{ text: lines.join('\n'), error: false }],
      'nothing is marked outside a failed step',
    );
    assert.deepEqual(logSegments([], true), []);
  });
});
