import { module, test } from 'qunit';
import { click, render, settled } from '@ember/test-helpers';
import { setupRenderingTest } from 'koroku-console/tests/helpers';
import AsyncView from 'koroku-console/components/async-view';
import ErrorMessage from 'koroku-console/components/error-message';
import { ApiError } from 'koroku-console/utils/errors';
import Loadable from 'koroku-console/utils/loadable';

/** A Loadable whose requests are answered by the test. */
function controlled(): {
  data: Loadable<string[]>;
  answer: (value: string[]) => Promise<void>;
  fail: (error: unknown) => Promise<void>;
  requests: () => number;
} {
  const pending: {
    resolve: (value: string[]) => void;
    reject: (reason: unknown) => void;
  }[] = [];
  const data = new Loadable<string[]>(
    () => new Promise((resolve, reject) => pending.push({ resolve, reject })),
  );
  const last = () => {
    const request = pending[pending.length - 1];
    if (!request) throw new Error('nothing was requested');
    return request;
  };
  return {
    data,
    answer: async (value) => {
      last().resolve(value);
      await settled();
    },
    fail: async (error) => {
      last().reject(error);
      await settled();
    },
    requests: () => pending.length,
  };
}

module('Integration | Component | async-view', function (hooks) {
  setupRenderingTest(hooks);

  test('loading, then the value; exactly one of the states at a time', async function (assert) {
    const { data, answer } = controlled();
    void data.load();

    await render(
      <template>
        <AsyncView @data={{data}} @errorTitle="取得できませんでした" as |items|>
          <ul data-test-value>
            {{#each items as |item|}}<li>{{item}}</li>{{/each}}
          </ul>
        </AsyncView>
      </template>,
    );

    assert
      .dom('[data-test-loading]')
      .hasText('読み込み中…')
      .hasAttribute('aria-live', 'polite');
    assert.dom('[data-test-value]').doesNotExist();
    assert.dom('[data-test-error]').doesNotExist();

    await answer(['a', 'b']);

    assert.dom('[data-test-value] li').exists({ count: 2 });
    assert.dom('[data-test-loading]').doesNotExist();
    assert.dom('[data-test-error]').doesNotExist();
  });

  test('an empty value is still a value: the block decides what empty means', async function (assert) {
    const { data, answer } = controlled();
    void data.load();

    await render(
      <template>
        <AsyncView @data={{data}} as |items|>
          {{#if items.length}}
            <p data-test-value>{{items.length}}</p>
          {{else}}
            <p data-test-empty>まだありません</p>
          {{/if}}
        </AsyncView>
      </template>,
    );
    await answer([]);

    assert.dom('[data-test-empty]').exists();
    assert.dom('[data-test-loading]').doesNotExist();
  });

  test('a custom loading block replaces the plain line', async function (assert) {
    const { data } = controlled();
    void data.load();

    await render(
      <template>
        <AsyncView @data={{data}}>
          <:loading><div data-test-skeleton></div></:loading>
          <:default><p data-test-value></p></:default>
        </AsyncView>
      </template>,
    );

    assert.dom('[data-test-skeleton]').exists();
    assert.dom('[data-test-loading]').doesNotExist();
  });

  test('a failure shows what failed and why, and retries on request', async function (assert) {
    const { data, answer, fail, requests } = controlled();
    void data.load();

    await render(
      <template>
        <AsyncView
          @data={{data}}
          @errorTitle="一覧を取得できませんでした"
          as |items|
        >
          <p data-test-value>{{items.length}}</p>
        </AsyncView>
      </template>,
    );
    await fail(new ApiError(0, 'network_error', ''));

    assert
      .dom('[data-test-error]')
      .hasAttribute('role', 'alert')
      .includesText('一覧を取得できませんでした')
      .includesText('サーバーに接続できませんでした。');
    assert.dom('[data-test-error] button').exists({ count: 1 });
    assert.dom('[data-test-value]').doesNotExist();
    assert.dom('[data-test-loading]').doesNotExist();
    assert.strictEqual(requests(), 1);

    await click('[data-test-error] button');
    await answer(['a']);

    assert.strictEqual(requests(), 2, 'one new request per press');
    assert.dom('[data-test-value]').hasText('1');
    assert.dom('[data-test-error]').doesNotExist();
  });

  test('a missing feature is a placeholder, not an error with a retry', async function (assert) {
    const { data, fail } = controlled();
    void data.load();

    await render(
      <template>
        <AsyncView @data={{data}} @errorTitle="取得できませんでした" />
      </template>,
    );
    await fail(new ApiError(503, 'not_configured', ''));

    assert
      .dom('[data-test-unavailable="not_configured"]')
      .hasAttribute('role', 'status')
      .includesText('この機能は設定されていません。');
    assert.dom('[data-test-error]').doesNotExist();
    assert.dom('button').doesNotExist('retrying would not help');

    void data.load();
    await fail(new ApiError(404, 'not_found', ''));

    assert
      .dom('[data-test-unavailable="unavailable"]')
      .includesText('この機能はまだ利用できません。');
  });

  test('a quiet refresh that fails keeps the value on screen', async function (assert) {
    const { data, answer, fail } = controlled();
    void data.load();

    await render(
      <template>
        <AsyncView @data={{data}} as |items|>
          <p data-test-value>{{items.length}}</p>
        </AsyncView>
      </template>,
    );
    await answer(['a']);

    void data.load({ quiet: true });
    await settled();
    assert.dom('[data-test-value]').hasText('1', 'no flash of "loading"');
    assert.dom('[data-test-loading]').doesNotExist();

    await fail(new ApiError(500, 'internal', ''));

    assert.dom('[data-test-value]').hasText('1');
    assert.dom('[data-test-error]').doesNotExist();
  });
});

module('Integration | Component | error-message', function (hooks) {
  setupRenderingTest(hooks);

  test('nothing is rendered without an error', async function (assert) {
    await render(<template><ErrorMessage @error={{null}} /></template>);

    assert.dom('[data-test-error]').doesNotExist();
    assert.dom('[role="alert"]').doesNotExist();
  });

  test('a refusal is announced with its Japanese message', async function (assert) {
    const error = new ApiError(409, 'busy', 'another operation is running');

    await render(<template><ErrorMessage @error={{error}} /></template>);

    assert
      .dom('[data-test-error]')
      .hasAttribute('role', 'alert')
      .hasText('ほかの処理が実行中です。しばらくしてからやり直してください。');
  });

  test('field reasons are listed only where asked for', async function (assert) {
    const error = new ApiError(422, 'validation_failed', 'invalid', {
      ref: 'must not be empty',
    });

    await render(
      <template>
        <div data-test-plain><ErrorMessage @error={{error}} /></div>
        <div data-test-detailed>
          <ErrorMessage @error={{error}} @showDetails={{true}} />
        </div>
      </template>,
    );

    assert.dom('[data-test-plain] [data-test-error-detail]').doesNotExist();
    assert
      .dom('[data-test-detailed] [data-test-error-detail]')
      .exists({ count: 1 })
      .hasText('ref: must not be empty');
  });
});
