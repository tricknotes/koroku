// Deployment logs of the mock API (see server.mjs), in the shape koroku
// writes them: step headlines `==> <time> <key> <title>` with the output of
// each step below, the failure and the automatic rollback included
// (internal/deploy/steps.go). Nothing here is real output.

const SECOND = 1000;

/**
 * What a deployment's log depends on besides the deployment itself:
 * `slug`, `hostPort`, `healthPath`, `previousId` (the release that was
 * serving before, or null on a first deployment), `backup` (the app
 * backs up before deploying) and, for a blue/green app (ARCHITECTURE 7.2),
 * `blueGreen`: `{ webService, oldName, ports: { blue, green } }`, where
 * `oldName` is what served before (a colour, the public service's own
 * name, or null when nothing ran).
 */
function projectOf(context) {
  return `koroku-${context.slug}`;
}

function buildLines() {
  return [
    '#0 building with "default" instance using docker driver',
    '#1 [web internal] load build definition from Dockerfile',
    '#1 transferring dockerfile: 1.21kB done',
    '#2 [web internal] load metadata for docker.io/library/ruby:3.4-slim',
    '#3 [web 1/7] FROM docker.io/library/ruby:3.4-slim',
    '#4 [web 2/7] RUN apt-get update && apt-get install -y build-essential libpq-dev',
    '#4 CACHED',
    '#5 [web 3/7] WORKDIR /app',
    '#5 CACHED',
    '#6 [web 4/7] COPY Gemfile Gemfile.lock ./',
    '#7 [web 5/7] RUN bundle install',
    '#7 1.482 Fetching gem metadata from https://rubygems.org/.........',
    '#7 4.917 Installing rack 3.2.4',
    '#7 5.208 Installing railties 8.1.2',
    '#7 9.730 Bundle complete! 24 Gemfile dependencies, 96 gems now installed.',
    '#8 [web 6/7] COPY . .',
    '#9 [web 7/7] RUN bin/rails assets:precompile',
    '#9 3.356 Compiled application.css (241.3ms)',
    '#9 DONE 6.1s',
    '#10 exporting to image',
    '#10 writing image sha256:5f1c0d6f3b0a done',
    '#10 naming to docker.io/library/app-web done',
  ];
}

function startLines(context) {
  const project = projectOf(context);
  return [
    ` Container ${project}-db-1  Running`,
    ` Container ${project}-web-1  Recreate`,
    ` Container ${project}-web-1  Recreated`,
    ` Container ${project}-web-1  Starting`,
    ` Container ${project}-web-1  Started`,
    ` Container ${project}-db-1  Waiting`,
    ` Container ${project}-db-1  Healthy`,
  ];
}

/**
 * The steps from `start` on of a blue/green deployment
 * (internal/deploy/bluegreen.go). `entry.mockFailure` picks how a failed
 * one ends: `memory` (no room for a second copy), `check` (the new colour
 * fails its check and is removed) or `services` (a worker fails after the
 * switch; the requests go back).
 */
