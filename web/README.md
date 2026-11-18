# paas-console

Management UI of paasd, written in Ember + TypeScript. The production build
(`dist/`) is embedded into the `paasd` binary.

## Development

Nothing is installed on the host: run the commands inside the development
container with `bin/dev` from the repository root (see the top-level README).

```sh
bin/dev sh -c 'cd web && pnpm install --frozen-lockfile'
bin/dev sh -c 'cd web && pnpm start'   # http://localhost:4200, proxies /api and /auth to paasd on 127.0.0.1:8080
bin/dev sh -c 'cd web && pnpm lint && pnpm test'
bin/dev sh -c 'cd web && pnpm build'   # writes dist/
```

Set `PAASD_URL` to proxy to a different paasd address.

## Design system

- `app/styles/tokens.css` holds every design token as a CSS custom property:
  colours (a neutral scale, one accent, the five state colours), typography,
  spacing, radius, shadow and the focus ring. Components use the semantic
  tokens (`--color-text`, `--space-4`, ...) and never raw values.
- Light and dark: each colour is written once as `light-dark(<light>, <dark>)`
  and follows `color-scheme`. By default that is the OS setting; the theme
  switch in the top bar pins `light` or `dark` with `data-theme` on `<html>`
  and remembers it in `localStorage` (`app/services/theme.ts`).
- The stylesheets are imported from `app/app.ts` in cascade order: tokens,
  base, shared components, shell, then one file per screen.
- Shared components live in `app/components/`: `button`, `badge`,
  `environment-badge`, `status-indicator`, `icon`, `row-list`, `tabs`,
  `form-field`, `confirm-dialog`, `banner`, `empty-state`, `relative-time`,
  `copy-button`, `theme-switch`, `user-menu`.
- The deploy tab is built from `deployment-detail` (one deployment, live over
  SSE), `deployment-steps` (the vertical list of steps) and `step-log` (the
  log of a step, following its tail). `app/utils/deploy-steps.ts` turns the
  log into steps and reads the outcome of the automatic rollback from it.
- App detail layout: on the routes listed in `FULL_SIZE_ROUTES`
  (`app/templates/app.gts`) the root gets the class `app-detail-full`, the
  page drops the centred column and `.app-detail-body` fills the rest of the
  window (`app/styles/app-detail.css`).
- `tests/unit/styles/tokens-test.ts` checks that text colours keep WCAG AA
  contrast on their backgrounds in both themes.

## Mock API for design work

`mock/server.mjs` serves the built app and answers the API from fixtures, so
every state of a screen can be looked at without a host, Docker or GitHub. It
is plain Node (no dependencies), listens on `127.0.0.1` only, and is not part
of the production build or the embedded binary.

```sh
bin/dev sh -c 'cd web && pnpm build && node mock/server.mjs'
# then open http://127.0.0.1:4300/ in a browser on the host
```

The page URL selects what the API answers and, optionally, the theme:

| URL                     | Shows                                                                                                         |
| ----------------------- | ------------------------------------------------------------------------------------------------------------- |
| `/?scenario=healthy`    | four apps, everything fine (the default)                                                                      |
| `/?scenario=warnings`   | a deployment in progress, a failed one, a stopped app, a pending domain, disk above the limit, a stale backup |
| `/?scenario=empty`      | no apps yet                                                                                                   |
| `/?scenario=undeployed` | one app that was created and never deployed                                                                   |
| `/?scenario=error`      | every API call fails with 500                                                                                 |
| `/?scenario=loading`    | API calls take a minute to answer                                                                             |
| `/?scenario=login`      | not logged in                                                                                                 |
| `...&theme=dark`        | pins the theme (`light`, `dark`, or `system` to unpin)                                                        |

Further scenarios cover single screens (`host-trouble`, `unconfigured`,
`fresh`, `backups-down`); `/__mock` lists every scenario with what it shows.

The scenario is kept in a cookie, so it stays while navigating; changes made
in the UI (deploying, adding a domain, ...) are kept in memory until the mock
restarts or `/__mock/reset` is opened. `MOCK_PORT` changes the port and
`MOCK_SCENARIO` the default scenario.

For the tabs of the app detail there are also `tab-error` (apps load, but
everything a tab asks for fails with 500) and `tab-loading` (the same, taking
a minute). The app `notes` of `fresh` has no variables and no custom domains,
for the empty states. In `warnings`, the app `shop` has services in trouble (a
failing health check close to its memory limit, a crashed worker) and `blog`
a domain that is still waiting for its DNS record.

`mock/screenshots.mjs` takes the reference screenshots with the headless
Chromium of the development image (light and dark, desktop and 390px wide,
the empty / error / loading states, keyboard focus) and writes them to
`tmp/shots/`, which is git-ignored:

```sh
bin/dev sh -c 'cd web && pnpm build && node mock/screenshots.mjs'
```

The image has Noto Sans CJK for Japanese text; a real browser uses the system
fonts named in `--font-sans`.

### Log screen

The log tab (`/apps/<slug>/logs`) keeps its filters in the URL, which is also
how the mock and the screenshots reach its states:

| Query                         | Shows                                                                        |
| ----------------------------- | ---------------------------------------------------------------------------- |
| (none)                        | the live tail: the newest lines, followed as they arrive                     |
| `period=15m\|1h\|24h\|7d`     | a search over what the host keeps (14 days), for a span ending now           |
| `period=custom&from=...&to=…` | a search over a range; `from` / `to` are `datetime-local` values read as JST |
| `period=archive`              | the archive list (object storage), for what is older than the host keeps     |
| `service=web`, `q=error`      | one service (`_access` is the access log of the reverse proxy); a keyword    |

`mock/logs.mjs` generates the log as a function of time, so the tail, a search
and its next page agree with each other. It adds scenarios that change only the
log endpoints: `logs-empty` (nothing logged, nothing archived), `logs-error`
(the log endpoints answer 500), `logs-loading` (searches take a minute) and
`logs-no-storage` (object storage is not configured). Example:
`/apps/blog/logs?scenario=logs-no-storage&period=archive`.
