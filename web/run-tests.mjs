// Runs the test suite in the browser (`testem ci`) and starts it again when
// the browser never connected.
//
// On a slow machine (a CI runner right after it was created) Chrome can take
// most of a minute to come up and then lose its network service before it
// loads the test page. testem then waits out browser_start_timeout and
// reports one failed "test" named after the browser, without having run
// anything. That says nothing about the code, so the run is repeated, at
// most twice. A run in which the browser connected is never repeated,
// whatever its result: failing tests stay failures.
//
// Usage: node run-tests.mjs [command [args...]]
// The default command is `testem ci --port 0`.

import { spawn } from 'node:child_process';

const MAX_RUNS = 3;

// What testem prints when its browser did not report within
// browser_start_timeout (testem.cjs).
const NEVER_CONNECTED = /Browser failed to connect within \d+s/;

const [command, ...args] =
  process.argv.length > 2
    ? process.argv.slice(2)
    : ['testem', 'ci', '--port', '0'];

function run() {
  return new Promise((resolve) => {
    const child = spawn(command, args, {
      stdio: ['inherit', 'pipe', 'inherit'],
    });
    let output = '';
    child.stdout.on('data', (chunk) => {
      process.stdout.write(chunk);
      output += chunk;
    });
    child.on('error', (error) => {
      console.error(`run-tests: cannot run ${command}: ${error.message}`);
      resolve({ code: 127, output });
    });
    child.on('close', (code, signal) => {
      resolve({ code: code ?? (signal ? 1 : 0), output });
    });
  });
}

for (let attempt = 1; ; attempt++) {
  const { code, output } = await run();
  if (code === 0 || attempt === MAX_RUNS || !NEVER_CONNECTED.test(output)) {
    process.exitCode = code;
    break;
  }
  console.error(
    `run-tests: the browser never connected (run ${attempt} of ${MAX_RUNS}); starting the tests again`,
  );
}
