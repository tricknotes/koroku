// Memory usage history of the mock API (ARCHITECTURE 9.5): made-up 5-minute
// buckets per service, summarized the way `GET /api/apps/:slug/memory` does
// it in internal/api/memory.go.

const MINUTE = 60 * 1000;
const HOUR = 60 * MINUTE;
const DAY = 24 * HOUR;
const MIB = 1024 ** 2;
const BUCKET = 5 * MINUTE;

const RANGES = {
  '24h': { span: DAY, step: 5 * MINUTE },
  '7d': { span: 7 * DAY, step: 30 * MINUTE },
  '14d': { span: 14 * DAY, step: HOUR },
};

/**
 * How each service of the fixtures behaves: a daily wave around `base`
 * (MiB), noise, and a nightly batch that climbs by `batch`. `limit` is in
 * MiB. Production numbers; staging apps use a fraction of them.
 */
const PROFILES = {
  // Close to its limit during the nightly batch, and restarted once.
  web: { base: 330, wave: 40, noise: 12, batch: 140, limit: 512, restarts: 1 },
  db: { base: 380, wave: 25, noise: 8, batch: 30, limit: 1024 },
  // Uses a fraction of its limit all the time.
  worker: { base: 82, wave: 8, noise: 4, batch: 0, limit: 512 },
};

/** A small deterministic PRNG, so screenshots are stable. */
function random(seed) {
  let value = seed % 2147483647;
  if (value <= 0) value += 2147483646;
  return () => {
    value = (value * 48271) % 2147483647;
    return (value - 1) / 2147483646;
  };
}

function hash(text) {
  let h = 7;
  for (const char of text) h = (h * 31 + char.charCodeAt(0)) % 1000003;
  return h;
}

/** The buckets of one service, oldest first, over `days` up to now. */
function buckets(slug, service, profile, { days, scale, now }) {
  const next = random(hash(`${slug}/${service}`));
  const end = Math.floor(now / BUCKET) * BUCKET;
  const count = Math.round((days * DAY) / BUCKET);
  const out = [];
  const restartAt = profile.restarts ? count - 1 - 9 * 12 : -1;
  for (let i = 0; i < count; i++) {
    const start = end - (count - 1 - i) * BUCKET;
    // JST hour of the bucket: busy in the evening, the batch at 3 o'clock.
    const hour = (new Date(start).getUTCHours() + 9) % 24;
    const wave = Math.sin(((hour - 15) / 24) * 2 * Math.PI);
    const batch = hour === 3 ? profile.batch * (0.82 + next() * 0.15) : 0;
    const avg =
      (profile.base + profile.wave * wave + profile.noise * next()) * scale;
    const max = avg + (profile.noise * next() + batch) * scale;
    out.push({
      start,
      max: Math.round(max * MIB),
      avg: Math.round(avg * MIB),
      samples: 5,
      limit: profile.limit * MIB,
      restarts: i === restartAt ? profile.restarts : 0,
    });
  }
  return out;
}

function suggestion(list) {
  const current = list[list.length - 1].limit;
  let first = list.length - 1;
  while (first > 0 && list[first - 1].limit === current) first--;
  const basis = list.slice(first);
  const maxima = basis.map((b) => b.max).sort((a, b) => a - b);
  const peak = maxima[maxima.length - 1];
  const p95 = maxima[Math.ceil(0.95 * maxima.length) - 1];
  const ready = basis.length >= 24 * 12;
  let limitBytes = null;
  if (ready) {
    const step = 64 * MIB;
    limitBytes = Math.max(128 * MIB, Math.ceil((peak * 1.5) / step) * step);
  }
  let flag = null;
  if (current) {
    if (peak >= current * 0.9) flag = 'near_limit';
    else if (basis.length >= 7 * 24 * 12 && peak < current * 0.3) {
      flag = 'generous';
    }
  }
  return {
    basis_since: new Date(basis[0].start).toISOString(),
    basis_hours: Math.round((basis.length * 10) / 12) / 10,
    peak_bytes: peak,
    p95_bytes: p95,
    ready,
    limit_bytes: limitBytes,
    limit: limitBytes === null ? null : `${limitBytes / MIB}m`,
    flag,
  };
}

function summarize(name, list, from, step) {
  const points = [];
  let peak = 0;
  let restarts = 0;
  let sum = 0;
  let samples = 0;
  for (const b of list) {
    if (b.start < from) continue;
    const at = Math.floor(b.start / step) * step;
    let point = points[points.length - 1];
    if (!point || point.at !== at) {
      if (point) point.avg_bytes = Math.round(sum / samples);
      point = { at, max_bytes: 0, avg_bytes: 0, limit_bytes: null };
      points.push(point);
      sum = samples = 0;
    }
    point.max_bytes = Math.max(point.max_bytes, b.max);
    point.limit_bytes = Math.max(point.limit_bytes ?? 0, b.limit);
    sum += b.avg * b.samples;
    samples += b.samples;
    peak = Math.max(peak, b.max);
    restarts += b.restarts;
  }
  if (points.length === 0) return null;
  points[points.length - 1].avg_bytes = Math.round(sum / samples);
  return {
    name,
    limit_bytes: list[list.length - 1].limit,
    peak_bytes: peak,
    restarts,
    points: points.map((p) => ({ ...p, at: new Date(p.at).toISOString() })),
    suggestion: suggestion(list),
  };
}

/**
 * `GET /api/apps/:slug/memory`. `collectingHours` makes every service
 * young (the `fresh` scenario).
 */
export function memoryHistory(target, rangeName, options = {}) {
  const range = RANGES[rangeName];
  const now = Date.now();
  const from = now - range.span;
  const result = {
    range: rangeName,
    step_seconds: range.step / 1000,
    from: new Date(from).toISOString(),
    to: new Date(now).toISOString(),
    services: [],
  };
  if (!target.current_deployment) return result;
  const scale = target.environment === 'production' ? 1 : 0.35;
  const days = options.collectingHours ? options.collectingHours / 24 : 14;
  const names = options.collectingHours ? ['web'] : Object.keys(PROFILES);
  for (const name of names.sort()) {
    const list = buckets(target.slug, name, PROFILES[name], {
      days,
      scale: name === 'web' || name === 'worker' ? scale : Math.max(scale, 0.6),
      now,
    });
    const service = summarize(name, list, from, range.step);
    if (service) result.services.push(service);
  }
  return result;
}