function blueGreenSteps(entry, context, step, at, stamp, scale) {
  const bg = context.blueGreen;
  const project = projectOf(context);
  const color = entry.web_color ?? entry.mockColor;
  const port = bg.ports[color];
  const fail = (title) => [
    { line: '' },
    { line: `==> ${stamp(at())} failed ${title}` },
  ];
  const out = [];
  const push = (items) =>
    items.forEach((item) => out.push({ at: at(), line: item.line }));

  if (bg.oldName === null) {
    // Nothing to switch from: everything starts at once, as in chapter 7.
    step(
      'start',
      `Starting ${color} on port ${port}`,
      [
        'The public service is not running; all services start at once.',
        ` Container ${project}-db-1  Started`,
        ` Container ${project}-${bg.webService}-${color}-1  Started`,
      ],
      6 * SECOND * scale,
    );
    step('route', 'Updating routes', [], 1 * SECOND * scale);
    return { out, finished: false };
  }

  const service = `${bg.webService}-${color}`;
  if (entry.mockFailure === 'memory') {
    step(
      'start',
      `Starting ${color} on port ${port}`,
      [
        entry.error,
        'Free memory (stop or put other apps to sleep, lower the limit) or turn blue/green deploys off, then deploy again.',
      ],
      1 * SECOND * scale,
    );
    push(fail(`Deployment failed: ${entry.error}`));
    return { out, finished: true };
  }
  step(
    'start',
    `Starting ${color} on port ${port}`,
    [
      ` Container ${project}-migrate-1  Started`,
      ` Container ${project}-migrate-1  Exited`,
      ` Container ${project}-${service}-1  Created`,
      ` Container ${project}-${service}-1  Started`,
      ` Container ${project}-${service}-1  Healthy`,
    ],
    7 * SECOND * scale,
  );
  const url = `http://127.0.0.1:${port}${context.healthPath}`;
  if (entry.mockFailure === 'check') {
    step(
      'check',
      `Checking ${url}`,
      [
        'Last output of the containers:',
        `${service}-1  | => Booting Puma`,
        `${service}-1  | Started GET "${context.healthPath}" for 172.18.0.1`,
        `${service}-1  | ActiveRecord::StatementInvalid (PG::UndefinedColumn: ERROR:  column posts.reading_minutes does not exist)`,
        `${service}-1  | Completed 500 Internal Server Error in 9ms`,
        `Removing ${service}; the previous release keeps serving.`,
      ],
      3 * SECOND * scale,
    );
    push(fail(`Deployment failed: ${entry.error}`));
    return { out, finished: true };
  }
  step(
    'check',
    `Checking ${url}`,
    ['The app is answering.'],
    2 * SECOND * scale,
  );
  step(
    'switch',
    `Switching traffic from ${bg.oldName} to ${color}`,
    [`Requests now reach ${color} on port ${port}.`],
    1 * SECOND * scale,
  );
  if (entry.mockFailure === 'services') {
    step(
      'services',
      'Recreating the other services',
      [
        ` Container ${project}-worker-1  Recreate`,
        ` Container ${project}-worker-1  Error`,
        'Last output of the containers:',
        'worker-1  | NameError: uninitialized constant ReadingTimeJob',
      ],
      4 * SECOND * scale,
    );
    step(
      'rollback',
      `Switching back and restoring the other services of deployment #${context.previousId}`,
      [
        'Requests reach the previous release again.',
        ` Container ${project}-worker-1  Recreated`,
        `Restored worker of deployment #${context.previousId}.`,
        `Removing ${service}.`,
      ],
      4 * SECOND * scale,
    );
    push(
      fail(
        `Deployment failed: ${entry.error}; the previous release was restored`,
      ),
    );
    return { out, finished: true };
  }
  step(
    'services',
    'Recreating the other services',
    [
      ` Container ${project}-worker-1  Recreate`,
      ` Container ${project}-worker-1  Recreated`,
      ` Container ${project}-worker-1  Started`,
    ],
    4 * SECOND * scale,
  );
  const oldService =
    bg.oldName === bg.webService
      ? bg.webService
      : `${bg.webService}-${bg.oldName}`;
  step(
    'drain',
    `Stopping ${bg.oldName}`,
    [
      `Waiting ${context.drainSeconds}s for the requests in flight.`,
      ` Container ${project}-${oldService}-1  Stopping`,
      ` Container ${project}-${oldService}-1  Stopped`,
      ` Container ${project}-${oldService}-1  Removed`,
    ],
    Math.max(1, context.drainSeconds) * SECOND * scale,
  );
  return { out, finished: false };
}

/**
 * The whole log of a deployment that runs to its end, as a list of
 * `{ at, line }`: `at` is the offset from the start in milliseconds.
 */
