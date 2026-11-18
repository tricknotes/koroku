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
- The stylesheets are imported from `app/app.ts` in cascade order.
  `legacy.css` restyles the screens that are not redesigned yet and shrinks as
  they move to the shared components.
- Shared components live in `app/components/`: `button`, `badge`,
  `environment-badge`, `status-indicator`, `icon`, `row-list`, `tabs`,
  `form-field`, `confirm-dialog`, `banner`, `empty-state`, `relative-time`,
  `copy-button`, `theme-switch`, `user-menu`.
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

| URL                   | Shows                                                                                                         |
| --------------------- | ------------------------------------------------------------------------------------------------------------- |
| `/?scenario=healthy`  | four apps, everything fine (the default)                                                                      |
| `/?scenario=warnings` | a deployment in progress, a failed one, a stopped app, a pending domain, disk above the limit, a stale backup |
| `/?scenario=empty`    | no apps yet                                                                                                   |
| `/?scenario=error`    | every API call fails with 500                                                                                 |
| `/?scenario=loading`  | API calls take a minute to answer                                                                             |
| `/?scenario=login`    | not logged in                                                                                                 |
| `...&theme=dark`      | pins the theme (`light`, `dark`, or `system` to unpin)                                                        |

The scenario is kept in a cookie, so it stays while navigating; changes made
in the UI (deploying, adding a domain, ...) are kept in memory until the mock
restarts or `/__mock/reset` is opened. `MOCK_PORT` changes the port and
`MOCK_SCENARIO` the default scenario.

Three more scenarios exist for the tabs of the app detail (overview,
environment variables, domains):

- `fresh`: one app, `notes`, that was never deployed. It has no variables and
  no custom domains, and backups are not configured (the empty states).
- `tab-error`: apps load, but everything a tab asks for fails with 500.
- `tab-loading`: apps load, but everything a tab asks for takes a minute.

In `warnings`, the app `shop` has services in trouble (a failing health check
close to its memory limit, a crashed worker) and `blog` a domain that is still
waiting for its DNS record.

`mock/screenshots.mjs` takes the reference screenshots with the headless
Chromium of the development image (light and dark, desktop and 390px wide,
the empty / error / loading states, keyboard focus) and writes them to
`tmp/shots/`, which is git-ignored:

```sh
bin/dev sh -c 'cd web && pnpm build && node mock/screenshots.mjs'
```

The image has Noto Sans CJK for Japanese text; a real browser uses the system
fonts named in `--font-sans`.
