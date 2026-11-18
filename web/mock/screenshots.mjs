// Takes screenshots of the console against the mock API, for design review.
// It starts the mock (server.mjs) and a headless Chromium, drives the
// browser over the DevTools protocol and writes PNGs to web/tmp/shots/
// (git-ignored). No dependencies; a development tool only.
//
//   node mock/screenshots.mjs            (from web/, after `pnpm build`)
//   node mock/screenshots.mjs list-      (only shots whose name starts so)
//
// The theme is chosen by emulating the OS setting (`prefers-color-scheme`),
// which is the path a first-time visitor takes.

import { spawn } from 'node:child_process';
import { mkdir, mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { setTimeout as sleep } from 'node:timers/promises';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const OUT = resolve(HERE, '..', 'tmp', 'shots');
const PORT = Number(process.env.MOCK_PORT ?? 4310);
const ORIGIN = `http://127.0.0.1:${PORT}`;
const CHROMIUM = process.env.CHROMIUM ?? 'chromium';

const DESKTOP = { width: 1280, height: 900 };
const PHONE = { width: 390, height: 844, scale: 2, fullPage: true };
const LIST = '[data-test-app]';
const DEPLOYMENT = '[data-test-deployment]';

/**
 * name, path, what to wait for, and optionally: viewport, `dark`, `keys`
 * (pressed before the capture, to show keyboard focus), `click`, or
 * `settle` (how long to wait before the capture, in milliseconds).
 */
const SHOTS = [
  { name: 'list-healthy', path: '/?scenario=healthy', wait: LIST },
  { name: 'list-warnings', path: '/?scenario=warnings', wait: LIST },
  {
    name: 'list-healthy-phone',
    path: '/?scenario=healthy',
    wait: LIST,
    ...PHONE,
  },
  {
    name: 'list-warnings-phone',
    path: '/?scenario=warnings',
    wait: LIST,
    ...PHONE,
  },
  {
    name: 'list-empty',
    path: '/?scenario=empty',
    wait: '[data-test-empty]',
    themes: ['light'],
  },
  {
    name: 'list-error',
    path: '/?scenario=error',
    wait: '[data-test-error]',
    themes: ['light'],
  },
  {
    name: 'list-loading',
    path: '/?scenario=loading',
    wait: '[data-test-loading]',
    themes: ['light'],
  },
  // Keyboard: Tab from the top of the page until a row has focus.
  {
    name: 'list-focus-row',
    path: '/?scenario=warnings',
    wait: LIST,
    keys: tabs(13),
  },
  {
    name: 'list-focus-open',
    path: '/?scenario=warnings',
    wait: LIST,
    keys: tabs(14),
    themes: ['light'],
  },
  {
    name: 'shell-user-menu',
    path: '/?scenario=healthy',
    wait: LIST,
    click: '[data-test-user-menu]',
    themes: ['light'],
  },
  {
    name: 'shell-user-menu-phone',
    path: '/?scenario=healthy',
    wait: LIST,
    click: '[data-test-user-menu]',
    ...PHONE,
    fullPage: false,
    themes: ['dark'],
  },
  // App detail header and the deploy tab.
  {
    name: 'deploy-history',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: DEPLOYMENT,
  },
  {
    name: 'deploy-history-open',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: DEPLOYMENT,
    click: '[data-test-show-log]',
    fullPage: true,
  },
  {
    name: 'deploy-running',
    path: '/apps/blog-staging/deploy?scenario=warnings',
    wait: '[data-test-step="build"]',
    fullPage: true,
  },
  {
    name: 'deploy-failed',
    path: '/apps/shop-staging/deploy?scenario=warnings',
    wait: '[data-test-rollback-notice]',
    fullPage: true,
  },
  {
    name: 'deploy-empty',
    path: '/apps/notes/deploy?scenario=undeployed',
    wait: '[data-test-no-deployments]',
  },
  {
    name: 'deploy-confirm',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: DEPLOYMENT,
    click: '[data-test-deploy-submit]',
  },
  {
    name: 'deploy-history-phone',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: DEPLOYMENT,
    ...PHONE,
  },
  {
    name: 'deploy-running-phone',
    path: '/apps/blog-staging/deploy?scenario=warnings',
    wait: '[data-test-step="build"]',
    ...PHONE,
  },
  {
    name: 'deploy-failed-phone',
    path: '/apps/shop-staging/deploy?scenario=warnings',
    wait: '[data-test-rollback-notice]',
    ...PHONE,
  },
  {
    name: 'deploy-empty-phone',
    path: '/apps/notes/deploy?scenario=undeployed',
    wait: '[data-test-no-deployments]',
    ...PHONE,
    themes: ['light'],
  },
  {
    name: 'deploy-confirm-phone',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: DEPLOYMENT,
    click: '[data-test-deploy-submit]',
    ...PHONE,
    fullPage: false,
    themes: ['dark'],
  },
  // Keyboard: Tab from the top of the page to the first history row.
  {
    name: 'deploy-focus-row',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: DEPLOYMENT,
    keys: tabs(18),
  },
  {
    name: 'detail-error',
    path: '/apps/blog/deploy?scenario=error',
    wait: '[data-test-app-error]',
    themes: ['light'],
  },
  // Screens that are not redesigned yet, to check they sit well in the shell.
  {
    name: 'other-login',
    path: '/?scenario=login',
    wait: '[data-test-login-link]',
    themes: ['light'],
  },
  {
    name: 'other-overview',
    path: '/apps/blog?scenario=healthy',
    wait: '[data-test-services]',
    themes: ['light'],
  },
  {
    name: 'other-system',
    path: '/system?scenario=warnings',
    wait: '.card',
    themes: ['light'],
  },
  {
    name: 'other-domains-phone',
    path: '/apps/blog/domains?scenario=warnings',
    wait: '.domain-row',
    ...PHONE,
    themes: ['light'],
  },
  // Last, because it changes the scenario: a deployment started in the UI,
  // a few seconds in, with its log streaming.
  {
    name: 'deploy-live',
    path: '/apps/shop-staging/deploy?scenario=healthy',
    wait: DEPLOYMENT,
    click: '[data-test-deploy-submit]',
    settle: 6000,
    themes: ['light'],
  },
];

function tabs(count) {
  return Array.from({ length: count }, () => 'Tab');
}

const LOGS = '/apps/blog/logs';
const LOG_ROW = '[data-test-log-row]';
const PHONE_SCREEN = { ...PHONE, fullPage: false };

/** Sets a `<select>` the way a reader would. */
function choose(selector, value) {
  return `(() => {
    const control = document.querySelector(${JSON.stringify(selector)});
    control.value = ${JSON.stringify(value)};
    control.dispatchEvent(new Event('change', { bubbles: true }));
  })()`;
}

/** Moves the selection of the log list with the keyboard. */
const SELECT_BY_KEYBOARD = `(() => {
  const list = document.querySelector('[data-test-log-viewport]');
  list.focus();
  for (let i = 0; i < 4; i++) {
    list.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true }),
    );
  }
})()`;

/**
 * The log screen. `script` is evaluated in the page before the capture, for
 * states that a URL cannot express (a scroll position, a selected row).
 */
const LOG_SHOTS = [
  { name: 'logs-live', path: `${LOGS}?scenario=healthy`, wait: LOG_ROW },
  {
    name: 'logs-live-phone',
    path: `${LOGS}?scenario=healthy`,
    wait: LOG_ROW,
    ...PHONE_SCREEN,
  },
  {
    name: 'logs-detail',
    path: `${LOGS}?scenario=healthy`,
    wait: LOG_ROW,
    click: `${LOG_ROW}:nth-last-child(12)`,
  },
  {
    name: 'logs-detail-phone',
    path: `${LOGS}?scenario=healthy`,
    wait: LOG_ROW,
    click: `${LOG_ROW}:nth-last-child(6)`,
    ...PHONE_SCREEN,
    themes: ['light'],
  },
  {
    name: 'logs-keyboard',
    path: `${LOGS}?scenario=healthy&service=web`,
    wait: LOG_ROW,
    script: SELECT_BY_KEYBOARD,
    themes: ['dark'],
  },
  {
    name: 'logs-paused',
    path: `${LOGS}?scenario=healthy`,
    wait: LOG_ROW,
    script: `document.querySelector('[data-test-log-viewport]').scrollTop = 4000`,
    // Long enough for the mock to send a few new lines.
    settle: 5000,
    themes: ['light'],
  },
  {
    name: 'logs-nowrap',
    path: `${LOGS}?scenario=healthy`,
    wait: LOG_ROW,
    // The choice is remembered per browser; forget it for the next shots.
    script: `(() => {
      document.querySelector('[data-test-log-wrap]').click();
      localStorage.removeItem('paasd.logs.wrap');
    })()`,
    themes: ['dark'],
  },
  {
    name: 'logs-search',
    path: `${LOGS}?scenario=healthy&service=web&period=1h&q=Completed`,
    wait: LOG_ROW,
    themes: ['light'],
  },
  {
    name: 'logs-truncated',
    path: `${LOGS}?scenario=healthy&service=web&period=24h`,
    wait: '[data-test-log-truncated]',
  },
  {
    name: 'logs-custom-range',
    path: `${LOGS}?scenario=healthy`,
    wait: LOG_ROW,
    script: choose('[data-test-log-period]', 'custom'),
    themes: ['light'],
  },
  {
    name: 'logs-custom-range-phone',
    path: `${LOGS}?scenario=healthy`,
    wait: LOG_ROW,
    script: choose('[data-test-log-period]', 'custom'),
    ...PHONE_SCREEN,
    themes: ['dark'],
  },
  {
    name: 'logs-archives',
    path: `${LOGS}?scenario=healthy&period=archive`,
    wait: '[data-test-archive]',
  },
  {
    name: 'logs-archives-phone',
    path: `${LOGS}?scenario=healthy&period=archive`,
    wait: '[data-test-archive]',
    ...PHONE_SCREEN,
    themes: ['light'],
  },
  {
    name: 'logs-archives-no-storage',
    path: `${LOGS}?scenario=logs-no-storage&period=archive`,
    wait: '[data-test-archives-not-configured]',
  },
  {
    name: 'logs-empty',
    path: `${LOGS}?scenario=logs-empty`,
    wait: '[data-test-log-empty]',
  },
  {
    name: 'logs-empty-search',
    path: `${LOGS}?scenario=healthy&period=1h&q=no-such-text`,
    wait: '[data-test-log-empty]',
    themes: ['light'],
  },
  {
    name: 'logs-empty-phone',
    path: `${LOGS}?scenario=logs-empty`,
    wait: '[data-test-log-empty]',
    ...PHONE_SCREEN,
    themes: ['light'],
  },
  {
    name: 'logs-error',
    path: `${LOGS}?scenario=logs-error`,
    wait: '[data-test-log-error]',
  },
  {
    name: 'logs-error-search',
    path: `${LOGS}?scenario=logs-error&period=1h`,
    wait: '[data-test-log-error]',
    themes: ['light'],
  },
  {
    name: 'logs-loading',
    path: `${LOGS}?scenario=logs-loading&period=1h`,
    wait: '[data-test-loading]',
    themes: ['light'],
  },
];

/** Starts a process and resolves once its output matches `ready`. */
function start(command, args, ready, env = {}) {
  const child = spawn(command, args, {
    env: { ...process.env, ...env },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const match = new Promise((done, fail) => {
    let output = '';
    const onData = (chunk) => {
      output += chunk;
      const found = ready.exec(output);
      if (found) done(found);
    };
    child.stdout.on('data', onData);
    child.stderr.on('data', onData);
    child.on('error', fail);
    child.on('exit', (code) =>
      fail(new Error(`${command} exited with ${code}:\n${output}`)),
    );
  });
  return { child, match };
}

/** A minimal DevTools protocol client over the browser's WebSocket. */
async function connect(url) {
  // The global WebSocket needs Node 22.4; this tool only runs in the
  // development image, which is newer than the `engines` range of the app.
  // eslint-disable-next-line n/no-unsupported-features/node-builtins
  const socket = new WebSocket(url);
  await new Promise((done, fail) => {
    socket.addEventListener('open', done, { once: true });
    socket.addEventListener('error', fail, { once: true });
  });
  const pending = new Map();
  let lastId = 0;
  socket.addEventListener('message', (event) => {
    const message = JSON.parse(event.data);
    const waiting = pending.get(message.id);
    if (!waiting) return;
    pending.delete(message.id);
    if (message.error) waiting.fail(new Error(message.error.message));
    else waiting.done(message.result);
  });
  return {
    send(method, params = {}, sessionId) {
      const id = ++lastId;
      socket.send(JSON.stringify({ id, method, params, sessionId }));
      return new Promise((done, fail) => pending.set(id, { done, fail }));
    },
    close: () => socket.close(),
  };
}

async function capture(browser, shot, theme) {
  const {
    width,
    height,
    scale = 1,
    fullPage = false,
  } = {
    ...DESKTOP,
    ...shot,
  };
  const { targetId } = await browser.send('Target.createTarget', {
    url: 'about:blank',
  });
  const { sessionId } = await browser.send('Target.attachToTarget', {
    targetId,
    flatten: true,
  });
  const page = (method, params) => browser.send(method, params, sessionId);
  const evaluate = async (expression) =>
    (await page('Runtime.evaluate', { expression, returnByValue: true })).result
      .value;

  await page('Emulation.setDeviceMetricsOverride', {
    width,
    height,
    deviceScaleFactor: scale,
    mobile: width < 600,
  });
  await page('Emulation.setEmulatedMedia', {
    features: [{ name: 'prefers-color-scheme', value: theme }],
  });
  // `theme=system` clears a theme pinned by an earlier visit.
  const separator = shot.path.includes('?') ? '&' : '?';
  await page('Page.navigate', {
    url: `${ORIGIN}${shot.path}${separator}theme=system`,
  });

  const selector = JSON.stringify(shot.wait);
  for (let attempt = 0; ; attempt++) {
    if (await evaluate(`document.querySelector(${selector}) !== null`)) break;
    if (attempt > 100) throw new Error(`${shot.name}: ${shot.wait} not found`);
    await sleep(100);
  }
  if (shot.click) {
    await evaluate(
      `document.querySelector(${JSON.stringify(shot.click)}).click()`,
    );
  }
  if (shot.script) await evaluate(shot.script);
  for (const key of shot.keys ?? []) {
    const event = { key, code: key, windowsVirtualKeyCode: 9 };
    await page('Input.dispatchKeyEvent', { type: 'keyDown', ...event });
    await page('Input.dispatchKeyEvent', { type: 'keyUp', ...event });
  }
  await sleep(shot.settle ?? 300);

  const options = { format: 'png' };
  if (fullPage) {
    const { cssContentSize } = await page('Page.getLayoutMetrics');
    options.captureBeyondViewport = true;
    options.clip = {
      x: 0,
      y: 0,
      width,
      height: Math.max(height, Math.ceil(cssContentSize.height)),
      scale: 1,
    };
  }
  const { data } = await page('Page.captureScreenshot', options);
  const file = join(OUT, `${shot.name}-${theme}.png`);
  await writeFile(file, Buffer.from(data, 'base64'));
  await browser.send('Target.closeTarget', { targetId });
  return file;
}

const only = process.argv[2] ?? '';
const profile = await mkdtemp(join(tmpdir(), 'paasd-shots-'));
await mkdir(OUT, { recursive: true });

const mock = start(
  process.execPath,
  [join(HERE, 'server.mjs')],
  /mock paasd: http/,
  { MOCK_PORT: String(PORT) },
);
const chromium = start(
  CHROMIUM,
  [
    '--headless=new',
    '--no-sandbox',
    '--disable-gpu',
    '--hide-scrollbars',
    '--remote-debugging-port=0',
    `--user-data-dir=${profile}`,
    'about:blank',
  ],
  /DevTools listening on (ws:\/\/\S+)/,
);

try {
  await mock.match;
  const [, endpoint] = await chromium.match;
  const browser = await connect(endpoint);
  const shots = [...SHOTS, ...LOG_SHOTS];
  for (const shot of shots.filter((entry) => entry.name.startsWith(only))) {
    for (const theme of shot.themes ?? ['light', 'dark']) {
      console.log(await capture(browser, shot, theme));
    }
  }
  browser.close();
} finally {
  mock.child.kill();
  chromium.child.kill();
  // Chromium may still be writing to its profile for a moment.
  await sleep(300);
  await rm(profile, { recursive: true, force: true, maxRetries: 3 });
}
