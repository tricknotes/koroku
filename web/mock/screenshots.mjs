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
// Fills in the token dialog and submits it; the mock answers with a made-up
// value.
const CREATE_TOKEN = `(() => {
  const name = document.querySelector('[data-test-token-name-input]');
  name.value = 'ノート PC の Claude Code';
  name.dispatchEvent(new Event('input', { bubbles: true }));
  document.querySelector('[data-test-dialog-confirm]').click();
})()`;

/**
 * name, path, what to wait for, and optionally: viewport, `dark`, `keys`
 * (pressed before the capture, to show keyboard focus), `click` / `clicks`
 * (selectors, clicked in order), `script` and `then` (see `capture`),
 * `settle` (how long to wait before the capture, in milliseconds), or
 * `mutates` for a shot that changes what the mock answers (it is reset
 * afterwards).
 */
const SHOTS = [
  { name: 'list-healthy', path: '/?scenario=healthy', wait: LIST },
  { name: 'list-warnings', path: '/?scenario=warnings', wait: LIST },
  // Opening an app that takes a while: the loading substate of the app
  // detail (templates/app-loading), with the bar along the top edge.
  {
    name: 'app-loading',
    path: '/?scenario=app-loading',
    wait: LIST,
    click: '[data-test-app-link]',
    then: '[data-test-app-loading]',
    settle: 1500,
  },
  {
    name: 'app-loading-phone',
    path: '/?scenario=app-loading',
    wait: LIST,
    click: '[data-test-app-link]',
    then: '[data-test-app-loading]',
    settle: 1500,
    ...PHONE,
    fullPage: false,
  },
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
  // Security: sessions, login history and the re-login prompt.
  {
    name: 'security',
    path: '/security?scenario=healthy',
    wait: '[data-test-sessions]',
  },
  {
    name: 'security-phone',
    path: '/security?scenario=healthy',
    wait: '[data-test-sessions]',
    ...PHONE,
  },
  {
    name: 'security-stale',
    path: '/security?scenario=stale-login',
    wait: '[data-test-sessions]',
    themes: ['light'],
  },
  {
    name: 'security-revoke-confirm',
    path: '/security?scenario=healthy',
    wait: '[data-test-sessions]',
    click: '[data-test-revoke-others]',
    themes: ['light'],
  },
  // API tokens for Claude Code (ARCHITECTURE 10.5).
  {
    name: 'security-tokens-empty',
    path: '/security?scenario=empty',
    wait: '[data-test-no-tokens]',
  },
  {
    name: 'security-token-create',
    path: '/security?scenario=healthy',
    wait: '[data-test-tokens]',
    click: '[data-test-token-create]',
  },
  {
    name: 'security-token-created',
    path: '/security?scenario=healthy',
    wait: '[data-test-tokens]',
    click: '[data-test-token-create]',
    script: CREATE_TOKEN,
    then: '[data-test-token-value]',
    mutates: true,
  },
  {
    name: 'security-token-created-phone',
    path: '/security?scenario=healthy',
    wait: '[data-test-tokens]',
    click: '[data-test-token-create]',
    script: CREATE_TOKEN,
    then: '[data-test-token-value]',
    mutates: true,
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'security-token-revoke',
    path: '/security?scenario=healthy',
    wait: '[data-test-tokens]',
    click: '[data-test-token="3fa9c2d17b40e815"] [data-test-token-revoke]',
  },
  {
    name: 'security-reauth-dialog',
    path: '/apps/blog/backups?scenario=stale-login',
    wait: '[data-test-snapshot-download]',
    click: '[data-test-snapshot-download]',
  },
  {
    name: 'security-reauth-dialog-phone',
    path: '/apps/blog/backups?scenario=stale-login',
    wait: '[data-test-snapshot-download]',
    click: '[data-test-snapshot-download]',
    ...PHONE,
    fullPage: false,
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
  // Members and their roles (ARCHITECTURE 10.7): the owner's screen, and
  // what a member sees.
  {
    name: 'members',
    path: '/members?scenario=healthy',
    wait: '[data-test-members]',
  },
  {
    name: 'members-phone',
    path: '/members?scenario=healthy',
    wait: '[data-test-members]',
    ...PHONE,
  },
  {
    name: 'members-empty',
    path: '/members?scenario=empty',
    wait: '[data-test-no-members]',
  },
  {
    name: 'members-remove',
    path: '/members?scenario=healthy',
    wait: '[data-test-members]',
    click: '[data-test-member-remove]',
    themes: ['light'],
  },
  {
    name: 'members-menu',
    path: '/?scenario=healthy',
    wait: LIST,
    click: '[data-test-user-menu]',
    themes: ['light'],
  },
  {
    name: 'members-security-owner',
    path: '/security?scenario=healthy',
    wait: '[data-test-sessions]',
  },
  { name: 'member-list', path: '/?scenario=member', wait: LIST },
  {
    name: 'member-list-phone',
    path: '/?scenario=member',
    wait: LIST,
    ...PHONE,
  },
  {
    name: 'member-menu',
    path: '/?scenario=member',
    wait: LIST,
    click: '[data-test-user-menu]',
    themes: ['light'],
  },
  {
    name: 'member-view-deploy',
    path: '/apps/shop/deploy?scenario=member',
    wait: DEPLOYMENT,
  },
  {
    name: 'member-view-deploy-phone',
    path: '/apps/shop/deploy?scenario=member',
    wait: DEPLOYMENT,
    ...PHONE,
  },
  {
    name: 'member-deploy-env',
    path: '/apps/blog/env?scenario=member',
    wait: '[data-test-env-forbidden]',
  },
  {
    name: 'member-deploy-env-phone',
    path: '/apps/blog/env?scenario=member',
    wait: '[data-test-env-forbidden]',
    ...PHONE,
  },
  {
    name: 'member-view-settings',
    path: '/apps/shop/settings?scenario=member',
    wait: '[data-test-settings-form]',
    fullPage: true,
  },
  {
    name: 'member-manage-settings',
    path: '/apps/blog-staging/settings?scenario=member',
    wait: '[data-test-settings-form]',
    fullPage: true,
  },
  {
    name: 'member-manage-settings-phone',
    path: '/apps/blog-staging/settings?scenario=member',
    wait: '[data-test-settings-form]',
    ...PHONE,
  },
  {
    name: 'member-security',
    path: '/security?scenario=member',
    wait: '[data-test-sessions]',
    themes: ['light'],
  },
  // App detail header and the deploy tab.
  {
    name: 'deploy-history',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: DEPLOYMENT,
  },
  {
    // From the overview's recent deployments (or GitHub): the deployment
    // opens and the page scrolls to it. Viewport only, to show the position.
    name: 'deploy-linked',
    path: '/apps/blog/deploy?scenario=healthy&deployment=105',
    wait: '[data-test-deployment="105"] [aria-expanded="true"]',
    settle: 800,
  },
  {
    name: 'deploy-linked-phone',
    path: '/apps/blog/deploy?scenario=healthy&deployment=105',
    wait: '[data-test-deployment="105"] [aria-expanded="true"]',
    settle: 800,
    ...PHONE,
    fullPage: false,
    themes: ['light'],
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
  // The picked commit needs variables that are not saved: a warning, not a block.
  {
    name: 'deploy-env-warning',
    path: '/apps/blog/deploy?scenario=env-missing',
    wait: '[data-test-deploy-env-warning]',
  },
  {
    name: 'deploy-env-warning-phone',
    path: '/apps/blog/deploy?scenario=env-missing',
    wait: '[data-test-deploy-env-warning]',
    ...PHONE,
  },
  {
    name: 'deploy-env-confirm',
    path: '/apps/blog/deploy?scenario=env-missing',
    wait: '[data-test-deploy-env-warning]',
    clicks: ['[data-test-deploy-submit]'],
    settle: 800,
  },
  {
    name: 'deploy-env-confirm-phone',
    path: '/apps/blog/deploy?scenario=env-missing',
    wait: '[data-test-deploy-env-warning]',
    clicks: ['[data-test-deploy-submit]'],
    settle: 800,
    ...PHONE,
    fullPage: false,
    themes: ['light'],
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
  // Promotion of the paired staging release (ARCHITECTURE 9.7).
  {
    name: 'promote-reuse',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: '[data-test-promote]',
  },
  {
    name: 'promote-reuse-phone',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: '[data-test-promote]',
    ...PHONE,
  },
  {
    name: 'promote-rebuild',
    path: '/apps/shop/deploy?scenario=healthy',
    wait: '[data-test-promote]',
  },
  {
    name: 'promote-rebuild-phone',
    path: '/apps/shop/deploy?scenario=healthy',
    wait: '[data-test-promote]',
    ...PHONE,
  },
  {
    name: 'promote-unpaired',
    path: '/apps/blog/deploy?scenario=promote-unpaired',
    wait: '[data-test-promotion-unpaired]',
  },
  {
    name: 'promote-confirm',
    path: '/apps/shop/deploy?scenario=healthy',
    wait: '[data-test-promote]',
    click: '[data-test-promote]',
  },
  {
    name: 'promote-confirm-phone',
    path: '/apps/shop/deploy?scenario=healthy',
    wait: '[data-test-promote]',
    click: '[data-test-promote]',
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'promote-settings',
    path: '/apps/blog/settings?scenario=healthy',
    wait: '[data-test-field="promote_from"] option[value="blog-staging"]',
    fullPage: true,
  },
  // Keyboard: Tab from the top of the page to the first history row.
  {
    name: 'deploy-focus-row',
    path: '/apps/blog/deploy?scenario=healthy',
    wait: DEPLOYMENT,
    // The promotion panel of the production app adds its summary and button.
    keys: tabs(20),
  },
  {
    name: 'detail-error',
    path: '/apps/blog/deploy?scenario=error',
    wait: '[data-test-app-error]',
    themes: ['light'],
  },
  ...rollbackShots(),
  ...appTabShots(),
  // Last, because it changes the scenario: a deployment started in the UI,
  // a few seconds in, with its log streaming.
  {
    name: 'deploy-live',
    path: '/apps/shop-staging/deploy?scenario=healthy',
    wait: DEPLOYMENT,
    click: '[data-test-deploy-submit]',
    settle: 6000,
    mutates: true,
    themes: ['light'],
  },
];

/** Going back to a kept release from the deploy history (ARCHITECTURE 7.1). */
function rollbackShots() {
  const STAGING = '/apps/blog-staging/deploy?scenario=rollback';
  const PRODUCTION = '/apps/blog/deploy?scenario=rollback';
  const ROLLBACK = '[data-test-rollback]';
  // Rows, newest first: the rollback, a failed deploy, a kept release, the
  // running release, another kept one, and one that is not kept any more.
  const row = (n) => `${DEPLOYMENT}:nth-child(${n}) [data-test-show-log]`;
  return [
    { name: 'rollback-history', path: STAGING, wait: ROLLBACK },
    {
      name: 'rollback-history-phone',
      path: STAGING,
      wait: ROLLBACK,
      ...PHONE,
    },
    {
      name: 'rollback-confirm',
      path: STAGING,
      wait: ROLLBACK,
      click: ROLLBACK,
    },
    {
      name: 'rollback-confirm-production',
      path: PRODUCTION,
      wait: ROLLBACK,
      click: ROLLBACK,
    },
    {
      name: 'rollback-confirm-production-phone',
      path: PRODUCTION,
      wait: ROLLBACK,
      click: ROLLBACK,
      ...PHONE,
      fullPage: false,
    },
    {
      name: 'rollback-detail',
      path: STAGING,
      wait: ROLLBACK,
      click: row(1),
      then: '[data-test-step="done"]',
      fullPage: true,
    },
    {
      name: 'rollback-detail-phone',
      path: STAGING,
      wait: ROLLBACK,
      click: row(1),
      then: '[data-test-step="done"]',
      ...PHONE,
      themes: ['light'],
    },
    {
      name: 'rollback-kept',
      path: STAGING,
      wait: ROLLBACK,
      click: row(3),
      then: '[data-test-release-kept]',
      fullPage: true,
      themes: ['light'],
    },
    {
      name: 'rollback-unavailable',
      path: STAGING,
      wait: ROLLBACK,
      click: row(6),
      then: '[data-test-release-gone]',
      fullPage: true,
    },
    {
      name: 'rollback-unavailable-phone',
      path: STAGING,
      wait: ROLLBACK,
      click: row(6),
      then: '[data-test-release-gone]',
      ...PHONE,
      themes: ['light'],
    },
    // Production with a login older than 10 minutes: the re-login prompt.
    {
      name: 'rollback-reauth',
      path: '/apps/blog/deploy?scenario=stale-login',
      wait: ROLLBACK,
      clicks: [ROLLBACK, '[data-test-dialog-confirm]'],
      then: '[data-test-reauth-dialog]',
      themes: ['light'],
    },
    // Confirmed: the rollback runs, its log streams in.
    {
      name: 'rollback-live',
      path: STAGING,
      wait: ROLLBACK,
      clicks: [ROLLBACK, '[data-test-dialog-confirm]'],
      then: '[data-test-rollback-summary]',
      settle: 3000,
      mutates: true,
      fullPage: true,
      themes: ['light'],
    },
  ];
}

/** Overview, environment and domains tabs of the app detail. */
function appTabShots() {
  const SERVICES = '[data-test-services]';
  const ACTIVITY = '[data-test-activity-log] [data-test-activity]';
  const SCROLL_TO_ACTIVITY =
    "document.querySelector('[data-test-activity-log]').scrollIntoView()";
  const ENV = '[data-test-env-row]';
  const DOMAIN = '[data-test-domain-default]';
  const PENDING = '[data-test-domain-pending]';
  // Types into the bulk editor the way a paste would.
  const badDotenv = [
    '# pasted from a local file',
    'RAILS_ENV=production',
    'this line has no equals sign',
    '1BAD=value',
    'RAILS_ENV=staging',
    'GREETING="hello world"',
  ].join('\n');
  const typeBadDotenv = `(() => {
    const field = document.querySelector('[data-test-env-bulk-text]');
    field.value = ${JSON.stringify(badDotenv)};
    field.dispatchEvent(new Event('input', { bubbles: true }));
  })()`;

  return [
    // Overview
    {
      name: 'overview-healthy',
      path: '/apps/blog?scenario=healthy',
      wait: SERVICES,
    },
    {
      name: 'overview-healthy-phone',
      path: '/apps/blog?scenario=healthy',
      wait: SERVICES,
      ...PHONE,
    },
    {
      name: 'overview-unhealthy',
      path: '/apps/shop?scenario=warnings',
      wait: SERVICES,
    },
    {
      name: 'overview-unhealthy-phone',
      path: '/apps/shop?scenario=warnings',
      wait: SERVICES,
      ...PHONE,
      themes: ['light'],
    },
    {
      name: 'overview-failed',
      path: '/apps/shop-staging?scenario=warnings',
      wait: SERVICES,
    },
    {
      name: 'overview-deploying',
      path: '/apps/blog-staging?scenario=warnings',
      wait: SERVICES,
      themes: ['light'],
    },
    {
      name: 'overview-stopped',
      path: '/apps/wiki?scenario=warnings',
      wait: '[data-test-no-services]',
      themes: ['light'],
    },
    {
      name: 'overview-empty',
      path: '/apps/notes?scenario=undeployed',
      wait: '[data-test-overview-empty]',
    },
    {
      name: 'overview-error',
      path: '/apps/blog?scenario=tab-error',
      wait: '[data-test-error]',
      themes: ['light'],
    },
    {
      name: 'overview-loading',
      path: '/apps/blog?scenario=tab-loading',
      wait: '[data-test-overview]',
      themes: ['light'],
    },
    // Who did what (ARCHITECTURE 10.7), at the side of the overview.
    {
      name: 'overview-activity',
      path: '/apps/blog?scenario=healthy',
      wait: ACTIVITY,
      script: SCROLL_TO_ACTIVITY,
    },
    {
      name: 'overview-activity-more',
      path: '/apps/blog?scenario=healthy',
      wait: ACTIVITY,
      click: '[data-test-activity-more]',
      then: '[data-test-activity="app.created"]',
      fullPage: true,
    },
    {
      name: 'overview-activity-phone',
      path: '/apps/blog?scenario=healthy',
      wait: ACTIVITY,
      ...PHONE,
    },
    {
      name: 'overview-activity-empty',
      path: '/apps/shop?scenario=healthy',
      wait: '[data-test-activity-empty]',
      script: SCROLL_TO_ACTIVITY,
      themes: ['light'],
    },

    // Environment variables
    { name: 'env-masked', path: '/apps/blog/env?scenario=healthy', wait: ENV },
    // Without a recent login: keys only, and a way to log in again.
    {
      name: 'env-hidden',
      path: '/apps/blog/env?scenario=stale-login',
      wait: '[data-test-env-hidden]',
    },
    {
      name: 'env-hidden-phone',
      path: '/apps/blog/env?scenario=stale-login',
      wait: '[data-test-env-hidden]',
      ...PHONE,
      themes: ['light'],
    },
    {
      name: 'env-editing',
      path: '/apps/blog/env?scenario=healthy',
      wait: ENV,
      clicks: [
        '[data-test-env-row]:nth-child(2) [data-test-env-reveal]',
        '[data-test-env-row]:nth-child(3) [data-test-env-edit]',
        '[data-test-env-add]',
      ],
      script: `(() => {
        const set = (selector, value) => {
          const field = document.querySelector(selector);
          field.value = value;
          field.dispatchEvent(new Event('input', { bubbles: true }));
        };
        set('[data-test-env-row]:nth-child(3) [data-test-env-value-input]', '1111111111111111111111111111111111111111');
        set('[data-test-env-row]:nth-child(4) [data-test-env-key-input]', 'MAIL FROM');
        set('[data-test-env-row]:nth-child(4) [data-test-env-value-input]', 'noreply@example.org');
      })()`,
    },
    // Saved, but not deployed yet: marks on the rows and one way to redeploy.
    {
      name: 'env-pending',
      path: '/apps/blog/env?scenario=env-pending',
      wait: '[data-test-env-pending-banner]',
    },
    {
      name: 'env-pending-phone',
      path: '/apps/blog/env?scenario=env-pending',
      wait: '[data-test-env-pending-banner]',
      ...PHONE,
    },
    // Deleting asks in the row.
    {
      name: 'env-deleting',
      path: '/apps/blog/env?scenario=healthy',
      wait: ENV,
      clicks: ['[data-test-env-row]:nth-child(2) [data-test-env-remove]'],
    },
    {
      name: 'env-deleting-phone',
      path: '/apps/blog/env?scenario=healthy',
      wait: ENV,
      clicks: ['[data-test-env-row]:nth-child(2) [data-test-env-remove]'],
      ...PHONE,
      themes: ['light'],
    },
    {
      name: 'env-editing-phone',
      path: '/apps/blog/env?scenario=healthy',
      wait: ENV,
      clicks: [
        '[data-test-env-row]:nth-child(2) [data-test-env-reveal]',
        '[data-test-env-row]:nth-child(3) [data-test-env-edit]',
      ],
      ...PHONE,
    },
    {
      name: 'env-bulk',
      path: '/apps/blog/env?scenario=healthy',
      wait: ENV,
      clicks: ['[data-test-env-bulk]'],
      themes: ['light'],
    },
    {
      name: 'env-bulk-errors',
      path: '/apps/blog/env?scenario=healthy',
      wait: ENV,
      clicks: ['[data-test-env-bulk]'],
      script: typeBadDotenv,
    },
    {
      name: 'env-bulk-errors-phone',
      path: '/apps/blog/env?scenario=healthy',
      wait: ENV,
      clicks: ['[data-test-env-bulk]'],
      script: typeBadDotenv,
      ...PHONE,
      themes: ['light'],
    },
    {
      name: 'env-confirm-production',
      path: '/apps/blog/env?scenario=env-pending',
      wait: ENV,
      clicks: ['[data-test-env-redeploy]'],
      themes: ['light'],
    },
    // What the repository needs, compared with the saved variables.
    {
      name: 'env-reqs-missing',
      path: '/apps/blog/env?scenario=env-missing',
      wait: '[data-test-env-reqs-missing]',
    },
    {
      name: 'env-reqs-missing-phone',
      path: '/apps/blog/env?scenario=env-missing',
      wait: '[data-test-env-reqs-missing]',
      ...PHONE,
    },
    {
      name: 'env-reqs-add',
      path: '/apps/blog/env?scenario=env-missing',
      wait: '[data-test-env-reqs-missing]',
      clicks: ['[data-test-env-req="SMTP_HOST"] [data-test-env-req-add]'],
      themes: ['light'],
    },
    {
      name: 'env-reqs-optional',
      path: '/apps/blog/env?scenario=env-optional',
      wait: '[data-test-env-reqs-missing]',
    },
    {
      name: 'env-reqs-ok',
      path: '/apps/blog/env?scenario=healthy',
      wait: '[data-test-env-reqs-ok]',
      themes: ['light'],
    },
    {
      name: 'env-reqs-dotenv',
      path: '/apps/blog/env?scenario=env-dotenv',
      wait: '[data-test-env-reqs-unlisted]',
    },
    {
      name: 'env-reqs-dotenv-phone',
      path: '/apps/blog/env?scenario=env-dotenv',
      wait: '[data-test-env-reqs-unlisted]',
      ...PHONE,
      themes: ['light'],
    },
    {
      name: 'env-reqs-file-missing',
      path: '/apps/blog/env?scenario=env-unavailable',
      wait: '[data-test-env-reqs-unavailable]',
    },
    {
      name: 'env-reqs-private',
      path: '/apps/shop/env?scenario=env-unavailable',
      wait: '[data-test-env-reqs-unavailable]',
      themes: ['light'],
    },
    {
      // No GitHub App: the public repository is read anonymously.
      name: 'env-reqs-no-app-phone',
      path: '/apps/blog/env?scenario=unconfigured',
      wait: '[data-test-env-reqs-ok], [data-test-env-reqs-missing], [data-test-env-reqs-none]',
      ...PHONE,
      themes: ['dark'],
    },
    {
      name: 'env-empty',
      path: '/apps/notes/env?scenario=fresh',
      wait: '[data-test-env-empty]',
    },
    {
      name: 'env-error',
      path: '/apps/blog/env?scenario=tab-error',
      wait: '[data-test-error]',
      themes: ['light'],
    },
    // Keyboard: Tab into the list until a row action has focus.
    {
      name: 'env-focus',
      path: '/apps/blog/env?scenario=healthy',
      wait: ENV,
      keys: tabs(18),
      themes: ['light'],
    },

    // Domains
    {
      name: 'domains-active',
      path: '/apps/blog/domains?scenario=healthy',
      wait: DOMAIN,
    },
    {
      name: 'domains-pending',
      path: '/apps/blog/domains?scenario=warnings',
      wait: PENDING,
    },
    {
      name: 'domains-pending-phone',
      path: '/apps/blog/domains?scenario=warnings',
      wait: PENDING,
      ...PHONE,
    },
    {
      name: 'domains-empty',
      path: '/apps/notes/domains?scenario=fresh',
      wait: '[data-test-no-domains]',
    },
    {
      name: 'domains-invalid',
      path: '/apps/blog/domains?scenario=healthy',
      wait: DOMAIN,
      clicks: ['[data-test-domain-add]'],
      themes: ['light'],
    },
    {
      name: 'domains-delete',
      path: '/apps/blog/domains?scenario=healthy',
      wait: DOMAIN,
      clicks: [
        '[data-test-domain="www.example.org"] [data-test-domain-delete]',
      ],
      themes: ['light'],
    },
    {
      name: 'domains-error',
      path: '/apps/blog/domains?scenario=tab-error',
      wait: '[data-test-error]',
      themes: ['light'],
    },
  ];
}

// Backups tab.
const SNAPSHOT = '[data-test-snapshot]';
SHOTS.push(
  {
    name: 'backups-normal',
    path: '/apps/blog/backups?scenario=healthy',
    wait: SNAPSHOT,
    fullPage: true,
  },
  {
    name: 'backups-problem',
    path: '/apps/blog/backups?scenario=warnings',
    wait: SNAPSHOT,
  },
  {
    name: 'backups-running',
    path: '/apps/shop/backups?scenario=healthy',
    wait: SNAPSHOT,
    click: '[data-test-backup-run]',
    then: '[data-test-backup-run][aria-busy]',
    mutates: true,
    themes: ['light'],
  },
  {
    name: 'backups-restore-dialog',
    path: '/apps/blog/backups?scenario=healthy',
    wait: SNAPSHOT,
    click: '[data-test-snapshot-restore]',
  },
  {
    name: 'backups-empty',
    path: '/apps/notes/backups?scenario=fresh',
    wait: '[data-test-no-snapshots]',
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'backups-unconfigured',
    path: '/apps/blog/backups?scenario=unconfigured',
    wait: '[data-test-backups-unconfigured]',
    themes: ['light'],
  },
  {
    name: 'backups-error',
    path: '/apps/blog/backups?scenario=backups-down',
    wait: '[data-test-error]',
    themes: ['dark'],
  },
  // The volume list fails on its own; the rest of the tab stays usable.
  {
    name: 'backups-volumes-error',
    path: '/apps/blog/backups?scenario=volumes-down',
    wait: '[data-test-volumes-error]',
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'backups-normal-phone',
    path: '/apps/blog/backups?scenario=healthy',
    wait: SNAPSHOT,
    ...PHONE,
    themes: ['light'],
  },
  {
    name: 'backups-problem-phone',
    path: '/apps/blog/backups?scenario=warnings',
    wait: SNAPSHOT,
    ...PHONE,
    themes: ['dark'],
  },
);

// Backups tab of a staging app: copying the production database into it.
const COPY = '[data-test-db-copy]';
const COPY_DETAIL = '[data-test-copy-detail] [data-test-step]';
const scrollTo = (selector) =>
  `document.querySelector(${JSON.stringify(selector)}).scrollIntoView()`;
SHOTS.push(
  {
    name: 'dbcopy-normal',
    path: '/apps/blog-staging/backups?scenario=healthy',
    wait: COPY_DETAIL,
    script: scrollTo(COPY),
  },
  {
    name: 'dbcopy-normal-phone',
    path: '/apps/blog-staging/backups?scenario=healthy',
    wait: COPY_DETAIL,
    script: scrollTo(COPY),
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'dbcopy-confirm',
    path: '/apps/blog-staging/backups?scenario=healthy',
    wait: '[data-test-copy-run]:not([disabled])',
    click: '[data-test-copy-run]',
  },
  {
    name: 'dbcopy-confirm-phone',
    path: '/apps/blog-staging/backups?scenario=healthy',
    wait: '[data-test-copy-run]:not([disabled])',
    click: '[data-test-copy-run]',
    ...PHONE,
    fullPage: false,
    themes: ['light'],
  },
  {
    name: 'dbcopy-running',
    path: '/apps/blog-staging/backups?scenario=copy-running',
    wait: '[data-test-copy-detail] [data-state="running"]',
    script: scrollTo('[data-test-copy-detail]'),
  },
  {
    name: 'dbcopy-running-phone',
    path: '/apps/blog-staging/backups?scenario=copy-running',
    wait: '[data-test-copy-detail] [data-state="running"]',
    script: scrollTo('[data-test-copy-detail]'),
    ...PHONE,
    fullPage: false,
    themes: ['dark'],
  },
  {
    name: 'dbcopy-failed',
    path: '/apps/blog-staging/backups?scenario=copy-failed',
    wait: '[data-test-copy-failed]',
    script: scrollTo('[data-test-copy-failed]'),
  },
  {
    name: 'dbcopy-failed-phone',
    path: '/apps/blog-staging/backups?scenario=copy-failed',
    wait: '[data-test-copy-failed]',
    script: scrollTo('[data-test-copy-failed]'),
    ...PHONE,
    fullPage: false,
    themes: ['light'],
  },
  {
    name: 'dbcopy-settings-preset',
    path: '/apps/shop-staging/backups?scenario=healthy',
    wait: '[data-test-load-preset]',
    script: `(() => {
      ${choose('[data-test-load-preset]', 'mysql')};
      ${scrollTo('[data-test-copy-settings]')};
    })()`,
  },
  {
    name: 'dbcopy-dump-preset',
    path: '/apps/blog/backups?scenario=healthy',
    wait: '[data-test-dump-preset]',
    script: scrollTo('[data-test-dump-preset]'),
    themes: ['light'],
  },
);

// Automatic deployment on push (ARCHITECTURE 5.2): the settings section,
// the production confirmation and the mark in the deploy history.
const AUTO_DEPLOY = '[data-test-auto-deploy-group]';
const TURN_ON_AUTO_DEPLOY = `(() => {
  document.querySelector('[data-test-field="auto_deploy"]').click();
  setTimeout(() => document.querySelector('[data-test-settings-save]').click(), 300);
})()`;
SHOTS.push(
  {
    name: 'autodeploy-settings',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: '[data-test-webhook-url]',
    script: scrollTo(AUTO_DEPLOY),
  },
  {
    name: 'autodeploy-settings-phone',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: '[data-test-webhook-url]',
    script: scrollTo(AUTO_DEPLOY),
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'autodeploy-settings-off',
    path: '/apps/blog/settings?scenario=healthy',
    wait: AUTO_DEPLOY,
    script: scrollTo(AUTO_DEPLOY),
    themes: ['light'],
  },
  {
    name: 'autodeploy-secret-missing',
    path: '/apps/blog-staging/settings?scenario=webhook-unset',
    wait: '[data-test-webhook-secret-missing]',
    script: scrollTo(AUTO_DEPLOY),
  },
  {
    name: 'autodeploy-secret-missing-phone',
    path: '/apps/blog-staging/settings?scenario=webhook-unset',
    wait: '[data-test-webhook-secret-missing]',
    script: scrollTo(AUTO_DEPLOY),
    ...PHONE,
    fullPage: false,
    themes: ['dark'],
  },
  {
    name: 'autodeploy-production-confirm',
    path: '/apps/blog/settings?scenario=healthy',
    wait: AUTO_DEPLOY,
    script: TURN_ON_AUTO_DEPLOY,
    then: '[data-test-auto-deploy-confirm]',
  },
  {
    name: 'autodeploy-production-confirm-phone',
    path: '/apps/blog/settings?scenario=healthy',
    wait: AUTO_DEPLOY,
    script: TURN_ON_AUTO_DEPLOY,
    then: '[data-test-auto-deploy-confirm]',
    ...PHONE,
    fullPage: false,
  },
  // Waiting for an image workflow before deploying a push (ARCHITECTURE
  // 5.2): the field and its App setup step, a refused path, the
  // production confirmation and the failed run in the activity.
  {
    name: 'autodeploy-workflow',
    path: '/apps/blog-staging/settings?scenario=workflow-gated',
    wait: '[data-test-workflow-setup]',
    script: scrollTo(AUTO_DEPLOY),
  },
  {
    name: 'autodeploy-workflow-phone',
    path: '/apps/blog-staging/settings?scenario=workflow-gated',
    wait: '[data-test-workflow-setup]',
    script: scrollTo(AUTO_DEPLOY),
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'autodeploy-workflow-invalid',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: '[data-test-field="auto_deploy_workflow"]',
    script: `(() => {
      const field = document.querySelector('[data-test-field="auto_deploy_workflow"]');
      field.value = 'image.yml';
      field.dispatchEvent(new Event('input', { bubbles: true }));
      setTimeout(() => document.querySelector('[data-test-settings-save]').click(), 300);
      setTimeout(() => document.querySelector('${AUTO_DEPLOY}').scrollIntoView(), 800);
    })()`,
    then: '[data-test-field-group="auto_deploy_workflow"] [data-test-field-error]',
    themes: ['light'],
  },
  {
    name: 'autodeploy-workflow-production-confirm',
    path: '/apps/blog/settings?scenario=healthy',
    wait: AUTO_DEPLOY,
    script: `(() => {
      document.querySelector('[data-test-field="auto_deploy"]').click();
      setTimeout(() => {
        const field = document.querySelector('[data-test-field="auto_deploy_workflow"]');
        field.value = '.github/workflows/image.yml';
        field.dispatchEvent(new Event('input', { bubbles: true }));
      }, 300);
      setTimeout(() => document.querySelector('[data-test-settings-save]').click(), 600);
    })()`,
    then: '[data-test-auto-deploy-confirm]',
  },
  {
    name: 'autodeploy-workflow-activity',
    path: '/apps/blog-staging?scenario=workflow-gated',
    wait: '[data-test-activity="auto_deploy.skipped"]',
    script:
      "document.querySelector('[data-test-activity-log]').scrollIntoView()",
  },
  {
    name: 'autodeploy-workflow-activity-phone',
    path: '/apps/blog-staging?scenario=workflow-gated',
    wait: '[data-test-activity="auto_deploy.skipped"]',
    ...PHONE,
    themes: ['dark'],
  },
  {
    name: 'autodeploy-history',
    path: '/apps/blog-staging/deploy?scenario=healthy',
    wait: '[data-test-push-badge]',
  },
  {
    name: 'autodeploy-history-open',
    path: '/apps/blog-staging/deploy?scenario=healthy',
    wait: '[data-test-push-badge]',
    click: '[data-test-show-log]',
    then: '[data-test-push-trigger]',
    themes: ['light'],
  },
  {
    name: 'autodeploy-history-phone',
    path: '/apps/blog-staging/deploy?scenario=healthy',
    wait: '[data-test-push-badge]',
    ...PHONE,
  },
);

// The container registry credential on the settings tab (ARCHITECTURE
// 6.1): set (blog), not set (blog-staging), the form, a refused value, the
// removal, and a member who may only look.
const REGISTRY = '[data-test-registry]';
const BAD_REGISTRY = `(() => {
  const input = document.querySelector('[data-test-registry-field="registry"]');
  input.value = 'https://ghcr.io/octo';
  input.dispatchEvent(new Event('input', { bubbles: true }));
  const user = document.querySelector('[data-test-registry-field="username"]');
  user.value = 'octo-bot';
  user.dispatchEvent(new Event('input', { bubbles: true }));
  document.querySelector('[data-test-registry-save]').click();
  setTimeout(() => document.querySelector('${REGISTRY}').scrollIntoView(), 200);
})()`;
SHOTS.push(
  {
    name: 'registry-set',
    path: '/apps/blog/settings?scenario=healthy',
    wait: '[data-test-registry-readout]',
    script: scrollTo(REGISTRY),
  },
  {
    name: 'registry-set-phone',
    path: '/apps/blog/settings?scenario=healthy',
    wait: '[data-test-registry-readout]',
    script: scrollTo(REGISTRY),
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'registry-empty',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: '[data-test-registry-empty]',
    script: scrollTo(REGISTRY),
  },
  {
    name: 'registry-form',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: '[data-test-registry-empty]',
    click: '[data-test-registry-edit]',
    script: scrollTo(REGISTRY),
    then: '[data-test-registry-form]',
  },
  {
    name: 'registry-form-phone',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: '[data-test-registry-empty]',
    click: '[data-test-registry-edit]',
    script: scrollTo(REGISTRY),
    then: '[data-test-registry-form]',
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'registry-change',
    path: '/apps/blog/settings?scenario=healthy',
    wait: '[data-test-registry-readout]',
    click: '[data-test-registry-edit]',
    script: scrollTo(REGISTRY),
    then: '[data-test-registry-form]',
    themes: ['light'],
  },
  {
    name: 'registry-error',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: '[data-test-registry-empty]',
    click: '[data-test-registry-edit]',
    script: BAD_REGISTRY,
    then: `${REGISTRY} [data-test-field-error]`,
  },
  {
    name: 'registry-error-phone',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: '[data-test-registry-empty]',
    click: '[data-test-registry-edit]',
    script: BAD_REGISTRY,
    then: `${REGISTRY} [data-test-field-error]`,
    ...PHONE,
    fullPage: false,
    themes: ['dark'],
  },
  {
    name: 'registry-remove-confirm',
    path: '/apps/blog/settings?scenario=healthy',
    wait: '[data-test-registry-readout]',
    click: '[data-test-registry-remove]',
    then: '[data-test-dialog]',
  },
  {
    name: 'registry-member-view',
    path: '/apps/shop/settings?scenario=member',
    wait: '[data-test-registry-empty]',
    script: scrollTo(REGISTRY),
    themes: ['light'],
  },
);

// Settings tab.
const SETTINGS = '[data-test-settings-form]';
SHOTS.push(
  {
    name: 'settings-normal',
    path: '/apps/blog/settings?scenario=healthy',
    wait: SETTINGS,
    fullPage: true,
  },
  {
    name: 'settings-env-source',
    path: '/apps/blog/settings?scenario=env-dotenv',
    wait: '[data-test-env-source-group]',
    script: `document.querySelector('[data-test-env-source-group]').scrollIntoView()`,
  },
  {
    name: 'settings-env-source-phone',
    path: '/apps/blog/settings?scenario=env-dotenv',
    wait: '[data-test-env-source-group]',
    script: `document.querySelector('[data-test-env-source-group]').scrollIntoView()`,
    ...PHONE,
    fullPage: false,
    themes: ['light'],
  },
  {
    name: 'settings-env-requirements',
    path: '/apps/blog/settings?scenario=healthy',
    wait: '[data-test-settings-requirements]',
    script: `document.querySelector('[data-test-env-source-group]').scrollIntoView()`,
  },
  {
    name: 'settings-env-requirements-phone',
    path: '/apps/blog/settings?scenario=healthy',
    wait: '[data-test-settings-requirements]',
    script: `document.querySelector('[data-test-env-source-group]').scrollIntoView()`,
    ...PHONE,
    fullPage: false,
    themes: ['light'],
  },
  {
    name: 'settings-unsaved-dialog',
    path: '/apps/blog/settings?scenario=healthy',
    wait: SETTINGS,
    script: `(() => {
      const input = document.querySelector('[data-test-field="healthcheck_path"]');
      input.value = '/healthz';
      input.dispatchEvent(new Event('input', { bubbles: true }));
      setTimeout(() => document.querySelector('[data-test-header-deploy]').click(), 300);
    })()`,
    then: '[data-test-unsaved-dialog]',
  },
  {
    name: 'settings-unsaved-dialog-phone',
    path: '/apps/blog/settings?scenario=healthy',
    wait: SETTINGS,
    script: `(() => {
      const input = document.querySelector('[data-test-field="healthcheck_path"]');
      input.value = '/healthz';
      input.dispatchEvent(new Event('input', { bubbles: true }));
      setTimeout(() => document.querySelector('[data-test-header-deploy]').click(), 300);
    })()`,
    then: '[data-test-unsaved-dialog]',
    ...PHONE,
    fullPage: false,
    themes: ['light'],
  },
  {
    name: 'settings-relaxed',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: SETTINGS,
    click: '[data-test-field="relax_policy"]',
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'settings-stopped',
    path: '/apps/wiki/settings?scenario=warnings',
    wait: SETTINGS,
    fullPage: true,
    themes: ['dark'],
  },
  {
    name: 'settings-invalid',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: SETTINGS,
    script: `(() => {
      const input = document.querySelector('[data-test-field="memory_limit"]');
      input.value = '512 MB';
      input.dispatchEvent(new Event('input', { bubbles: true }));
      document.querySelector('[data-test-settings-save]').click();
    })()`,
    then: '[data-test-field-error]',
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'settings-delete-dialog',
    path: '/apps/blog/settings?scenario=healthy',
    wait: SETTINGS,
    click: '[data-test-delete]',
  },
  {
    name: 'settings-stop-dialog',
    path: '/apps/blog/settings?scenario=healthy',
    wait: SETTINGS,
    click: '[data-test-control="stop"]',
    themes: ['light'],
  },
  {
    name: 'settings-focus',
    path: '/apps/blog/settings?scenario=healthy',
    wait: SETTINGS,
    keys: tabs(16),
    themes: ['light'],
  },
  {
    name: 'settings-normal-phone',
    path: '/apps/blog/settings?scenario=healthy',
    wait: SETTINGS,
    ...PHONE,
    themes: ['light'],
  },
  {
    name: 'settings-stopped-phone',
    path: '/apps/wiki/settings?scenario=warnings',
    wait: SETTINGS,
    ...PHONE,
    themes: ['dark'],
  },
);

// Memory history in the settings (ARCHITECTURE 9.5). In `healthy`, blog's
// web is near its limit, db is fine and worker has a lot of headroom.
const MEMORY = '[data-test-memory-service]';
const toMemory = (service = '') =>
  `document.querySelector('[data-test-memory-history] ${
    service ? `[data-test-memory-service="${service}"]` : ''
  }').scrollIntoView()`;
const focusChart = (service, steps) => `(() => {
  const plot = document.querySelector('[data-test-memory-service="${service}"] [data-test-memory-chart-plot]');
  plot.scrollIntoView({ block: 'center' });
  plot.focus();
  for (let i = 0; i < ${steps}; i++) {
    plot.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true }));
  }
})()`;
SHOTS.push(
  {
    name: 'memory-normal',
    path: '/apps/blog/settings?scenario=healthy',
    wait: MEMORY,
    script: toMemory(),
  },
  {
    name: 'memory-near-limit',
    path: '/apps/blog/settings?scenario=healthy',
    wait: MEMORY,
    script: focusChart('web', 22),
    then: '[data-test-memory-readout] dl',
  },
  {
    name: 'memory-generous-7d',
    path: '/apps/blog/settings?scenario=healthy',
    wait: MEMORY,
    click: '[data-test-memory-range-option="7d"]',
    script: toMemory('worker'),
    settle: 600,
  },
  {
    name: 'memory-applied',
    path: '/apps/blog/settings?scenario=healthy',
    wait: MEMORY,
    clicks: [
      '[data-test-memory-apply="web"]',
      '[data-test-memory-apply="worker"]',
    ],
    script: `document.querySelector('[data-test-field-group="service_memory_limits"]').scrollIntoView()`,
    then: '[data-test-memory-applied]',
    themes: ['light'],
  },
  {
    name: 'memory-collecting',
    path: '/apps/notes/settings?scenario=fresh',
    wait: MEMORY,
    script: toMemory(),
  },
  {
    name: 'memory-empty',
    path: '/apps/notes/settings?scenario=undeployed',
    wait: '[data-test-memory-empty]',
    script: toMemory(),
    themes: ['light'],
  },
  {
    name: 'memory-error',
    path: '/apps/blog/settings?scenario=tab-error',
    wait: '[data-test-memory-history] [data-test-error]',
    script: toMemory(),
    themes: ['light'],
  },
  {
    name: 'memory-normal-phone',
    path: '/apps/blog/settings?scenario=healthy',
    wait: MEMORY,
    script: toMemory(),
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'memory-near-limit-phone',
    path: '/apps/blog/settings?scenario=healthy',
    wait: MEMORY,
    script: focusChart('web', 60),
    then: '[data-test-memory-readout] dl',
    ...PHONE,
    fullPage: false,
    themes: ['dark'],
  },
);

// Access protection (ARCHITECTURE 8.3), on the settings tab.
const PROTECTION = '[data-test-protection-group]';
const SHOW_PROTECTION = `document.querySelector('${PROTECTION}').scrollIntoView()`;
SHOTS.push(
  {
    name: 'protection-on',
    path: '/apps/shop-staging/settings?scenario=healthy',
    wait: PROTECTION,
    script: SHOW_PROTECTION,
  },
  {
    name: 'protection-on-phone',
    path: '/apps/shop-staging/settings?scenario=healthy',
    wait: PROTECTION,
    script: SHOW_PROTECTION,
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'protection-off',
    path: '/apps/blog-staging/settings?scenario=healthy',
    wait: PROTECTION,
    click: '[data-test-field="access_protection"]',
    script: SHOW_PROTECTION,
  },
  {
    name: 'protection-invalid',
    path: '/apps/shop-staging/settings?scenario=healthy',
    wait: PROTECTION,
    script: `(() => {
      document.querySelector('[data-test-exempt-add]').click();
      setTimeout(() => {
        const inputs = document.querySelectorAll('[data-test-exempt-path]');
        const input = inputs[inputs.length - 1];
        input.value = '/hooks/*';
        input.dispatchEvent(new Event('input', { bubbles: true }));
        document.querySelector('[data-test-settings-save]').click();
        setTimeout(() => document.querySelector('${PROTECTION}').scrollIntoView(), 50);
      }, 50);
    })()`,
    then: '[data-test-field-group="protection_exempt_paths"] [data-test-field-error]',
    themes: ['light'],
  },
  {
    name: 'protection-production-confirm',
    path: '/apps/blog/settings?scenario=healthy',
    wait: PROTECTION,
    clicks: [
      '[data-test-field="access_protection"]',
      '[data-test-settings-save]',
    ],
    then: '[data-test-protection-confirm]',
  },
  {
    name: 'protection-production-confirm-phone',
    path: '/apps/blog/settings?scenario=healthy',
    wait: PROTECTION,
    clicks: [
      '[data-test-field="access_protection"]',
      '[data-test-settings-save]',
    ],
    then: '[data-test-protection-confirm]',
    ...PHONE,
    fullPage: false,
    themes: ['light'],
  },
);

// System screen. The container breakdown answers last (docker stats).
const RESOURCES = '[data-test-container-group]';
SHOTS.push(
  {
    name: 'system-normal',
    path: '/system?scenario=healthy',
    wait: RESOURCES,
    fullPage: true,
  },
  {
    name: 'system-github',
    path: '/system?scenario=healthy',
    wait: '[data-test-github-guide]',
    script: `document.querySelector('[data-test-github-app]').scrollIntoView()`,
  },
  {
    name: 'system-github-phone',
    path: '/system?scenario=healthy',
    wait: '[data-test-github-guide]',
    script: `document.querySelector('[data-test-github-app]').scrollIntoView()`,
    ...PHONE,
    fullPage: false,
    themes: ['light'],
  },
  {
    name: 'system-problem',
    path: '/system?scenario=host-trouble',
    wait: RESOURCES,
    fullPage: true,
  },
  {
    name: 'system-unconfigured',
    path: '/system?scenario=unconfigured',
    wait: RESOURCES,
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'system-no-containers',
    path: '/system?scenario=empty',
    wait: '[data-test-containers-empty]',
    themes: ['light'],
  },
  {
    name: 'system-partial-error',
    path: '/system?scenario=backups-down',
    wait: '[data-test-backup-load-error]',
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'system-error',
    path: '/system?scenario=error',
    wait: '[data-test-error]',
    themes: ['dark'],
  },
  {
    name: 'system-loading',
    path: '/system?scenario=loading',
    wait: '[data-test-loading]',
    themes: ['light'],
  },
  {
    name: 'system-prune-dialog',
    path: '/system?scenario=host-trouble',
    wait: RESOURCES,
    click: '[data-test-prune-open]',
  },
  {
    name: 'system-pruned',
    path: '/system?scenario=healthy',
    wait: RESOURCES,
    script: `(() => {
      document.querySelector('[data-test-prune-open]').click();
      setTimeout(
        () => document.querySelector('[data-test-dialog-confirm]').click(),
        200,
      );
    })()`,
    then: '[data-test-pruned]',
    mutates: true,
    themes: ['light'],
  },
  {
    name: 'system-normal-phone',
    path: '/system?scenario=healthy',
    wait: RESOURCES,
    ...PHONE,
    themes: ['light'],
  },
  {
    name: 'system-problem-phone',
    path: '/system?scenario=host-trouble',
    wait: RESOURCES,
    ...PHONE,
    themes: ['dark'],
  },
);

// App creation, login, not found.
const REPOSITORY = '[data-test-field="repo_full_name"]';
const CHOOSE_REPOSITORY = `(() => {
  const select = document.querySelector('${REPOSITORY}');
  select.value = 'octo/blog';
  select.dispatchEvent(new Event('change', { bubbles: true }));
})()`;
SHOTS.push(
  {
    name: 'new-app-start',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    themes: ['light'],
  },
  {
    name: 'new-app-filled',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: CHOOSE_REPOSITORY,
    then: '[data-test-submit]',
    fullPage: true,
  },
  {
    name: 'new-app-branch',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: `(() => {
      ${CHOOSE_REPOSITORY};
      setTimeout(() => {
        const branch = document.querySelector('[data-test-field="default_branch"]');
        branch.value = 'feature/redis';
        branch.dispatchEvent(new Event('input', { bubbles: true }));
        branch.dispatchEvent(new Event('change', { bubbles: true }));
      }, 400);
    })()`,
    then: '[data-test-submit]',
    settle: 1200,
    fullPage: true,
  },
  {
    name: 'new-app-branch-phone',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    ...PHONE,
    script: `(() => {
      ${CHOOSE_REPOSITORY};
      setTimeout(() => {
        const branch = document.querySelector('[data-test-field="default_branch"]');
        branch.value = 'feature/redis';
        branch.dispatchEvent(new Event('input', { bubbles: true }));
        branch.dispatchEvent(new Event('change', { bubbles: true }));
      }, 400);
    })()`,
    then: '[data-test-submit]',
    settle: 1200,
    themes: ['light'],
  },
  {
    name: 'new-app-url',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: `(() => {
      document.querySelector('[data-test-repo-use-url]').click();
      setTimeout(() => {
        const url = document.querySelector('[data-test-field="repo_url"]');
        url.value = 'https://github.com/octo/oss-tool';
        url.dispatchEvent(new Event('input', { bubbles: true }));
        url.dispatchEvent(new Event('change', { bubbles: true }));
      }, 300);
    })()`,
    then: '[data-test-compose-path]',
    fullPage: true,
  },
  {
    name: 'new-app-url-phone',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: `(() => {
      document.querySelector('[data-test-repo-use-url]').click();
      setTimeout(() => {
        const url = document.querySelector('[data-test-field="repo_url"]');
        url.value = 'https://github.com/octo/oss-tool';
        url.dispatchEvent(new Event('input', { bubbles: true }));
        url.dispatchEvent(new Event('change', { bubbles: true }));
      }, 300);
    })()`,
    then: '[data-test-compose-path]',
    ...PHONE,
    themes: ['light'],
  },
  {
    name: 'new-app-compose-path',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: `(() => {
      ${CHOOSE_REPOSITORY};
      setTimeout(() => {
        const select = document.querySelector('[data-test-field="compose_path"]');
        select.value = select.options[select.options.length - 1].value;
        select.dispatchEvent(new Event('change', { bubbles: true }));
        setTimeout(() => {
          const input = document.querySelector('[data-test-field="compose_path_input"]');
          input.value = 'deploy/compose.production.yml';
          input.dispatchEvent(new Event('input', { bubbles: true }));
          input.dispatchEvent(new Event('change', { bubbles: true }));
        }, 200);
      }, 400);
    })()`,
    then: '[data-test-compose-use-candidates]',
    settle: 800,
    fullPage: true,
  },
  {
    name: 'new-app-compose-path-phone',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: `(() => {
      ${CHOOSE_REPOSITORY};
      setTimeout(() => {
        const select = document.querySelector('[data-test-field="compose_path"]');
        select.value = select.options[select.options.length - 1].value;
        select.dispatchEvent(new Event('change', { bubbles: true }));
        setTimeout(() => {
          const input = document.querySelector('[data-test-field="compose_path_input"]');
          input.value = 'deploy/compose.production.yml';
          input.dispatchEvent(new Event('input', { bubbles: true }));
          input.dispatchEvent(new Event('change', { bubbles: true }));
        }, 200);
      }, 400);
    })()`,
    then: '[data-test-compose-use-candidates]',
    settle: 800,
    ...PHONE,
    themes: ['light'],
  },
  {
    name: 'new-app-invalid',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: `(() => {
      ${CHOOSE_REPOSITORY};
      setTimeout(() => {
        const slug = document.querySelector('[data-test-field="slug"]');
        slug.value = 'Blog_1';
        slug.dispatchEvent(new Event('input', { bubbles: true }));
        document.querySelector('[data-test-submit]').click();
      }, 400);
    })()`,
    then: '[data-test-field-error]',
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'new-app-no-github',
    path: '/apps/new?scenario=unconfigured',
    wait: '[data-test-field="repo_url"]',
    themes: ['light'],
  },
  {
    name: 'new-app-error',
    path: '/apps/new?scenario=error',
    wait: '[data-test-error]',
    themes: ['dark'],
  },
  {
    name: 'new-app-loading',
    path: '/apps/new?scenario=loading',
    wait: '[data-test-loading]',
    themes: ['light'],
  },
  {
    name: 'new-app-filled-phone',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: CHOOSE_REPOSITORY,
    then: '[data-test-submit]',
    ...PHONE,
    themes: ['light'],
  },
  // Environment presets (ARCHITECTURE 9.8): a Rails repository.
  {
    name: 'new-app-env',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: CHOOSE_REPOSITORY,
    then: '[data-test-env-draft-row="SECRET_KEY_BASE"]',
    fullPage: true,
  },
  {
    name: 'new-app-env-revealed',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: `(() => {
      ${CHOOSE_REPOSITORY};
      setTimeout(() => {
        document
          .querySelector('[data-test-env-draft-row="SECRET_KEY_BASE"] [data-test-env-draft-reveal]')
          .click();
      }, 400);
    })()`,
    then: '[data-test-env-draft-row="SECRET_KEY_BASE"] [data-test-env-draft-value]',
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'new-app-env-invalid',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: `(() => {
      ${CHOOSE_REPOSITORY};
      setTimeout(() => {
        document.querySelector('[data-test-env-draft-add]').click();
        setTimeout(() => {
          const keys = document.querySelectorAll('[data-test-env-draft-key]');
          const key = keys[keys.length - 1];
          key.value = 'RAILS_ENV';
          key.dispatchEvent(new Event('input', { bubbles: true }));
          document.querySelector('[data-test-submit]').click();
        }, 200);
      }, 400);
    })()`,
    then: '[data-test-env-draft-error]',
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'new-app-env-none',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: `(() => {
      const select = document.querySelector('${REPOSITORY}');
      select.value = 'octo/wiki';
      select.dispatchEvent(new Event('change', { bubbles: true }));
    })()`,
    then: '[data-test-env-presets-none]',
    fullPage: true,
    themes: ['light'],
  },
  {
    name: 'new-app-env-reauth',
    path: '/apps/new?scenario=stale-login',
    wait: REPOSITORY,
    script: CHOOSE_REPOSITORY,
    then: '[data-test-env-presets-reauth]',
    fullPage: true,
  },
  {
    name: 'new-app-env-phone',
    path: '/apps/new?scenario=healthy',
    wait: REPOSITORY,
    script: CHOOSE_REPOSITORY,
    then: '[data-test-env-draft-row="SECRET_KEY_BASE"]',
    ...PHONE,
  },
  { name: 'login', path: '/?scenario=login', wait: '[data-test-login-link]' },
  {
    name: 'login-denied',
    path: '/?scenario=login&login_error=not_allowed',
    wait: '[data-test-login-error]',
  },
  {
    name: 'login-denied-phone',
    path: '/?scenario=login&login_error=not_allowed',
    wait: '[data-test-login-error]',
    ...PHONE,
    fullPage: false,
    themes: ['light'],
  },
  {
    name: 'login-focus',
    path: '/?scenario=login',
    wait: '[data-test-login-link]',
    keys: tabs(5),
    themes: ['dark'],
  },
  {
    name: 'not-found',
    path: '/nowhere?scenario=healthy',
    wait: '[data-test-not-found]',
    themes: ['light'],
  },
);

function tabs(count) {
  return Array.from({ length: count }, () => 'Tab');
}

const TASK_RUN = '[data-test-task-run]';
const RUN_OUTPUT = '[data-test-task-run-detail] [data-test-step-log]';

/** Tasks tab: registered tasks, the command form and the runs. */
const TASK_SHOTS = [
  {
    name: 'tasks-normal',
    path: '/apps/blog/tasks?scenario=healthy',
    wait: TASK_RUN,
    fullPage: true,
  },
  {
    name: 'tasks-normal-phone',
    path: '/apps/blog/tasks?scenario=healthy',
    wait: TASK_RUN,
    ...PHONE,
  },
  {
    name: 'tasks-open',
    path: '/apps/blog/tasks?scenario=healthy',
    wait: TASK_RUN,
    click: '[data-test-show-output]',
    then: RUN_OUTPUT,
    fullPage: true,
  },
  {
    name: 'tasks-running',
    path: '/apps/blog/tasks?scenario=tasks-running',
    wait: RUN_OUTPUT,
    settle: 2500,
    fullPage: true,
  },
  {
    name: 'tasks-running-phone',
    path: '/apps/blog/tasks?scenario=tasks-running',
    wait: RUN_OUTPUT,
    settle: 2500,
    ...PHONE,
  },
  {
    name: 'tasks-failed',
    path: '/apps/blog/tasks?scenario=tasks-failed',
    wait: '[data-test-run-notice="failed"]',
    then: RUN_OUTPUT,
    fullPage: true,
  },
  {
    name: 'tasks-failed-phone',
    path: '/apps/blog/tasks?scenario=tasks-failed',
    wait: '[data-test-run-notice="failed"]',
    then: RUN_OUTPUT,
    ...PHONE,
  },
  {
    name: 'tasks-empty',
    path: '/apps/shop-staging/tasks?scenario=tasks-failed',
    wait: '[data-test-no-tasks]',
  },
  {
    name: 'tasks-empty-phone',
    path: '/apps/shop-staging/tasks?scenario=tasks-failed',
    wait: '[data-test-no-tasks]',
    ...PHONE,
    themes: ['light'],
  },
  {
    name: 'tasks-not-deployed',
    path: '/apps/notes/tasks?scenario=undeployed',
    wait: '[data-test-tasks-not-deployed]',
    themes: ['light'],
  },
  {
    name: 'tasks-confirm',
    path: '/apps/blog/tasks?scenario=healthy',
    wait: '[data-test-run-task]',
    click: '[data-test-run-task]',
    then: '[data-test-dialog-command]',
  },
  {
    name: 'tasks-confirm-phone',
    path: '/apps/blog/tasks?scenario=healthy',
    wait: '[data-test-run-task]',
    click: '[data-test-run-task]',
    then: '[data-test-dialog-command]',
    ...PHONE,
    fullPage: false,
  },
  {
    name: 'tasks-editing',
    path: '/apps/blog/tasks?scenario=healthy',
    wait: '[data-test-edit-tasks]',
    click: '[data-test-edit-tasks]',
    then: '[data-test-task-editor]',
  },
  {
    name: 'tasks-editing-phone',
    path: '/apps/blog/tasks?scenario=healthy',
    wait: '[data-test-edit-tasks]',
    click: '[data-test-edit-tasks]',
    then: '[data-test-task-editor]',
    ...PHONE,
    themes: ['light'],
  },
  // Last, because it changes the scenario: a task started in the UI of a
  // staging app (no confirmation), its output streaming in.
  {
    name: 'tasks-live',
    path: '/apps/blog-staging/tasks?scenario=healthy',
    wait: '[data-test-run-task]',
    clicks: ['[data-test-run-task]'],
    then: RUN_OUTPUT,
    settle: 3000,
    mutates: true,
    themes: ['light'],
  },
];

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
      localStorage.removeItem('koroku.logs.wrap');
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
  for (const target of shot.clicks ?? (shot.click ? [shot.click] : [])) {
    await evaluate(`document.querySelector(${JSON.stringify(target)}).click()`);
    await sleep(100);
  }
  // `script` drives what a single click cannot (choosing an option, typing);
  // `then` is what has to be on screen before the capture.
  if (shot.script) await evaluate(shot.script);
  if (shot.then) {
    const expected = JSON.stringify(shot.then);
    for (let attempt = 0; ; attempt++) {
      if (await evaluate(`document.querySelector(${expected}) !== null`)) break;
      if (attempt > 100)
        throw new Error(`${shot.name}: ${shot.then} not found`);
      await sleep(100);
    }
  }
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
  // A shot that changed the mock's state leaves it as it found it.
  // eslint-disable-next-line n/no-unsupported-features/node-builtins
  if (shot.mutates) await fetch(`${ORIGIN}/__mock/reset`);
  await browser.send('Target.closeTarget', { targetId });
  return file;
}

const only = process.argv[2] ?? '';
const profile = await mkdtemp(join(tmpdir(), 'koroku-shots-'));
await mkdir(OUT, { recursive: true });

const mock = start(
  process.execPath,
  [join(HERE, 'server.mjs')],
  /mock koroku: http/,
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
  const shots = [...SHOTS, ...LOG_SHOTS, ...TASK_SHOTS];
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
