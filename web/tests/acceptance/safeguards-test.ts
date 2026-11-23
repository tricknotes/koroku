import { module, test } from 'qunit';
import {
  click,
  currentURL,
  fillIn,
  find,
  findAll,
  settled,
  visit,
  waitUntil,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  serveConsole,
  type Console,
} from 'koroku-console/tests/helpers/console';
import {
  SHA_A,
  installFakes,
  makeApp,
  makeDeployment,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';

const APP = '/api/apps/blog';
const DEPLOYMENTS = `${APP}/deployments`;

/** The buttons of the dialog while its action is on its way. */
function assertDialogBusy(assert: Assert, what: string): void {
  assert
    .dom('[data-test-dialog-confirm]')
    .isDisabled(`${what}: no second confirmation`)
    .hasAttribute('aria-busy', 'true')
    .hasText('実行中…');
  assert
    .dom('[data-test-dialog-cancel]')
    .isDisabled(`${what}: no way to walk off mid-action`);
  assert.true(
    (find('[data-test-dialog]') as HTMLDialogElement).matches(':modal'),
    `${what}: the page behind stays out of reach`,
  );
}

/**
 * Tries what an impatient reader would: the confirm button again, Enter in
 * the dialog's form, and Escape.
 */
function hammerDialog(): void {
  (find('[data-test-dialog-confirm]') as HTMLButtonElement).click();
  (find('[data-test-dialog] form') as HTMLFormElement).requestSubmit();
  find('[data-test-dialog]')?.dispatchEvent(
    new Event('cancel', { cancelable: true }),
  );
}

// What keeps a slip from becoming an outage, looked at while the request
// is still running: one click is one request, the button says it is busy,
// and nothing destructive can be started twice. The requests are held open
// by the fake API until the test answers them.
module('Acceptance | safeguards', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let state: Console;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    state = serveConsole(fakes);
  });

  /** The saved variables differ from the running release. */
  function pendingEnv(): void {
    fakes.api.on('GET', `${APP}/env`, () => ({
      vars: state.env.map((v, i) => ({
        ...v,
        pending: i === 0 ? 'changed' : null,
      })),
      removed: [],
      compared: true,
    }));
  }

  function production(): void {
    state.app = { ...state.app, environment: 'production' };
  }

  function sent(method: string, path: string): number {
    return fakes.api.callsTo(method, path).length;
  }

  function queued() {
    return makeDeployment({
      id: 43,
      status: 'queued',
      started_at: null,
      finished_at: null,
    });
  }

  module('deploy', function () {
    test('staging: the form and the history rows are locked while it starts', async function (assert) {
      const held = fakes.api.hold('POST', DEPLOYMENTS);
      await visit('/apps/blog/deploy');

      const deploying = click('[data-test-deploy-submit]');
      await waitUntil(() => find('[data-test-deploy-submit][aria-busy]'));

      assert.dom('[data-test-dialog]').doesNotExist('staging does not ask');
      assert.dom('[data-test-deploy-submit]').isDisabled().hasText('開始中…');
      assert
        .dom('[data-test-redeploy]:not(:disabled)')
        .doesNotExist('no rollback is started on top of it');
      assert.true(findAll('[data-test-redeploy]').length > 0);
      (find('[data-test-deploy-submit]') as HTMLButtonElement).click();

      held.release(queued());
      await deploying;

      assert.strictEqual(sent('POST', DEPLOYMENTS), 1, 'one deployment');
      assert
        .dom('[data-test-deploy-submit]')
        .isNotDisabled()
        .hasText('デプロイ');
    });

    test('production: the confirmation stays, busy, until the deployment is accepted', async function (assert) {
      production();
      const held = fakes.api.hold('POST', DEPLOYMENTS);
      await visit('/apps/blog/deploy');
      await click('[data-test-deploy-submit]');
      assert.strictEqual(sent('POST', DEPLOYMENTS), 0);

      const confirming = click('[data-test-dialog-confirm]');
      await waitUntil(() => find('[data-test-dialog-confirm][aria-busy]'));

      assertDialogBusy(assert, 'deploy');
      hammerDialog();

      state.deployments = [queued(), ...state.deployments];
      held.release(queued());
      await confirming;

      assert.strictEqual(sent('POST', DEPLOYMENTS), 1, 'one deployment');
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.dom('[data-test-deployment="43"]').exists();
    });

    test('production: a refused deployment closes the dialog and says why', async function (assert) {
      production();
      const held = fakes.api.hold('POST', DEPLOYMENTS);
      await visit('/apps/blog/deploy');
      await click('[data-test-deploy-submit]');

      const confirming = click('[data-test-dialog-confirm]');
      await waitUntil(() => find('[data-test-dialog-confirm][aria-busy]'));
      held.refuse(409, 'busy');
      await confirming;

      assert.dom('[data-test-dialog]').doesNotExist();
      assert.dom('[data-test-error]').includesText('ほかの処理が実行中です');
      assert.dom('[data-test-deploy-submit]').isNotDisabled();

      // Trying again asks again: the confirmation is per deployment.
      await click('[data-test-deploy-submit]');
      assert.dom('[data-test-dialog]').exists();
      assert.strictEqual(sent('POST', DEPLOYMENTS), 1);
    });

    test('production: every way to deploy asks first', async function (assert) {
      production();
      fakes.api.on('POST', DEPLOYMENTS, queued());
      pendingEnv();

      await visit('/apps/blog/deploy');
      await click('[data-test-deploy-submit]');
      assert.dom('[data-test-dialog]').exists('the deploy form');
      await click('[data-test-dialog-cancel]');

      await click('[data-test-deployment="41"] [data-test-redeploy]');
      assert.dom('[data-test-dialog]').exists('a history row');
      await click('[data-test-dialog-cancel]');

      await visit('/apps/blog/env');
      await click('[data-test-env-redeploy]');
      assert.dom('[data-test-dialog]').exists('undeployed variables');
      await click('[data-test-dialog-cancel]');

      assert.strictEqual(
        sent('POST', DEPLOYMENTS),
        0,
        'and nothing was deployed by looking at the dialogs',
      );
    });

    test('canceling a deployment cannot be sent twice', async function (assert) {
      const running = makeDeployment({
        id: 50,
        status: 'running',
        finished_at: null,
      });
      state.deployments = [running, ...state.deployments];
      state.app = { ...state.app, latest_deployment: running };
      const held = fakes.api.hold('POST', '/api/deployments/50/cancel');
      await visit('/apps/blog/deploy');

      const canceling = click('[data-test-deployment="50"] [data-test-cancel]');
      await waitUntil(() => find('[data-test-cancel]:disabled'));
      (find('[data-test-cancel]') as HTMLButtonElement).click();

      const canceled = makeDeployment({ id: 50, status: 'canceled' });
      state.deployments = [canceled, ...state.deployments.slice(1)];
      held.release(canceled);
      await canceling;

      assert.strictEqual(sent('POST', '/api/deployments/50/cancel'), 1);
      assert.dom('[data-test-cancel]').doesNotExist();
    });
  });

  module('environment variables', function () {
    const ROW = '[data-test-env-row]:nth-child(2)';

    test('saving a row locks it until the variable is stored', async function (assert) {
      const held = fakes.api.hold('PUT', `${APP}/env/RAILS_ENV`);
      await visit('/apps/blog/env');
      await click(`${ROW} [data-test-env-edit]`);
      await fillIn(`${ROW} [data-test-env-value-input]`, 'staging');

      const saving = click(`${ROW} [data-test-env-save]`);
      await waitUntil(() => find(`${ROW} [data-test-env-save][aria-busy]`));

      assert.dom(`${ROW} [data-test-env-save]`).isDisabled().hasText('保存中…');
      assert.dom(`${ROW} [data-test-env-cancel]`).isDisabled();
      assert.dom(`${ROW} [data-test-env-value-input]`).isDisabled();
      (find(`${ROW} [data-test-env-save]`) as HTMLButtonElement).click();

      held.release({
        vars: [state.env[0], { key: 'RAILS_ENV', value: 'staging' }],
      });
      await saving;

      assert.strictEqual(sent('PUT', `${APP}/env/RAILS_ENV`), 1, 'one save');
      assert.strictEqual(sent('POST', DEPLOYMENTS), 0);
      assert.dom(`${ROW} [data-test-env-value-input]`).doesNotExist();
    });

    test('deleting a row cannot be sent twice', async function (assert) {
      const held = fakes.api.hold('DELETE', `${APP}/env/RAILS_ENV`);
      await visit('/apps/blog/env');
      await click(`${ROW} [data-test-env-remove]`);

      const removing = click(`${ROW} [data-test-env-remove-confirm]`);
      await waitUntil(() =>
        find(`${ROW} [data-test-env-remove-confirm][aria-busy]`),
      );

      assert.dom(`${ROW} [data-test-env-remove-confirm]`).isDisabled();
      assert.dom(`${ROW} [data-test-env-remove-cancel]`).isDisabled();
      (
        find(`${ROW} [data-test-env-remove-confirm]`) as HTMLButtonElement
      ).click();

      held.release({ vars: [state.env[0]] });
      await removing;

      assert.strictEqual(sent('DELETE', `${APP}/env/RAILS_ENV`), 1);
      assert.dom('[data-test-env-row]').exists({ count: 1 });
    });

    test('production: redeploying stays in the dialog until the deployment starts', async function (assert) {
      production();
      pendingEnv();
      const deploy = fakes.api.hold('POST', DEPLOYMENTS);
      await visit('/apps/blog/env');
      await click('[data-test-env-redeploy]');

      const confirming = click('[data-test-dialog-confirm]');
      await waitUntil(() => find('[data-test-dialog-confirm][aria-busy]'));

      assertDialogBusy(assert, 'while the deployment starts');
      hammerDialog();

      deploy.release(queued());
      await confirming;

      assert.strictEqual(sent('POST', DEPLOYMENTS), 1);
      assert.deepEqual(fakes.api.callsTo('POST', DEPLOYMENTS)[0]?.body, {
        sha: SHA_A,
        ref: 'main',
      });
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.dom('[data-test-env-redeployed]').exists();
    });
  });

  module('delete, stop and restart', function () {
    test('deleting: the dialog and its choices are locked while the app is removed', async function (assert) {
      production();
      const held = fakes.api.hold('DELETE', APP);
      await visit('/apps/blog/settings');
      await click('[data-test-delete]');
      await click('[data-test-delete-volumes]');
      await fillIn('[data-test-dialog-input]', 'blog');

      const deleting = click('[data-test-dialog-confirm]');
      await waitUntil(() => find('[data-test-dialog-confirm][aria-busy]'));

      assertDialogBusy(assert, 'delete');
      assert
        .dom('[data-test-delete-volumes]')
        .isDisabled('the choice about the data cannot change underway')
        .isChecked();
      hammerDialog();

      held.release();
      await deleting;

      const calls = fakes.api.callsTo('DELETE', APP);
      assert.strictEqual(calls.length, 1, 'one delete');
      assert.deepEqual(calls[0]?.query, { volumes: 'true' });
      assert.strictEqual(currentURL(), '/');
    });

    test('deleting: the name has to be typed again after a refusal', async function (assert) {
      fakes.api.fail('DELETE', APP, 409, 'busy');
      await visit('/apps/blog/settings');
      await click('[data-test-delete]');
      await fillIn('[data-test-dialog-input]', 'blog');
      await click('[data-test-dialog-confirm]');
      assert.dom('[data-test-dialog]').doesNotExist();

      await click('[data-test-delete]');

      assert.dom('[data-test-dialog-input]').hasValue('');
      assert.dom('[data-test-dialog-confirm]').isDisabled();
      assert.strictEqual(sent('DELETE', APP), 1);
    });

    test('stopping: the confirmation is locked, and so are the other operations', async function (assert) {
      const held = fakes.api.hold('POST', `${APP}/stop`);
      await visit('/apps/blog/settings');
      await click('[data-test-action-menu]');
      await click('[data-test-control="stop"]');

      const stopping = click('[data-test-dialog-confirm]');
      await waitUntil(() => find('[data-test-dialog-confirm][aria-busy]'));

      assertDialogBusy(assert, 'stop');
      hammerDialog();

      state.app = { ...state.app, desired_state: 'stopped' };
      held.release(state.app);
      await stopping;

      assert.strictEqual(sent('POST', `${APP}/stop`), 1);
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.dom('[data-test-app-state]').hasText('停止');
    });

    test('restarting: one request, and no stop on top of it', async function (assert) {
      const held = fakes.api.hold('POST', `${APP}/restart`);
      await visit('/apps/blog/settings');

      await click('[data-test-action-menu]');
      const restarting = click('[data-test-control="restart"]');
      await waitUntil(() => find('[data-test-control-busy]'));

      await click('[data-test-action-menu]');
      assert.dom('[data-test-control="restart"]').isDisabled();
      assert.dom('[data-test-control="stop"]').isDisabled();
      (find('[data-test-control="restart"]') as HTMLButtonElement).click();

      held.release(state.app);
      await restarting;

      assert.strictEqual(sent('POST', `${APP}/restart`), 1);
      await click('[data-test-action-menu]');
      assert.dom('[data-test-control="restart"]').isNotDisabled();
      assert.dom('[data-test-control="stop"]').isNotDisabled();
    });

    test('saving the settings shows it is busy and sends one PATCH', async function (assert) {
      const held = fakes.api.hold('PATCH', APP);
      await visit('/apps/blog/settings');
      await fillIn('[data-test-field="healthcheck_path"]', '/up');

      const saving = click('[data-test-settings-save]');
      await waitUntil(() => find('[data-test-settings-save][aria-busy]'));

      assert.dom('[data-test-settings-save]').isDisabled().hasText('保存中…');
      (find('[data-test-settings-save]') as HTMLButtonElement).click();

      state.app = { ...state.app, healthcheck_path: '/up' };
      held.release(state.app);
      await saving;

      assert.strictEqual(sent('PATCH', APP), 1);
      assert.dom('[data-test-settings-saved]').exists();
    });
  });

  module('restore and backup', function () {
    const RESTORE = `${APP}/backups/restore`;

    test('restoring: locked until the volume is back', async function (assert) {
      production();
      const held = fakes.api.hold('POST', RESTORE);
      await visit('/apps/blog/backups');
      await click('[data-test-snapshot-restore]');
      await fillIn('[data-test-dialog-input]', 'blog');

      const restoring = click('[data-test-dialog-confirm]');
      await waitUntil(() => find('[data-test-dialog-confirm][aria-busy]'));

      assertDialogBusy(assert, 'restore');
      assert
        .dom('[data-test-snapshot-restore]')
        .isDisabled('no second restore behind the dialog');
      hammerDialog();

      held.release({ restored: true });
      await restoring;

      assert.strictEqual(sent('POST', RESTORE), 1, 'one restore');
      assert.deepEqual(fakes.api.callsTo('POST', RESTORE)[0]?.body, {
        snapshot_id: '4bba301e',
      });
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.dom('[data-test-restored]').exists();
      assert.dom('[data-test-snapshot-restore]').isNotDisabled();
    });

    test('restoring: only a volume snapshot can be restored, a dump is downloaded', async function (assert) {
      await visit('/apps/blog/backups');

      assert
        .dom('[data-test-snapshot="77aa00ff"] [data-test-snapshot-restore]')
        .doesNotExist();
      assert
        .dom('[data-test-snapshot="77aa00ff"] a[download]')
        .hasAttribute('href', `${APP}/backups/77aa00ff/download`);
      assert
        .dom('[data-test-snapshot="4bba301e"] [data-test-snapshot-restore]')
        .exists();
    });

    test('"run now" starts one backup, from the app tab and from the system page', async function (assert) {
      for (const url of ['/apps/blog/backups', '/system/jobs']) {
        fakes.api.calls = [];
        const held = fakes.api.hold('POST', '/api/backups');
        await visit(url);

        const starting = click('[data-test-backup-run]');
        await waitUntil(() => find('[data-test-backup-run]:disabled'));
        (find('[data-test-backup-run]') as HTMLButtonElement).click();

        held.release({
          id: 13,
          trigger: 'manual',
          status: 'running',
          started_at: '2026-11-18T03:20:00Z',
          finished_at: null,
          error: '',
        });
        await starting;

        assert.strictEqual(sent('POST', '/api/backups'), 1, url);
      }
    });
  });

  module('domains', function () {
    test('deleting a domain: locked, and the other rows wait', async function (assert) {
      const held = fakes.api.hold('DELETE', `${APP}/domains/3`);
      await visit('/apps/blog/settings?section=domains');
      await click('[data-test-domain="example.net"] [data-test-domain-delete]');
      assert.dom('[data-test-dialog]').includesText('example.net');

      const deleting = click('[data-test-dialog-confirm]');
      await waitUntil(() => find('[data-test-dialog-confirm][aria-busy]'));

      assertDialogBusy(assert, 'delete a domain');
      assert
        .dom(
          '[data-test-domain-delete]:not(:disabled), [data-test-domain-verify]:not(:disabled), [data-test-domain-primary-toggle]:not(:disabled)',
        )
        .doesNotExist('no other action on any row meanwhile');
      hammerDialog();

      state.domains = state.domains.filter((domain) => domain.id !== 3);
      held.release();
      await deleting;

      assert.strictEqual(sent('DELETE', `${APP}/domains/3`), 1);
      assert.dom('[data-test-dialog]').doesNotExist();
      assert.dom('[data-test-domain="example.net"]').doesNotExist();
      assert.dom('[data-test-domain="www.example.org"]').exists();
    });

    test('adding a domain sends one request', async function (assert) {
      const held = fakes.api.hold('POST', `${APP}/domains`);
      await visit('/apps/blog/settings?section=domains');
      await fillIn('[data-test-domain-hostname]', 'docs.example.org');

      const adding = click('[data-test-domain-add]');
      await waitUntil(() => find('[data-test-domain-add][aria-busy]'));

      assert.dom('[data-test-domain-add]').isDisabled().hasText('追加中…');
      (find('[data-test-domain-add]') as HTMLButtonElement).click();

      held.release({
        id: 4,
        hostname: 'docs.example.org',
        status: 'pending',
        is_primary: false,
        force_active: false,
        last_checked_at: null,
        last_error: '',
        records: [],
      });
      await adding;

      assert.strictEqual(sent('POST', `${APP}/domains`), 1);
    });
  });

  module('system and new apps', function () {
    test('pruning: the dialog and its choices are locked while Docker works', async function (assert) {
      const held = fakes.api.hold('POST', '/api/system/prune');
      await visit('/system');
      await click('[data-test-prune-open]');
      await click('[data-test-prune-builder]');

      const pruning = click('[data-test-dialog-confirm]');
      await waitUntil(() => find('[data-test-dialog-confirm][aria-busy]'));

      assertDialogBusy(assert, 'prune');
      assert.dom('[data-test-prune-images]').isDisabled();
      assert.dom('[data-test-prune-builder]').isDisabled();
      hammerDialog();

      held.release({ output: 'Total reclaimed space: 1GB' });
      await pruning;

      const calls = fakes.api.callsTo('POST', '/api/system/prune');
      assert.strictEqual(calls.length, 1, 'one prune');
      assert.deepEqual(calls[0]?.body, { images: true, builder: false });
      assert.dom('[data-test-dialog]').doesNotExist();
    });

    test('creating an app sends one request', async function (assert) {
      const held = fakes.api.hold('POST', '/api/apps');
      fakes.api
        .on('GET', '/api/apps/blog-two', makeApp({ slug: 'blog-two' }))
        .on('GET', '/api/apps/blog-two/deployments', { deployments: [] });
      await visit('/apps/new');
      await fillIn('[data-test-field="repo_full_name"]', 'octo/blog');
      await fillIn('[data-test-field="slug"]', 'blog-two');

      const creating = click('[data-test-submit]');
      await waitUntil(() => find('[data-test-submit][aria-busy]'));

      assert.dom('[data-test-submit]').isDisabled().hasText('作成中…');
      (find('[data-test-submit]') as HTMLButtonElement).click();

      held.release(makeApp({ slug: 'blog-two' }));
      await creating;

      assert.strictEqual(sent('POST', '/api/apps'), 1);
      assert.strictEqual(currentURL(), '/apps/blog-two/deploy');
    });
  });

  test('a dialog that was canceled leaves nothing behind', async function (assert) {
    production();
    await visit('/apps/blog/settings');

    await click('[data-test-delete]');
    await click('[data-test-delete-volumes]');
    await fillIn('[data-test-dialog-input]', 'blog');
    find('[data-test-dialog]')?.dispatchEvent(
      new Event('cancel', { cancelable: true }),
    );
    await settled();

    assert.dom('[data-test-dialog]').doesNotExist('Escape cancels');
    assert.dom('[data-test-delete]').isFocused('focus is back on the button');
    assert.strictEqual(sent('DELETE', APP), 0);

    await click('[data-test-delete]');

    assert.dom('[data-test-dialog-input]').hasValue('');
    assert.dom('[data-test-delete-volumes]').isNotChecked();
    assert.dom('[data-test-dialog-confirm]').isDisabled();
  });
});
