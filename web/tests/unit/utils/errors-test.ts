import { module, test } from 'qunit';
import {
  ApiError,
  errorMessage,
  fieldErrors,
  loginErrorMessage,
  unavailability,
} from 'paas-console/utils/errors';

module('Unit | Utility | errors', function () {
  test('errorMessage maps known codes to Japanese', function (assert) {
    assert.strictEqual(
      errorMessage(new ApiError(404, 'app_not_found', 'no such app')),
      'アプリが見つかりません。',
    );
    assert.strictEqual(
      errorMessage(new ApiError(409, 'not_cancelable', '')),
      'このデプロイはすでに終了しているためキャンセルできません。',
    );
    assert.strictEqual(
      errorMessage(new ApiError(0, 'network_error', '')),
      'サーバーに接続できませんでした。',
    );
  });

  test('errorMessage adds the numbers of a full disk', function (assert) {
    const error = new ApiError(507, 'disk_full', 'disk is full', {
      used_percent: 91.24,
      limit_percent: 85,
    });

    assert.strictEqual(
      errorMessage(error),
      'ディスク使用率が上限を超えているためデプロイできません。（使用率 91.2% / 上限 85%）',
    );
    assert.strictEqual(
      errorMessage(new ApiError(507, 'disk_full', 'disk is full')),
      'ディスク使用率が上限を超えているためデプロイできません。',
    );
  });

  test('errorMessage falls back to a generic message plus the raw one', function (assert) {
    assert.strictEqual(
      errorMessage(new ApiError(418, 'teapot', 'short and stout')),
      'エラーが発生しました。（short and stout）',
    );
    assert.strictEqual(
      errorMessage(new ApiError(500, 'unknown', '')),
      'エラーが発生しました。',
    );
    assert.strictEqual(
      errorMessage(new Error('boom')),
      'エラーが発生しました。（boom）',
    );
    assert.strictEqual(errorMessage('nope'), 'エラーが発生しました。');
  });

  test('fieldErrors reads the details of a validation failure', function (assert) {
    const error = new ApiError(422, 'validation_failed', 'invalid', {
      web_port: 'must be between 1 and 65535',
      nested: { reason: 'odd' },
    });

    assert.deepEqual(fieldErrors(error), {
      web_port: 'must be between 1 and 65535',
      nested: '{"reason":"odd"}',
    });
    assert.deepEqual(fieldErrors(new ApiError(500, 'internal', '')), {});
    assert.deepEqual(fieldErrors(new Error('boom')), {});
  });

  test('fieldErrors puts conflicts on their field', function (assert) {
    assert.deepEqual(fieldErrors(new ApiError(409, 'slug_taken', '')), {
      slug: 'この slug はすでに使われています。',
    });
    assert.deepEqual(fieldErrors(new ApiError(409, 'hostname_taken', '')), {
      hostname: 'このホスト名はすでに登録されています。',
    });
  });

  test('unavailability tells missing features from missing records', function (assert) {
    assert.strictEqual(
      unavailability(new ApiError(503, 'not_configured', '')),
      'not_configured',
    );
    assert.strictEqual(
      unavailability(new ApiError(404, 'not_found', '')),
      'unavailable',
    );
    assert.strictEqual(
      unavailability(new ApiError(404, 'unknown', 'HTTP 404')),
      'unavailable',
    );
    assert.strictEqual(
      unavailability(new ApiError(404, 'app_not_found', '')),
      null,
    );
    assert.strictEqual(
      unavailability(new ApiError(404, 'repo_not_found', '')),
      null,
    );
    assert.strictEqual(unavailability(new ApiError(503, 'internal', '')), null);
    assert.strictEqual(unavailability(new Error('boom')), null);
  });

  test('loginErrorMessage covers the callback codes', function (assert) {
    assert.strictEqual(loginErrorMessage(null), '');
    assert.true(
      loginErrorMessage('not_allowed').includes('許可されていません'),
    );
    assert.true(loginErrorMessage('state_mismatch').includes('やり直して'));
    assert.true(loginErrorMessage('github_error').includes('GitHub'));
    assert.true(
      loginErrorMessage('not_configured').includes('設定されていません'),
    );
    assert.strictEqual(
      loginErrorMessage('something_new'),
      'ログインに失敗しました。',
    );
  });
});