function plan(entry, context) {
  const failed = entry.status === 'failed';
  const canceled = entry.status === 'canceled';
  const started = Date.parse(entry.started_at ?? entry.queued_at);
  const finished = entry.finished_at
    ? Date.parse(entry.finished_at)
    : (entry.mockFinishAt ?? started + 70 * SECOND);
  const total = Math.max(8 * SECOND, finished - started);
  // A deployment started through the mock is over in seconds: every step
  // shrinks with it. A rollback has no build and is short anyway.
  const usual = (entry.source_deployment_id ? 15 : 45) * SECOND;
  const scale = Math.min(1, total / usual);
  const url = `http://127.0.0.1:${context.hostPort}${context.healthPath}`;
  const out = [];
  let at = 0;

  const step = (key, title, body, duration) => {
    out.push({ at, line: `==> ${stamp(started + at)} ${key} ${title}` });
    body.forEach((line, index) => {
      out.push({
        at: at + ((index + 1) * duration) / (body.length + 1),
        line,
      });
    });
    at += duration;
  };
  const stamp = (time) => new Date(time).toISOString().replace('.000', '');

  if (entry.source_deployment_id) {
    // A rollback starts a kept release again: no fetch, build or pull
    // (internal/deploy/rollback.go).
    const source = entry.source_deployment_id;
    out.push({
      at: -1,
      line: `Queued rollback to deployment #${source} (${entry.commit_sha}).`,
    });
    step(
      'prepare',
      `Reusing the release of deployment #${source}`,
      [
        `Compose file: compose.yaml; images: [koroku-${context.slug}-web:r${source}]`,
      ],
      1 * SECOND * scale,
    );
    step(
      'validate',
      'Validating the release again',
      ['Services: [db web worker]'],
      2 * SECOND * scale,
    );
  } else {
    out.push({ at: -1, line: `Queued deployment of ${entry.commit_sha}.` });
    const short = entry.commit_sha.slice(0, 7);
    // Only a commit that the deployment's branch or tag reaches is fetched
    // (ARCHITECTURE 7).
    const checking = `Checking that ${short} is on ${entry.ref || 'main'}`;
    if (entry.mockFailure === 'fetch') {
      step('fetch', `Fetching ${entry.commit_sha}`, [checking], 1 * SECOND);
      out.push({ at, line: '' });
      out.push({
        at,
        line: `==> ${stamp(started + at)} failed Deployment failed: ${entry.error}`,
      });
      return out;
    }
    step(
      'fetch',
      `Fetching ${entry.commit_sha}`,
      [checking, `Checked out ${short} ${entry.commit_message}`],
      2 * SECOND * scale,
    );
    step(
      'prepare',
      'Preparing environment and Compose override',
      ['Compose file: compose.yaml'],
      1 * SECOND * scale,
    );
    step(
      'validate',
      'Validating the Compose project',
      ['Services: [db web worker]'],
      2 * SECOND * scale,
    );
    // The build takes whatever the other steps leave of the total.
    const fixed = (context.backup ? 30 : 22) * SECOND * scale;
    const promoted = entry.promoted_from;
    const images = entry.mockImages ?? [];
    if (promoted) {
      const source = `koroku-${promoted.app_slug}`;
      step(
        'reuse',
        `Reusing images of ${promoted.app_slug} deployment #${promoted.deployment_id}`,
        images.map((image) =>
          image.reuse
            ? `${image.service}: reusing ${source}-${image.service}:r${promoted.deployment_id}`
            : `${image.service}: building (build args differ: ${image.differing_args.join(', ')})`,
        ),
        2 * SECOND * scale,
      );
    }
    if (!promoted || images.some((image) => !image.reuse)) {
      step(
        'build',
        'Building images',
        buildLines(),
        Math.max(4 * SECOND * scale, total - fixed),
      );
    }
    step(
      'pull',
      'Pulling images',
      [' db Pulling', ' db Pulled'],
      3 * SECOND * scale,
    );
    if (context.backup && context.previousId !== null) {
      step(
        'backup',
        'Backing up before deploy',
        ['Backup finished.'],
        8 * SECOND * scale,
      );
    }
  }
  if (canceled) {
    out.push({ at, line: '' });
    out.push({
      at,
      line: `==> ${stamp(started + at)} failed Deployment canceled: ${entry.error || 'canceled by user'}`,
    });
    return out;
  }
  if (context.blueGreen) {
    const { out: extra, finished } = blueGreenSteps(
      entry,
      context,
      step,
      () => at,
      (time) => stamp(started + time),
      scale,
    );
    out.push(...extra);
    if (finished) return out;
    if (!failed) {
      // Started alone, it routes and checks afterwards; switched, it
      // checked before the switch.
      if (out.some((item) => / route /.test(item.line))) {
        step(
          'check',
          `Checking ${url}`,
          ['The app is answering.'],
          2 * SECOND * scale,
        );
      }
      step('done', 'Deployment succeeded', [], 0);
      step(
        'cleanup',
        'Pruning dangling images',
        ['Total reclaimed space: 0B'],
        1 * SECOND * scale,
      );
      return out;
    }
  }

  step('start', 'Starting containers', startLines(context), 9 * SECOND * scale);
  if (context.previousId === null) {
    step('route', 'Updating routes', [], 1 * SECOND * scale);
  }

  if (failed) {
    step(
      'check',
      `Checking ${url}`,
      // What koroku really writes here: nothing while it polls, then the
      // last output of the containers (docker compose logs --tail 30).
      [
        'Last output of the containers:',
        'db-1   | LOG:  database system is ready to accept connections',
        'web-1  | => Booting Puma',
        'web-1  | => Rails 8.1.2 application starting in production',
        'web-1  | Puma starting in single mode...',
        'web-1  | * Listening on http://0.0.0.0:3000',
        `web-1  | Started GET "${context.healthPath}" for 172.18.0.1`,
        "web-1  | NameError (undefined local variable or method 'payment_gateway' for an instance of CheckoutController)",
        `web-1  | Completed 500 Internal Server Error in 12ms`,
      ],
      3 * SECOND * scale,
    );
    out.push({ at, line: '' });
    out.push({
      at,
      line: `==> ${stamp(started + at)} failed Deployment failed: ${entry.error}`,
    });
    if (context.previousId === null) {
      step(
        'rollback',
        'No previous release; removing the failed containers',
        [` Container ${projectOf(context)}-web-1  Removed`],
        2 * SECOND * scale,
      );
    } else {
      step(
        'rollback',
        `Rolling back to deployment #${context.previousId}`,
        [
          ...startLines(context),
          `Rolled back: deployment #${context.previousId} is running again.`,
        ],
        2 * SECOND * scale,
      );
    }
    return out;
  }

  step(
    'check',
    `Checking ${url}`,
    ['The app is answering.'],
    2 * SECOND * scale,
  );
  step('done', 'Deployment succeeded', [], 0);
  step(
    'cleanup',
    'Pruning dangling images',
    ['Total reclaimed space: 0B'],
    1 * SECOND * scale,
  );
  return out;
}

