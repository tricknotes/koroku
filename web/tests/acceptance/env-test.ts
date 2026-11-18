import { module, test } from 'qunit';
import {
  click,
  currentURL,
  fillIn,
  findAll,
  visit,
  waitFor,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'paas-console/tests/helpers';
import {
  SHA_A,
  installFakes,
  makeApp,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'paas-console/tests/helpers/fakes';
import type { App } from 'paas-console/utils/types';

const SECRET = 'postgres://app:hunter2@db/app';

const ENV = '/api/apps/blog/env';
const DEPLOYMENTS = '/api/apps/blog/deployments';

function row(n: number, selector = ''): string {
  return `[data-test-env-row]:nth-child(${n}) ${selector}`.trim();
}

/** Everything on the page as text and attributes. */
function pageHtml(): string {
  return document.querySelector('#ember-testing')?.innerHTML ?? '';
}

module('Acceptance | environment variables', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const running = makeDeployment();
    app = makeApp({ current_deployment: running, latest_deployment: running });
    fakes.api
      .on('GET', '/api/apps/blog', () => app)
      .on('GET', ENV, {
        vars: [
          { key: 'DATABASE_URL', value: SECRET },
          { key: 'RAILS_ENV', value: 'production' },
        ],
      })
      .on('PUT', ENV, (call: FakeCall) => call.body)
      .on('POST', DEPLOYMENTS, () =>
        makeDeployment({
          id: 43,
          status: 'queued',
          started_at: null,
          finished_at: null,
        }),
      );
  });

  test('values are masked until revealed, one row or all at once', async function (assert) {
    await visit('/apps/blog/env');

    assert.dom('[data-test-env-row]').exists({ count: 2 });
    assert.dom(row(1, '[data-test-env-key]')).hasText('DATABASE_URL');
    assert.dom('[data-test-env-masked]').exists({ count: 2 });
    assert.dom('[data-test-env-value]').doesNotExist();
    assert.false(
      pageHtml().includes('hunter2'),
      'a masked value is not in the DOM at all',
    );
    assert
      .dom('[data-test-env-notice]')
      .includesText('次のデプロイで反映されます');
    assert.dom('[data-test-env-dirty]').doesNotExist();

    await click(row(1, '[data-test-env-reveal]'));

    assert.dom('[data-test-env-value]').exists({ count: 1 }).hasText(SECRET);
    assert.dom(row(1, '[data-test-env-reveal]')).hasAria('pressed', 'true');
    assert.dom('[data-test-env-masked]').exists({ count: 1 });
    assert
      .dom('[data-test-env-dirty]')
      .doesNotExist('looking at a value changes nothing');

    await click(row(1, '[data-test-env-reveal]'));
    assert.dom('[data-test-env-value]').doesNotExist();
    assert.false(pageHtml().includes('hunter2'));

    await click('[data-test-env-toggle-all]');
    assert.dom('[data-test-env-value]').exists({ count: 2 });
    assert.dom('[data-test-env-toggle-all]').hasText('すべて隠す');

    await click('[data-test-env-toggle-all]');
    assert.dom('[data-test-env-value]').doesNotExist();
  });

  test('a value is copied without being revealed', async function (assert) {
    await visit('/apps/blog/env');

    assert
      .dom(row(1, '[data-test-copy]'))
      .hasAria('label', 'DATABASE_URL の値をコピー');

    await click(row(1, '[data-test-copy]'));

    assert.deepEqual(fakes.browser.copied, [SECRET]);
    assert.dom('[data-test-env-masked]').exists({ count: 2 });
    assert.false(pageHtml().includes('hunter2'));
  });

  test('a value never appears in a label, a title or a URL', async function (assert) {
    await visit('/apps/blog/env');
    await click(row(1, '[data-test-env-reveal]'));
    await click(row(1, '[data-test-env-edit]'));
    await fillIn(row(1, '[data-test-env-value-input]'), `${SECRET}?x=1`);
    await click('[data-test-env-save]');

    const attributes = findAll(
      '#ember-testing [title], #ember-testing [aria-label]',
    ).map(
      (element) =>
        `${element.getAttribute('title')} ${element.getAttribute('aria-label')}`,
    );
    assert.false(attributes.join('\n').includes('hunter2'));
    assert.false(document.title.includes('hunter2'));
    assert.false(currentURL().includes('hunter2'));
    assert.false(
      fakes.api.calls.some((call) => call.path.includes('hunter2')),
      'values only travel in request bodies',
    );
    assert.dom('[data-test-env-saved]').doesNotIncludeText('hunter2');
  });

  test('edits, additions and removals are saved with one PUT', async function (assert) {
    await visit('/apps/blog/env');

    assert.dom('[data-test-env-save]').isDisabled('there is nothing to save');

    // Edit the first value.
    await click(row(1, '[data-test-env-edit]'));
    assert.dom(row(1, '[data-test-env-key-input]')).hasValue('DATABASE_URL');
    assert.dom(row(1, '[data-test-env-value-input]')).hasValue(SECRET);
    await fillIn(
      row(1, '[data-test-env-value-input]'),
      'postgres://app:changed@db/app',
    );
    await click(row(1, '[data-test-env-edit]'));
    assert.dom(row(1, '[data-test-env-status]')).hasText('変更');
    assert
      .dom(row(1, '[data-test-env-masked]'))
      .exists('done editing: the value is hidden again');

    // Remove the second variable.
    await click(row(2, '[data-test-env-remove]'));

    // Add a new one; an untouched extra row is ignored.
    await click('[data-test-env-add]');
    assert
      .dom(row(2, '[data-test-env-key-input]'))
      .isFocused('a new row is ready to type in');
    await fillIn(row(2, '[data-test-env-key-input]'), 'SECRET_KEY_BASE');
    await fillIn(row(2, '[data-test-env-value-input]'), 'abc123');
    await click(row(2, '[data-test-env-edit]'));
    assert.dom(row(2, '[data-test-env-status]')).hasText('新規');
    await click('[data-test-env-add]');

    assert
      .dom('[data-test-env-dirty]')
      .includesText('未保存の変更があります')
      .includesText('追加 1 件・変更 1 件・削除 1 件');
    assert.deepEqual(fakes.api.callsTo('PUT', ENV), []);

    await click('[data-test-env-save]');

    const puts = fakes.api.callsTo('PUT', ENV);
    assert.strictEqual(puts.length, 1, 'everything goes in a single request');
    assert.deepEqual(puts[0]?.body, {
      vars: [
        { key: 'DATABASE_URL', value: 'postgres://app:changed@db/app' },
        { key: 'SECRET_KEY_BASE', value: 'abc123' },
      ],
    });
    assert
      .dom('[data-test-env-saved]')
      .includesText('保存しました。次のデプロイで反映されます。');
    assert.dom('[data-test-env-dirty]').doesNotExist();
    assert.dom('[data-test-env-status]').doesNotExist();
    assert.dom('[data-test-env-row]').exists({ count: 2 });
    assert
      .dom('[data-test-env-masked]')
      .exists({ count: 2 }, 'saved values are masked again');
    assert.deepEqual(
      fakes.api.callsTo('POST', DEPLOYMENTS),
      [],
      'saving alone deploys nothing',
    );
  });

  test('discarding puts back what is saved', async function (assert) {
    await visit('/apps/blog/env');

    await click(row(1, '[data-test-env-remove]'));
    await click('[data-test-env-add]');
    await fillIn(row(2, '[data-test-env-key-input]'), 'NEW');
    assert.dom('[data-test-env-dirty]').exists();

    await click('[data-test-env-discard]');

    assert.dom('[data-test-env-dirty]').doesNotExist();
    assert.dom('[data-test-env-discard]').doesNotExist();
    assert.dom('[data-test-env-row]').exists({ count: 2 });
    assert.dom(row(1, '[data-test-env-key]')).hasText('DATABASE_URL');
    assert.deepEqual(fakes.api.callsTo('PUT', ENV), []);
  });

  test('invalid and duplicate keys block saving', async function (assert) {
    await visit('/apps/blog/env');

    await click('[data-test-env-add]');
    await fillIn(row(3, '[data-test-env-key-input]'), '1BAD');
    assert.dom('[data-test-env-key-error]').exists({ count: 1 });
    assert.dom(row(3, '[data-test-env-key-input]')).hasAria('invalid', 'true');
    assert.dom('[data-test-env-save]').isDisabled();
    assert.dom('[data-test-env-save-deploy]').isDisabled();

    await fillIn(row(3, '[data-test-env-key-input]'), 'RAILS_ENV');
    assert.dom('[data-test-env-key-error]').hasText('キーが重複しています。');
    assert.dom('[data-test-env-save]').isDisabled();

    await fillIn(row(3, '[data-test-env-key-input]'), 'OK_1');
    assert.dom('[data-test-env-key-error]').doesNotExist();
    assert.dom('[data-test-env-save]').isNotDisabled();
  });

  test('bulk mode edits everything as .env text', async function (assert) {
    await visit('/apps/blog/env');
    await click('[data-test-env-bulk]');

    assert.dom('[data-test-env-row]').doesNotExist();
    assert
      .dom('[data-test-env-bulk-text]')
      .hasValue(`DATABASE_URL=${SECRET}\nRAILS_ENV=production\n`);
    assert.dom('[data-test-env-dirty]').doesNotExist();

    await fillIn(
      '[data-test-env-bulk-text]',
      [
        '# pasted from a local file',
        'RAILS_ENV=staging',
        '',
        'export GREETING="hello world"',
        "LITERAL='a # b'",
      ].join('\n'),
    );

    assert.dom('[data-test-env-issues]').doesNotExist();
    assert
      .dom('[data-test-env-dirty]')
      .includesText('追加 2 件・変更 1 件・削除 1 件');

    await click('[data-test-env-table]');

    assert.dom('[data-test-env-bulk-text]').doesNotExist();
    assert.dom('[data-test-env-row]').exists({ count: 3 });
    assert.dom(row(1, '[data-test-env-key]')).hasText('RAILS_ENV');
    assert.dom(row(1, '[data-test-env-status]')).hasText('変更');
    assert.dom(row(2, '[data-test-env-status]')).hasText('新規');
    assert
      .dom('[data-test-env-masked]')
      .exists({ count: 3 }, 'back in the table the values are hidden');

    await click('[data-test-env-save]');

    assert.deepEqual(fakes.api.callsTo('PUT', ENV)[0]?.body, {
      vars: [
        { key: 'RAILS_ENV', value: 'staging' },
        { key: 'GREETING', value: 'hello world' },
        { key: 'LITERAL', value: 'a # b' },
      ],
    });
  });

  test('bulk mode can be saved from directly, and shows edits made in the table', async function (assert) {
    await visit('/apps/blog/env');
    await click(row(2, '[data-test-env-remove]'));
    await click('[data-test-env-bulk]');

    assert
      .dom('[data-test-env-bulk-text]')
      .hasValue(`DATABASE_URL=${SECRET}\n`, 'the unsaved removal carries over');

    await fillIn('[data-test-env-bulk-text]', 'ONLY=one\n');
    await click('[data-test-env-save]');

    assert.deepEqual(fakes.api.callsTo('PUT', ENV)[0]?.body, {
      vars: [{ key: 'ONLY', value: 'one' }],
    });
    assert.dom('[data-test-env-saved]').exists();
    assert.dom('[data-test-env-bulk-text]').hasValue('ONLY=one\n');
  });

  test('bulk mode reports parse errors per line and blocks saving', async function (assert) {
    await visit('/apps/blog/env');
    await click('[data-test-env-bulk]');
    await fillIn(
      '[data-test-env-bulk-text]',
      [
        'RAILS_ENV=production',
        'topsecret-without-equals',
        '1BAD=topsecret',
        'RAILS_ENV=topsecret',
        'OPEN="topsecret',
      ].join('\n'),
    );

    assert.dom('[data-test-env-issue]').exists({ count: 4 });
    assert.dom('[data-test-env-issue="2"]').includesText('2 行目');
    assert.dom('[data-test-env-issue="2"]').includesText('KEY=VALUE');
    assert
      .dom('[data-test-env-issue="3"]')
      .includesText('英字かアンダースコア');
    assert.dom('[data-test-env-issue="4"]').includesText('1 行目と同じ');
    assert.dom('[data-test-env-issue="5"]').includesText('引用符');
    assert
      .dom('[data-test-env-issues]')
      .doesNotIncludeText('topsecret', 'messages do not repeat the input');
    assert.dom('[data-test-env-bulk-text]').hasAria('invalid', 'true');
    assert.dom('[data-test-env-save]').isDisabled();
    assert.dom('[data-test-env-save-deploy]').isDisabled();
    assert.dom('[data-test-env-table]').isDisabled();

    await fillIn('[data-test-env-bulk-text]', 'RAILS_ENV=production\nA=1');

    assert.dom('[data-test-env-issues]').doesNotExist();
    assert.dom('[data-test-env-save]').isNotDisabled();
    assert.dom('[data-test-env-table]').isNotDisabled();
  });

  test('save and redeploy: a staging app saves, then deploys the running commit', async function (assert) {
    await visit('/apps/blog/env');
    await click(row(2, '[data-test-env-remove]'));
    await click('[data-test-env-save-deploy]');

    assert.dom('[data-test-dialog]').doesNotExist('staging does not ask');
    assert.deepEqual(fakes.api.callsTo('PUT', ENV)[0]?.body, {
      vars: [{ key: 'DATABASE_URL', value: SECRET }],
    });
    assert.deepEqual(fakes.api.callsTo('POST', DEPLOYMENTS)[0]?.body, {
      sha: SHA_A,
      ref: 'main',
    });
    const order = fakes.api.calls
      .filter((call) => call.method !== 'GET')
      .map((call) => `${call.method} ${call.path}`);
    assert.deepEqual(order, [`PUT ${ENV}`, `POST ${DEPLOYMENTS}`]);

    assert
      .dom('[data-test-env-redeployed]')
      .includesText('保存して、再デプロイを開始しました');
    assert.dom('[data-test-env-dirty]').doesNotExist();
    assert.strictEqual(currentURL(), '/apps/blog/env');

    await click('[data-test-env-redeployed] a');
    assert.strictEqual(currentURL(), '/apps/blog/deploy');
  });

  test('save and redeploy: a production app goes through the confirmation first', async function (assert) {
    app = { ...app, environment: 'production' };

    await visit('/apps/blog/env');
    await click(row(2, '[data-test-env-remove]'));
    await click('[data-test-env-save-deploy]');

    assert
      .dom('[data-test-dialog]')
      .includesText('本番アプリを再デプロイしますか？')
      .includesText('blog')
      .includesText('環境変数を保存して');
    assert.dom('[data-test-dialog-target]').hasText(SHA_A.slice(0, 7));
    assert.dom('[data-test-dialog]').doesNotIncludeText('hunter2');
    assert.deepEqual(fakes.api.callsTo('PUT', ENV), [], 'nothing is saved yet');
    assert.deepEqual(fakes.api.callsTo('POST', DEPLOYMENTS), []);

    await click('[data-test-dialog-cancel]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(fakes.api.callsTo('PUT', ENV), []);
    assert.deepEqual(fakes.api.callsTo('POST', DEPLOYMENTS), []);
    assert.dom('[data-test-env-dirty]').exists('the edits are still there');

    await click('[data-test-env-save-deploy]');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.strictEqual(fakes.api.callsTo('PUT', ENV).length, 1);
    assert.deepEqual(fakes.api.callsTo('POST', DEPLOYMENTS)[0]?.body, {
      sha: SHA_A,
      ref: 'main',
    });
    assert.dom('[data-test-env-redeployed]').exists();
  });

  test('a plain save on a production app needs no confirmation', async function (assert) {
    app = { ...app, environment: 'production' };

    await visit('/apps/blog/env');
    await click(row(2, '[data-test-env-remove]'));
    await click('[data-test-env-save]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.strictEqual(fakes.api.callsTo('PUT', ENV).length, 1);
    assert.dom('[data-test-env-saved]').exists();

    // Redeploying afterwards is offered, and that does ask.
    await click('[data-test-env-redeploy]');

    assert.dom('[data-test-dialog]').exists();
    assert.dom('[data-test-dialog]').doesNotIncludeText('環境変数を保存して');
    assert.deepEqual(fakes.api.callsTo('POST', DEPLOYMENTS), []);

    await click('[data-test-dialog-confirm]');

    assert.strictEqual(fakes.api.callsTo('PUT', ENV).length, 1, 'no new PUT');
    assert.strictEqual(fakes.api.callsTo('POST', DEPLOYMENTS).length, 1);
    assert
      .dom('[data-test-env-redeployed]')
      .includesText('再デプロイを開始しました');
  });

  test('a redeploy that is refused keeps the save and says why', async function (assert) {
    fakes.api.fail('POST', DEPLOYMENTS, 507, 'disk_full', {
      used_percent: 91.2,
      limit_percent: 85,
    });

    await visit('/apps/blog/env');
    await click(row(2, '[data-test-env-remove]'));
    await click('[data-test-env-save-deploy]');

    assert.strictEqual(fakes.api.callsTo('PUT', ENV).length, 1);
    assert.dom('[data-test-env-saved]').exists('the variables are stored');
    assert
      .dom('[data-test-env-deploy-error]')
      .includesText('再デプロイを開始できませんでした')
      .includesText('ディスク使用率が上限を超えている');
    assert.dom('[data-test-env-redeployed]').doesNotExist();
    assert.dom('[data-test-env-dirty]').doesNotExist();
  });

  test('an app without a running release only offers to save', async function (assert) {
    app = makeApp();

    await visit('/apps/blog/env');
    await click(row(2, '[data-test-env-remove]'));

    assert.dom('[data-test-env-save-deploy]').doesNotExist();

    await click('[data-test-env-save]');

    assert.dom('[data-test-env-saved]').exists();
    assert.dom('[data-test-env-redeploy]').doesNotExist();
  });

  test('a rejected save keeps the edits and shows the error', async function (assert) {
    fakes.api.fail('PUT', ENV, 422, 'validation_failed', {
      vars: 'duplicate key',
    });

    await visit('/apps/blog/env');
    await click('[data-test-env-add]');
    await fillIn(row(3, '[data-test-env-key-input]'), 'NEW');
    await click('[data-test-env-save-deploy]');

    assert.dom('[data-test-error]').hasText('入力内容に誤りがあります。');
    assert.dom('[data-test-env-server-errors]').hasText('vars: duplicate key');
    assert.dom('[data-test-env-row]').exists({ count: 3 });
    assert.dom('[data-test-env-saved]').doesNotExist();
    assert.dom('[data-test-env-dirty]').exists();
    assert.deepEqual(
      fakes.api.callsTo('POST', DEPLOYMENTS),
      [],
      'nothing is deployed when the save failed',
    );
  });

  test('without variables there is exactly one action: add one', async function (assert) {
    fakes.api.on('GET', ENV, { vars: [] });

    await visit('/apps/blog/env');

    assert
      .dom('[data-test-env-empty]')
      .includesText('環境変数はまだありません');
    assert
      .dom('[data-test-env-empty] a, [data-test-env-empty] button')
      .exists({ count: 1 })
      .hasText('変数を追加');
    assert.dom('[data-test-env-save]').doesNotExist();
    assert.dom('[data-test-env-toggle-all]').doesNotExist();

    await click('[data-test-env-add]');

    assert.dom('[data-test-env-empty]').doesNotExist();
    assert.dom('[data-test-env-row]').exists({ count: 1 });
    assert
      .dom('[data-test-env-save]')
      .isDisabled('an empty row is not a change');

    await fillIn(row(1, '[data-test-env-key-input]'), 'FIRST');
    await fillIn(row(1, '[data-test-env-value-input]'), 'line one\nline two');
    await click('[data-test-env-save]');

    assert.deepEqual(fakes.api.callsTo('PUT', ENV)[0]?.body, {
      vars: [{ key: 'FIRST', value: 'line one\nline two' }],
    });
  });

  test('the list shows a loading line, then an error that can be retried', async function (assert) {
    let fail: (reason: unknown) => void = () => {};
    fakes.api.on(
      'GET',
      ENV,
      () => new Promise((_resolve, reject) => (fail = reject)),
    );

    const visiting = visit('/apps/blog/env');
    await waitFor('[data-test-loading]');

    assert.dom('[data-test-env-row]').doesNotExist();
    assert.dom('[data-test-env-bulk]').doesNotExist();

    fakes.api.fail('GET', ENV, 500, 'internal');
    fail(new Error('gone'));
    await visiting;

    assert
      .dom('[data-test-error]')
      .hasAttribute('role', 'alert')
      .includesText('環境変数を取得できませんでした');
    assert.dom('[data-test-env-form]').doesNotExist();

    fakes.api.on('GET', ENV, { vars: [{ key: 'A', value: '1' }] });
    await click('[data-test-error] button');

    assert.dom('[data-test-error]').doesNotExist();
    assert.dom('[data-test-env-row]').exists({ count: 1 });
  });

  test('moving straight to another app shows that app', async function (assert) {
    fakes.api
      .on('GET', '/api/apps/docs', makeApp({ slug: 'docs' }))
      .on('GET', '/api/apps/docs/env', {
        vars: [{ key: 'ONLY_ONE', value: 'x' }],
      });

    await visit('/apps/blog/env');
    assert.dom('[data-test-env-row]').exists({ count: 2 });

    // As with the browser's history: same route, different app.
    await visit('/apps/docs/env');

    assert.dom('[data-test-env-row]').exists({ count: 1 });
    assert.dom('[data-test-env-key]').hasText('ONLY_ONE');
  });
});
