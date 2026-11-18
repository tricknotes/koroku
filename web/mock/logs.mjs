// Runtime logs of the mock API (see server.mjs): the live tail, the search
// over what the host keeps and the archive list. The log is a function of
// time, so the tail, a search and its next page all agree with each other.
// Nothing here is real: names, addresses and messages are placeholders.

const SECOND = 1000;
const HOUR = 60 * 60 * SECOND;
const DAY = 24 * HOUR;
const JST = 9 * HOUR;

/** One line every so often, for as long as the app has existed. */
const STEP = 1500;
/** What the host keeps; older logs only exist as archives. */
const RETENTION = 14 * DAY;
/** How far back the archives go. */
const ARCHIVE_DAYS = 30;
const ARCHIVE_INTERVAL = 4 * HOUR;
const SLOW = 60 * SECOND;

/** What each log scenario shows; listed by `GET /__mock`. */
export const LOG_SCENARIOS = {
  'logs-empty': 'the app has written no logs and nothing is archived',
  'logs-error': 'the log endpoints fail with 500',
  'logs-loading': 'log searches and the archive list take a minute',
  'logs-no-storage': 'object storage is not configured (no archives)',
};

const PATHS = ['/', '/posts', '/posts/42', '/posts/7/comments', '/up', '/feed'];
const CLIENTS = ['198.51.100.7', '198.51.100.23', '203.0.113.54'];
const AGENT =
  'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36';

/** A small deterministic hash, to vary the lines without randomness. */
function mix(n) {
  let value = (n ^ 0x9e3779b9) >>> 0;
  value = Math.imul(value ^ (value >>> 16), 0x85ebca6b) >>> 0;
  value = Math.imul(value ^ (value >>> 13), 0xc2b2ae35) >>> 0;
  return (value ^ (value >>> 16)) >>> 0;
}

function pick(list, seed) {
  return list[seed % list.length];
}

function accessLine(seed, at) {
  const path = pick(PATHS, seed >>> 3);
  return JSON.stringify({
    level: 'info',
    ts: at / 1000,
    logger: 'http.log.access',
    msg: 'handled request',
    request: {
      remote_ip: pick(CLIENTS, seed >>> 5),
      proto: 'HTTP/2.0',
      method: 'GET',
      host: 'blog.dev.example.com',
      uri: path,
      headers: { 'User-Agent': [AGENT], Accept: ['text/html'] },
    },
    duration: (5 + (seed % 90)) / 1000,
    size: 2048 + (seed % 30000),
    status: 200,
  });
}

/** A failed request with its stack trace: every 400 lines, 6 lines long. */
const FAILURE = [
  'Started GET "/posts/42/comments" for 198.51.100.23',
  'Processing by CommentsController#index as HTML',
  'ERROR -- : ActiveRecord::ConnectionTimeoutError (could not obtain a connection from the pool within 5.000 seconds (waited 5.002 seconds); all pooled connections were in use)',
  "  app/models/comment.rb:18:in 'Comment.recent'",
  "  app/controllers/comments_controller.rb:7:in 'CommentsController#index'",
  'Completed 500 Internal Server Error in 5012ms (ActiveRecord: 5002.4ms | Allocations: 3120)',
];

/** The line written at step `n` (time `n * STEP`). */
function lineAt(n) {
  const at = n * STEP;
  const time = new Date(at).toISOString();
  const burst = n % 400;
  if (burst < FAILURE.length) {
    return { service: 'web', time, line: FAILURE[burst] };
  }
  const seed = mix(n);
  const path = pick(PATHS, seed >>> 3);
  const ms = 4 + (seed % 60);
  switch (seed % 10) {
    case 0:
    case 1:
      return {
        service: 'web',
        time,
        line: `Started GET "${path}" for ${pick(CLIENTS, seed >>> 5)}`,
      };
    case 2:
      return {
        service: 'web',
        time,
        line: `Processing by ${path.startsWith('/posts') ? 'PostsController#show' : 'PagesController#home'} as HTML`,
      };
    case 3:
    case 4:
      return {
        service: 'web',
        time,
        line: `Completed 200 OK in ${ms}ms (Views: ${(ms * 0.6).toFixed(1)}ms | ActiveRecord: ${(ms * 0.2).toFixed(1)}ms | Allocations: ${3000 + (seed % 9000)})`,
      };
    case 5:
      return {
        service: 'worker',
        time,
        line: `Performed SessionCleanupJob (Job ID: ${(seed >>> 4).toString(16).padStart(8, '0')}) from Async(default) in ${ms}.8ms`,
      };
    case 6:
      return {
        service: 'db',
        time,
        line:
          seed % 3 === 0
            ? `LOG:  checkpoint complete: wrote ${seed % 40} buffers (0.1%); 0 WAL file(s) added, 0 removed, 0 recycled; write=1.204 s, sync=0.004 s, total=1.213 s`
            : 'LOG:  checkpoint starting: time',
      };
    default:
      return { service: '_access', time, line: accessLine(seed, at) };
  }
}