function isActive(entry) {
  return entry.status === 'running' || entry.status === 'queued';
}

/**
 * The log of a deployment as it stands, and how it goes on: `lines` is what
 * was written so far and `end` the final status when it is over (the API's
 * `event: end`). While it runs, `next()` returns what was written since the
 * last call, as `{ lines, end }` again.
 */
export function deploymentLog(entry, context) {
  if (!isActive(entry)) {
    return {
      lines: plan(entry, context).map((item) => item.line),
      end: entry.status,
    };
  }

  const started = Date.parse(entry.started_at ?? entry.queued_at);
  let script = plan({ ...entry, status: 'succeeded' }, context);
  // One from a fixture never ends: it stays in its build.
  const endless = entry.mockFinishAt === undefined;
  if (endless) {
    // It stays in its build, or in the step `mockStopAt` names.
    const key = entry.mockStopAt ?? 'build';
    const begin = script.findIndex((item) =>
      new RegExp(`^==> \\S+ ${key} `).test(item.line),
    );
    const next = script.findIndex(
      (item, index) => index > begin && item.line.startsWith('==> '),
    );
    const until = key === 'build' ? begin + 13 : next;
    script = script
      .slice(0, until)
      .map((item) => ({ ...item, at: Math.min(item.at, 0) }));
  }

  let sent = 0;
  const due = () => {
    const elapsed = Date.now() - started;
    let until = sent;
    while (until < script.length && script[until].at <= elapsed) until++;
    const lines = script.slice(sent, until).map((item) => item.line);
    sent = until;
    return lines;
  };
  let calls = 0;
  return {
    lines: due(),
    next() {
      if (!isActive(entry)) {
        const rest = entry.status === 'succeeded' ? script.slice(sent) : [];
        sent = script.length;
        return { lines: rest.map((item) => item.line), end: entry.status };
      }
      if (!endless) return { lines: due() };
      if (entry.mockStopAt) return { lines: [] };
      // Now and then a line, as a slow build would print.
      calls++;
      if (calls % 4 !== 0) return { lines: [] };
      const seconds = (Date.now() - started) / SECOND;
      return { lines: [`#7 ${seconds.toFixed(3)} Installing gems...`] };
    },
  };
}
