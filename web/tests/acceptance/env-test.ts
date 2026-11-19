import { module, test } from 'qunit';
import {
  click,
  currentURL,
  fillIn,
  findAll,
  triggerKeyEvent,
  visit,
  waitFor,
} from '@ember/test-helpers';
import { setupApplicationTest } from 'koroku-console/tests/helpers';
import {
  SHA_A,
  installFakes,
  makeApp,
  makeDeployment,
  type FakeCall,
  type Fakes,
} from 'koroku-console/tests/helpers/fakes';
import type { App } from 'koroku-console/utils/types';

const SECRET = 'postgres://app:hunter2@db/app';

const ENV = '/api/apps/blog/env';
const DEPLOYMENTS = '/api/apps/blog/deployments';

/** Every key a test below saves, renames or deletes. */
const KEYS = [
  'DATABASE_URL',
  'RAILS_ENV',
  'APP_HOST',
  'DB_URL',
  'FIRST',
  'NEW',
  'OK_1',
];

function row(n: number, selector = ''): string {
  return `[data-test-env-row]:nth-child(${n}) ${selector}`.trim();
}

/** Everything on the page as text and attributes. */
function pageHtml(): string {
  return document.querySelector('#ember-testing')?.innerHTML ?? '';
}

/** What the form controls hold: their values are not part of the HTML. */
function controlValues(): string {
  return [
    ...document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>(
      '#ember-testing input, #ember-testing textarea',
    ),
  ]
    .map((control) => control.value)
    .join('\n');
}

function keysOnScreen(): string[] {
  return findAll('[data-test-env-key]').map((key) => key.textContent.trim());
}

type Var = { key: string; value: string };

/**
 * A small stand-in for the API: the saved variables, what the running
 * release has (`null`: nothing to compare with), and the per-key endpoints.
 */
class EnvServer {
  saved: Var[];
  deployed: Var[] | null;

  constructor(saved: Var[], deployed: Var[] | null) {
    this.saved = saved;
    this.deployed = deployed;
  }

  private store(vars: Var[]): void {
    this.saved = [...vars].sort((a, b) => (a.key < b.key ? -1 : 1));
  }

  list() {
    const running = new Map(this.deployed?.map((v) => [v.key, v.value]));
    return {
      vars: this.saved.map((v) => ({
        ...v,
        pending: !this.deployed
          ? null
          : !running.has(v.key)
            ? 'added'
            : running.get(v.key) !== v.value
              ? 'changed'
              : null,
      })),
      removed: this.deployed
        ? [...running.keys()].filter(
            (key) => !this.saved.some((v) => v.key === key),
          )
        : [],
      compared: this.deployed !== null,
      values_hidden: false,
    };
  }

  install(fakes: Fakes, base = ENV): void {
    fakes.api
      .on('GET', base, () => this.list())
      .on('PUT', base, (call: FakeCall) => {
        this.store((call.body as { vars: Var[] }).vars);
        return this.list();
      });
    for (const from of KEYS) {
      fakes.api
        .on('PUT', `${base}/${from}`, (call: FakeCall) => {
          const body = call.body as { key?: string; value?: string };
          const key = body.key ?? from;
          const old = this.saved.find((v) => v.key === from);
          const value = body.value ?? old?.value ?? '';
          this.store([
            ...this.saved.filter((v) => v.key !== from && v.key !== key),
            { key, value },
          ]);
          return this.list();
        })
        .on('DELETE', `${base}/${from}`, () => {
          this.store(this.saved.filter((v) => v.key !== from));
          return this.list();
        });
    }
  }
}