function matches(line, service, keyword) {
  if (service && line.service !== service) return false;
  return !keyword || line.line.includes(keyword);
}

/** The last `count` lines up to now, oldest first. */
function tail(count, service) {
  const lines = [];
  const last = Math.floor(Date.now() / STEP);
  for (let n = last; lines.length < count && n > last - 20 * count; n--) {
    const line = lineAt(n);
    if (matches(line, service, '')) lines.unshift(line);
  }
  return { lines, next: last + 1 };
}

function parseTime(value, fallback) {
  const time = value ? Date.parse(value) : NaN;
  return Number.isNaN(time) ? fallback : time;
}

/** The oldest `limit` matching lines of a range, as the API answers. */
function search(query) {
  const now = Date.now();
  const from = Math.max(parseTime(query.get('from'), 0), now - RETENTION);
  const to = Math.min(parseTime(query.get('to'), now), now);
  const service = query.get('service') ?? '';
  const keyword = query.get('q') ?? '';
  const limit = Number(query.get('limit') ?? 1000);

  const lines = [];
  let truncated = false;
  // A bounded scan: a rare keyword over two weeks would take too long.
  const end = Math.min(
    Math.floor((to - 1) / STEP),
    Math.ceil(from / STEP) + 400000,
  );
  for (let n = Math.ceil(from / STEP); n <= end; n++) {
    const line = lineAt(n);
    if (!matches(line, service, keyword)) continue;
    if (lines.length === limit) {
      truncated = true;
      break;
    }
    lines.push(line);
  }
  return { lines, truncated };
}

function pad(value) {
  return String(value).padStart(2, '0');
}

/** One archive per service and archive run, newest first. */
function archives(slug, service) {
  const latest = Math.floor(Date.now() / ARCHIVE_INTERVAL) * ARCHIVE_INTERVAL;
  const list = [];
  for (let n = 0; n < (ARCHIVE_DAYS * DAY) / ARCHIVE_INTERVAL; n++) {
    const at = latest - n * ARCHIVE_INTERVAL;
    // The key and the hour are in the host's time zone (JST).
    const local = new Date(at + JST);
    const day = `${local.getUTCFullYear()}/${pad(local.getUTCMonth() + 1)}/${pad(local.getUTCDate())}`;
    const hour = pad(local.getUTCHours());
    for (const name of ['web', 'worker', 'db', '_access']) {
      if (service && service !== name) continue;
      list.push({
        key: `logs/${slug}/${name}/${day}/${hour}.log.gz`,
        service: name,
        hour: `${day.replaceAll('/', '-')}T${hour}:00:00+09:00`,
        size: 9000 + (mix(n * 31 + name.length) % 400000),
      });
    }
  }
  return list;
}

function wait(ms) {
  return new Promise((done) => setTimeout(done, ms));
}

/**
 * Registers the log routes. `scenario` is the name from the page URL; the
 * `logs-*` ones change only what these routes answer.
 */
export function registerLogRoutes(route, { appOf, HttpError }) {
  const check = (context) => {
    appOf(context);
    if (context.scenario === 'logs-error') throw new HttpError(500, 'internal');
  };

  route('GET', '/api/apps/:slug/logs', (context) => {
    check(context);
    if (context.scenario === 'logs-empty') {
      return { sse: [], live: () => undefined };
    }
    const service = context.query.get('service') ?? '';
    const count = Number(context.query.get('tail') ?? 200);
    const recent = tail(count, service);
    let next = recent.next;
    return {
      sse: recent.lines.map((line) => JSON.stringify(line)),
      // One line per tick of the stream, in step with the history.
      live: () => {
        for (;;) {
          const line = lineAt(next++);
          if (matches(line, service, '')) return JSON.stringify(line);
        }
      },
    };
  });

  route('GET', '/api/apps/:slug/logs/history', async (context) => {
    check(context);
    if (context.scenario === 'logs-loading') await wait(SLOW);
    if (context.scenario === 'logs-empty') {
      return { lines: [], truncated: false };
    }
    return search(context.query);
  });

  route('GET', '/api/apps/:slug/logs/archives', async (context) => {
    check(context);
    if (context.scenario === 'logs-loading') await wait(SLOW);
    if (context.scenario === 'logs-no-storage') {
      throw new HttpError(503, 'not_configured');
    }
    if (context.scenario === 'logs-empty') return { archives: [] };
    return {
      archives: archives(context.params.slug, context.query.get('service')),
    };
  });

  route('GET', '/api/apps/:slug/logs/archives/url', (context) => {
    check(context);
    if (context.scenario === 'logs-no-storage') {
      throw new HttpError(503, 'not_configured');
    }
    return { url: `/__mock/download?name=${context.query.get('key') ?? ''}` };
  });
}