module('Acceptance | environment variables', function (hooks) {
  setupApplicationTest(hooks);

  let fakes: Fakes;
  let app: App;
  let server: EnvServer;

  const INITIAL: Var[] = [
    { key: 'DATABASE_URL', value: SECRET },
    { key: 'RAILS_ENV', value: 'production' },
  ];

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
    const running = makeDeployment();
    app = makeApp({ current_deployment: running, latest_deployment: running });
    server = new EnvServer([...INITIAL], [...INITIAL]);
    server.install(fakes);
    fakes.api
      .on('GET', '/api/apps/blog', () => app)
      .on('POST', DEPLOYMENTS, () =>
        makeDeployment({
          id: 43,
          status: 'queued',
          started_at: null,
          finished_at: null,
        }),
      );
  });

  /** Requests that change something, in order. */
  function writes(): string[] {
    return fakes.api.calls
      .filter((call) => call.method !== 'GET')
      .map((call) => `${call.method} ${call.path}`);
  }

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
      .includesText('1 件ずつ保存できます')
      .includesText('次のデプロイで反映されます');
    assert.dom('[data-test-env-pending-banner]').doesNotExist();
    assert.dom('[data-test-env-pending]').doesNotExist();

    await click(row(1, '[data-test-env-reveal]'));

    assert.dom('[data-test-env-value]').exists({ count: 1 }).hasText(SECRET);
    assert.dom(row(1, '[data-test-env-reveal]')).hasAria('pressed', 'true');
    assert.dom('[data-test-env-masked]').exists({ count: 1 });

    await click(row(1, '[data-test-env-reveal]'));
    assert.dom('[data-test-env-value]').doesNotExist();
    assert.false(pageHtml().includes('hunter2'));

    await click('[data-test-env-toggle-all]');
    assert.dom('[data-test-env-value]').exists({ count: 2 });
    assert.dom('[data-test-env-toggle-all]').hasText('すべて隠す');

    await click('[data-test-env-toggle-all]');
    assert.dom('[data-test-env-value]').doesNotExist();
    assert.deepEqual(writes(), [], 'looking at values changes nothing');
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

  test('saving one row sends only that variable and deploys nothing', async function (assert) {
    await visit('/apps/blog/env');

    await click(row(2, '[data-test-env-edit]'));
    assert.dom(row(2, '[data-test-env-key-input]')).hasValue('RAILS_ENV');
    assert
      .dom(row(2, '[data-test-env-value-input]'))
      .hasValue('production')
      .isFocused('the value is ready to change');
    assert
      .dom(row(2, '[data-test-env-save]'))
      .isDisabled('nothing changed yet');

    await fillIn(row(2, '[data-test-env-value-input]'), 'staging');
    await click(row(2, '[data-test-env-save]'));

    assert.deepEqual(writes(), [`PUT ${ENV}/RAILS_ENV`]);
    assert.deepEqual(fakes.api.callsTo('PUT', `${ENV}/RAILS_ENV`)[0]?.body, {
      value: 'staging',
    });
    assert.dom(row(2, '[data-test-env-value-input]')).doesNotExist();
    assert
      .dom(row(2, '[data-test-env-masked]'))
      .exists('saved: the value is hidden again');
    assert.dom(row(2, '[data-test-env-pending]')).hasText('未反映（変更）');
    assert.dom(row(1, '[data-test-env-pending]')).doesNotExist();
    assert
      .dom('[data-test-env-announce]')
      .hasText('RAILS_ENV を保存しました。次のデプロイで反映されます。');
    assert.dom('[data-test-env-pending-banner]').exists();
    assert.deepEqual(
      fakes.api.callsTo('POST', DEPLOYMENTS),
      [],
      'saving alone deploys nothing',
    );
  });

  test('keys: Enter saves from the key, Ctrl+Enter from the value, Escape cancels', async function (assert) {
    await visit('/apps/blog/env');

    await click(row(2, '[data-test-env-edit]'));
    await fillIn(row(2, '[data-test-env-value-input]'), 'line one');
    await triggerKeyEvent(
      row(2, '[data-test-env-value-input]'),
      'keydown',
      'Enter',
    );
    assert.deepEqual(writes(), [], 'a plain Enter is a line break in a value');

    await triggerKeyEvent(
      row(2, '[data-test-env-value-input]'),
      'keydown',
      'Enter',
      { ctrlKey: true },
    );
    assert.deepEqual(writes(), [`PUT ${ENV}/RAILS_ENV`]);
    assert.dom(row(2, '[data-test-env-value-input]')).doesNotExist();

    await click(row(1, '[data-test-env-edit]'));
    await fillIn(row(1, '[data-test-env-value-input]'), 'not this');
    await triggerKeyEvent(
      row(1, '[data-test-env-value-input]'),
      'keydown',
      'Escape',
    );
    assert.dom(row(1, '[data-test-env-value-input]')).doesNotExist();
    assert.dom(row(1, '[data-test-env-masked]')).exists();
    assert.strictEqual(writes().length, 1, 'Escape saves nothing');

    await click(row(1, '[data-test-env-edit]'));
    assert
      .dom(row(1, '[data-test-env-value-input]'))
      .hasValue(SECRET, 'the cancelled draft is gone');
    await fillIn(row(1, '[data-test-env-key-input]'), 'DB_URL');
    await triggerKeyEvent(
      row(1, '[data-test-env-key-input]'),
      'keydown',
      'Enter',
    );
    assert.deepEqual(writes()[1], `PUT ${ENV}/DATABASE_URL`);
  });

  test('a new key renames the variable in one request', async function (assert) {
    await visit('/apps/blog/env');

    await click(row(1, '[data-test-env-edit]'));
    await fillIn(row(1, '[data-test-env-key-input]'), 'DB_URL');
    await click(row(1, '[data-test-env-save]'));

    assert.deepEqual(writes(), [`PUT ${ENV}/DATABASE_URL`]);
    assert.deepEqual(fakes.api.callsTo('PUT', `${ENV}/DATABASE_URL`)[0]?.body, {
      key: 'DB_URL',
      value: SECRET,
    });
    assert.deepEqual(keysOnScreen(), ['DB_URL', 'RAILS_ENV']);
    assert.dom(row(1, '[data-test-env-pending]')).hasText('未反映（追加）');
    assert
      .dom('[data-test-env-removed]')
      .includesText('DATABASE_URL', 'the old name is still in the release');
  });

  test('adding a variable saves it on its own, in the order of the server', async function (assert) {
    await visit('/apps/blog/env');

    await click('[data-test-env-add]');
    assert
      .dom(row(3, '[data-test-env-key-input]'))
      .isFocused('a new row is ready to type in');
    assert.dom(row(3, '[data-test-env-save]')).isDisabled('no key yet');

    await fillIn(row(3, '[data-test-env-key-input]'), 'APP_HOST');
    await fillIn(row(3, '[data-test-env-value-input]'), 'blog.example.org');
    await click(row(3, '[data-test-env-save]'));

    assert.deepEqual(writes(), [`PUT ${ENV}/APP_HOST`]);
    assert.deepEqual(fakes.api.callsTo('PUT', `${ENV}/APP_HOST`)[0]?.body, {
      value: 'blog.example.org',
    });
    assert.deepEqual(keysOnScreen(), ['APP_HOST', 'DATABASE_URL', 'RAILS_ENV']);
    assert.dom(row(1, '[data-test-env-pending]')).hasText('未反映（追加）');
    assert.dom('[data-test-env-key-input]').doesNotExist();
  });

  test('a row being edited keeps its draft while another row is saved', async function (assert) {
    await visit('/apps/blog/env');

    await click(row(1, '[data-test-env-edit]'));
    await fillIn(row(1, '[data-test-env-value-input]'), 'postgres://draft');
    await click('[data-test-env-add]');
    await fillIn(row(3, '[data-test-env-key-input]'), 'APP_HOST');
    await click(row(3, '[data-test-env-save]'));

    assert.deepEqual(keysOnScreen(), ['APP_HOST', 'RAILS_ENV']);
    assert
      .dom('[data-test-env-value-input]')
      .exists({ count: 1 })
      .hasValue('postgres://draft', 'the other draft is untouched');
    assert.dom('[data-test-env-key-input]').hasValue('DATABASE_URL');
    assert.deepEqual(writes(), [`PUT ${ENV}/APP_HOST`]);
  });

  test('invalid and duplicate keys block saving that row', async function (assert) {
    await visit('/apps/blog/env');

    await click('[data-test-env-add]');
    await fillIn(row(3, '[data-test-env-key-input]'), '1BAD');
    assert.dom('[data-test-env-key-error]').exists({ count: 1 });
    assert.dom(row(3, '[data-test-env-key-input]')).hasAria('invalid', 'true');
    assert.dom(row(3, '[data-test-env-save]')).isDisabled();

    await fillIn(row(3, '[data-test-env-key-input]'), 'RAILS_ENV');
    assert.dom('[data-test-env-key-error]').hasText('キーが重複しています。');
    assert.dom(row(3, '[data-test-env-save]')).isDisabled();

    await fillIn(row(3, '[data-test-env-key-input]'), 'OK_1');
    assert.dom('[data-test-env-key-error]').doesNotExist();
    assert.dom(row(3, '[data-test-env-save]')).isNotDisabled();

    await click(row(3, '[data-test-env-cancel]'));
    assert
      .dom('[data-test-env-row]')
      .exists({ count: 2 }, 'cancelling a new row removes it');
    assert.deepEqual(writes(), []);
  });

  test('deleting asks in the row first', async function (assert) {
    await visit('/apps/blog/env');

    await click(row(2, '[data-test-env-remove]'));
    assert.dom(row(2)).includesText('削除しますか？');
    assert.dom(row(2, '[data-test-env-edit]')).doesNotExist();
    assert.dom('[data-test-env-bulk]').isDisabled();

    await click(row(2, '[data-test-env-remove-cancel]'));
    assert.dom(row(2)).doesNotIncludeText('削除しますか？');
    assert.deepEqual(writes(), []);

    await click(row(2, '[data-test-env-remove]'));
    await click(row(2, '[data-test-env-remove-confirm]'));

    assert.deepEqual(writes(), [`DELETE ${ENV}/RAILS_ENV`]);
    assert.deepEqual(keysOnScreen(), ['DATABASE_URL']);
    assert
      .dom('[data-test-env-announce]')
      .hasText('RAILS_ENV を削除しました。次のデプロイで反映されます。');
    assert.dom('[data-test-env-pending-summary]').hasText('削除 1 件');
    assert.dom('[data-test-env-removed]').includesText('RAILS_ENV');
  });

  test('a rejected save keeps the draft and says why in the row', async function (assert) {
    fakes.api.fail('PUT', `${ENV}/RAILS_ENV`, 422, 'validation_failed', {
      value: 'cannot be written to an env file',
    });

    await visit('/apps/blog/env');
    await click(row(2, '[data-test-env-edit]'));
    await fillIn(row(2, '[data-test-env-value-input]'), 'bad');
    await click(row(2, '[data-test-env-save]'));

    assert
      .dom(row(2, '[data-test-env-row-error]'))
      .includesText('入力内容に誤りがあります。')
      .includesText('value: cannot be written to an env file');
    assert.dom(row(2, '[data-test-env-value-input]')).hasValue('bad');
    assert.dom(row(1, '[data-test-env-row-error]')).doesNotExist();

    await click(row(2, '[data-test-env-cancel]'));
    assert.dom('[data-test-env-row-error]').doesNotExist();
  });

  test('a value never appears in a label, a title, a URL or an error', async function (assert) {
    fakes.api.fail('PUT', `${ENV}/DATABASE_URL`, 0, 'network_error');

    await visit('/apps/blog/env');
    await click(row(1, '[data-test-env-edit]'));
    await fillIn(row(1, '[data-test-env-value-input]'), `${SECRET}?x=1`);
    await click(row(1, '[data-test-env-save]'));

    assert
      .dom(row(1, '[data-test-env-row-error]'))
      .hasText('サーバーに接続できませんでした。');
    const attributes = findAll(
      '#ember-testing [title], #ember-testing [aria-label]',
    ).map(
      (element) =>
        `${element.getAttribute('title')} ${element.getAttribute('aria-label')}`,
    );
    assert.false(attributes.join('\n').includes('hunter2'));
    assert.false(document.title.includes('hunter2'));
    assert.false(currentURL().includes('hunter2'));
    assert.false(pageHtml().includes('hunter2'));
    assert.false(
      fakes.api.calls.some((call) => call.path.includes('hunter2')),
      'values only travel in request bodies',
    );
  });

  test('editing one variable does not bring the others into the page', async function (assert) {
    await visit('/apps/blog/env');

    await click(row(2, '[data-test-env-edit]'));

    assert.dom(row(1, '[data-test-env-masked]')).exists();
    assert.false(pageHtml().includes('hunter2'));
    assert.false(
      controlValues().includes('hunter2'),
      'no hidden control holds the other value either',
    );

    await click(row(1, '[data-test-env-edit]'));
    assert.true(controlValues().includes('hunter2'), 'editing shows the value');

    await click(row(1, '[data-test-env-cancel]'));
    assert.dom(row(1, '[data-test-env-masked]')).exists();
    assert.false(
      controlValues().includes('hunter2'),
      'cancelled: hidden again',
    );
    assert.false(pageHtml().includes('hunter2'));
  });

  test('a revealed value does not stay revealed after leaving the tab', async function (assert) {
    await visit('/apps/blog/env');
    await click('[data-test-env-toggle-all]');
    assert.dom('[data-test-env-value]').exists({ count: 2 });

    await click('[data-test-tab="settings"]');
    assert.false(pageHtml().includes('hunter2'), 'gone with the tab');

    await click('[data-test-tab="env"]');

    assert.dom('[data-test-env-masked]').exists({ count: 2 });
    assert.dom('[data-test-env-value]').doesNotExist();
    assert.false(pageHtml().includes('hunter2'));
  });

  test('changes the release does not have are marked, and one redeploy applies them', async function (assert) {
    server.deployed = [
      { key: 'LEGACY_FLAG', value: '1' },
      { key: 'RAILS_ENV', value: 'staging' },
    ];

    await visit('/apps/blog/env');

    assert.dom(row(1, '[data-test-env-pending]')).hasText('未反映（追加）');
    assert.dom(row(2, '[data-test-env-pending]')).hasText('未反映（変更）');
    assert.dom(row(1)).hasAttribute('data-pending', 'added');
    assert
      .dom('[data-test-env-pending-banner]')
      .includesText('未反映の変更があります');
    assert
      .dom('[data-test-env-pending-summary]')
      .hasText('追加 1 件・変更 1 件・削除 1 件');
    assert.dom('[data-test-env-removed]').includesText('LEGACY_FLAG');

    await click('[data-test-env-redeploy]');

    assert.dom('[data-test-dialog]').doesNotExist('staging does not ask');
    assert.deepEqual(fakes.api.callsTo('POST', DEPLOYMENTS)[0]?.body, {
      sha: SHA_A,
      ref: 'main',
    });
    assert.deepEqual(writes(), [`POST ${DEPLOYMENTS}`]);
    assert
      .dom('[data-test-env-redeployed]')
      .includesText('再デプロイを開始しました');
    assert.dom('[data-test-env-pending-banner]').doesNotExist();
    assert.strictEqual(currentURL(), '/apps/blog/env');

    await click('[data-test-env-redeployed] a');
    assert.strictEqual(currentURL(), '/apps/blog/deploy');
  });

  test('putting back the deployed value clears the mark', async function (assert) {
    await visit('/apps/blog/env');

    await click(row(2, '[data-test-env-edit]'));
    await fillIn(row(2, '[data-test-env-value-input]'), 'staging');
    await click(row(2, '[data-test-env-save]'));
    assert.dom('[data-test-env-pending-banner]').exists();

    await click(row(2, '[data-test-env-edit]'));
    await fillIn(row(2, '[data-test-env-value-input]'), 'production');
    await click(row(2, '[data-test-env-save]'));

    assert.dom('[data-test-env-pending]').doesNotExist();
    assert.dom('[data-test-env-pending-banner]').doesNotExist();
  });

  test('a production app asks before it redeploys', async function (assert) {
    app = { ...app, environment: 'production' };
    server.deployed = [{ key: 'RAILS_ENV', value: 'staging' }];

    await visit('/apps/blog/env');
    await click('[data-test-env-redeploy]');

    assert
      .dom('[data-test-dialog]')
      .includesText('本番アプリを再デプロイしますか？')
      .includesText('blog')
      .includesText('保存済みの環境変数で');
    assert.dom('[data-test-dialog-target]').hasText(SHA_A.slice(0, 7));
    assert.dom('[data-test-dialog]').doesNotIncludeText('hunter2');
    assert.deepEqual(fakes.api.callsTo('POST', DEPLOYMENTS), []);

    await click('[data-test-dialog-cancel]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(fakes.api.callsTo('POST', DEPLOYMENTS), []);
    assert.dom('[data-test-env-pending-banner]').exists();

    await click('[data-test-env-redeploy]');
    await click('[data-test-dialog-confirm]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.strictEqual(fakes.api.callsTo('POST', DEPLOYMENTS).length, 1);
    assert.dom('[data-test-env-redeployed]').exists();
  });

  test('saving a row on a production app needs no confirmation', async function (assert) {
    app = { ...app, environment: 'production' };

    await visit('/apps/blog/env');
    await click(row(2, '[data-test-env-edit]'));
    await fillIn(row(2, '[data-test-env-value-input]'), 'staging');
    await click(row(2, '[data-test-env-save]'));

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.deepEqual(writes(), [`PUT ${ENV}/RAILS_ENV`]);
  });

  test('a redeploy that is refused says why', async function (assert) {
    server.deployed = [{ key: 'RAILS_ENV', value: 'staging' }];
    fakes.api.fail('POST', DEPLOYMENTS, 507, 'disk_full', {
      used_percent: 91.2,
      limit_percent: 85,
    });

    await visit('/apps/blog/env');
    await click('[data-test-env-redeploy]');

    assert
      .dom('[data-test-env-deploy-error]')
      .includesText('再デプロイを開始できませんでした')
      .includesText('ディスク使用率が上限を超えている');
    assert.dom('[data-test-env-redeployed]').doesNotExist();
    assert
      .dom('[data-test-env-pending-banner]')
      .exists('the changes are still not deployed');
  });

  test('without a running release there is nothing to compare or redeploy', async function (assert) {
    app = makeApp();
    server.deployed = null;

    await visit('/apps/blog/env');
    await click(row(2, '[data-test-env-edit]'));
    await fillIn(row(2, '[data-test-env-value-input]'), 'staging');
    await click(row(2, '[data-test-env-save]'));

    assert.dom('[data-test-env-pending]').doesNotExist();
    assert.dom('[data-test-env-pending-banner]').doesNotExist();
    assert.dom('[data-test-env-redeploy]').doesNotExist();
    assert
      .dom('[data-test-env-announce]')
      .includesText('次のデプロイで反映されます');
  });

  test('bulk mode replaces everything with .env text', async function (assert) {
    await visit('/apps/blog/env');
    await click('[data-test-env-bulk]');

    assert.dom('[data-test-env-row]').doesNotExist();
    assert
      .dom('[data-test-env-bulk-text]')
      .hasValue(`DATABASE_URL=${SECRET}\nRAILS_ENV=production\n`);
    assert.dom('[data-test-env-bulk-form]').includesText('全体を置き換えます');
    assert.dom('[data-test-env-bulk-save]').isDisabled('nothing changed yet');

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
    await click('[data-test-env-bulk-save]');

    assert.deepEqual(writes(), [`PUT ${ENV}`]);
    assert.deepEqual(fakes.api.callsTo('PUT', ENV)[0]?.body, {
      vars: [
        { key: 'RAILS_ENV', value: 'staging' },
        { key: 'GREETING', value: 'hello world' },
        { key: 'LITERAL', value: 'a # b' },
      ],
    });
    assert.dom('[data-test-env-bulk-text]').doesNotExist('back to the table');
    assert.deepEqual(keysOnScreen(), ['GREETING', 'LITERAL', 'RAILS_ENV']);
    assert
      .dom('[data-test-env-masked]')
      .exists({ count: 3 }, 'back in the table the values are hidden');
    assert
      .dom('[data-test-env-pending-summary]')
      .hasText('追加 2 件・変更 1 件・削除 1 件');
  });

  test('bulk mode can be left without saving, and waits for rows being edited', async function (assert) {
    await visit('/apps/blog/env');

    await click(row(1, '[data-test-env-edit]'));
    assert.dom('[data-test-env-bulk]').isDisabled();
    await click(row(1, '[data-test-env-cancel]'));
    assert.dom('[data-test-env-bulk]').isNotDisabled();

    await click('[data-test-env-bulk]');
    await fillIn('[data-test-env-bulk-text]', 'ONLY=one\n');
    await click('[data-test-env-bulk-cancel]');

    assert.deepEqual(keysOnScreen(), ['DATABASE_URL', 'RAILS_ENV']);
    assert.deepEqual(writes(), []);

    await click('[data-test-env-bulk]');
    assert
      .dom('[data-test-env-bulk-text]')
      .hasValue(
        `DATABASE_URL=${SECRET}\nRAILS_ENV=production\n`,
        'it starts again from what is saved',
      );
  });

  test('a rejected bulk save keeps the text and shows the error', async function (assert) {
    fakes.api.fail('PUT', ENV, 422, 'validation_failed', {
      vars: 'duplicate key',
    });

    await visit('/apps/blog/env');
    await click('[data-test-env-bulk]');
    await fillIn('[data-test-env-bulk-text]', 'NEW=1\n');
    await click('[data-test-env-bulk-save]');

    assert.dom('[data-test-error]').hasText('入力内容に誤りがあります。');
    assert.dom('[data-test-env-bulk-errors]').hasText('vars: duplicate key');
    assert.dom('[data-test-env-bulk-text]').hasValue('NEW=1\n');
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
    assert.dom('[data-test-env-bulk-save]').isDisabled();

    await fillIn('[data-test-env-bulk-text]', 'RAILS_ENV=production\nA=1');

    assert.dom('[data-test-env-issues]').doesNotExist();
    assert.dom('[data-test-env-bulk-save]').isNotDisabled();
  });

  test('without variables there is exactly one action: add one', async function (assert) {
    server.saved = [];
    server.deployed = [];

    await visit('/apps/blog/env');

    assert
      .dom('[data-test-env-empty]')
      .includesText('環境変数はまだありません');
    assert
      .dom('[data-test-env-empty] a, [data-test-env-empty] button')
      .exists({ count: 1 })
      .hasText('変数を追加');
    assert.dom('[data-test-env-toggle-all]').doesNotExist();

    await click('[data-test-env-add]');

    assert.dom('[data-test-env-empty]').doesNotExist();
    assert.dom('[data-test-env-row]').exists({ count: 1 });

    await fillIn(row(1, '[data-test-env-key-input]'), 'FIRST');
    await fillIn(row(1, '[data-test-env-value-input]'), 'line one\nline two');
    await click(row(1, '[data-test-env-save]'));

    assert.deepEqual(fakes.api.callsTo('PUT', `${ENV}/FIRST`)[0]?.body, {
      value: 'line one\nline two',
    });
    assert.deepEqual(keysOnScreen(), ['FIRST']);
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
    assert.dom('[data-test-env-list]').doesNotExist();

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

module(
  'Acceptance | environment variables without a recent login',
  function (hooks) {
    setupApplicationTest(hooks);

    test('only the keys are listed and nothing can be edited', async function (assert) {
      const fakes = installFakes(this.owner);
      const running = makeDeployment();
      fakes.api
        .on(
          'GET',
          '/api/apps/blog',
          makeApp({ current_deployment: running, latest_deployment: running }),
        )
        .on('GET', ENV, {
          vars: [{ key: 'DATABASE_URL', value: null, pending: 'changed' }],
          removed: [],
          compared: true,
          values_hidden: true,
        });

      await visit('/apps/blog/env');

      assert.dom('[data-test-env-hidden]').exists();
      assert.dom('[data-test-env-key]').hasText('DATABASE_URL');
      assert
        .dom('[data-test-env-pending]')
        .hasText('未反映（変更）', 'what differs is not a secret');
      assert.dom('[data-test-env-pending-banner]').exists();
      assert.dom('[data-test-env-reveal]').doesNotExist();
      assert.dom('[data-test-env-edit]').doesNotExist();
      assert.dom('[data-test-env-remove]').doesNotExist();
      assert.dom('[data-test-env-add]').doesNotExist();
      assert.dom('[data-test-env-bulk]').doesNotExist();

      await click('[data-test-env-reauth]');
      assert.deepEqual(fakes.browser.assigned, [
        '/auth/github/login?return_to=%2Fapps%2Fblog%2Fenv',
      ]);
    });
  },
);
