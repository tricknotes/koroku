import { module, test } from 'qunit';
import {
  ApiError,
  errorMessage,
  fieldErrors,
  loginErrorMessage,
  policyViolations,
  unavailability,
} from 'koroku-console/utils/errors';

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

  test('every code the API can answer with has its own message', function (assert) {
    // The codes written by internal/api (and the client's own
    // `network_error`). A code without a message falls back to the generic
    // text plus the server's English one.
    const codes = [
      'invalid_json',
      'invalid_limit',
      'invalid_volumes',
      'body_too_large',
      'unsupported_media_type',
      'method_not_allowed',
      'unauthenticated',
      'forbidden_origin',
      'forbidden_host',
      'not_found',
      'app_not_found',
      'deployment_not_found',
      'domain_not_found',
      'snapshot_not_found',
      'repo_not_found',
      'slug_taken',
      'no_free_port',
      'not_cancelable',
      'hostname_taken',
      'busy',
      'not_deployed',
      'validation_failed',
      'policy_violation',
      'file_too_large',
      'ref_unresolvable',
      'disk_full',
      'internal',
      'prune_failed',
      'not_configured',
      'github_rate_limited',
      'github_error',
      'network_error',
    ];
    const seen = new Set<string>();
    for (const code of codes) {
      const message = errorMessage(new ApiError(400, code, 'english text'));
      assert.false(
        message.startsWith('エラーが発生しました'),
        `${code} has a message of its own`,
      );
      assert.false(message.includes('english text'), `${code} is translated`);
      seen.add(message);
    }
    assert.strictEqual(seen.size, codes.length, 'no two codes share a text');
  });

  test('errorMessage explains the hardening refusals', function (assert) {
    assert.true(
      errorMessage(new ApiError(415, 'unsupported_media_type', '')).includes(
        'Content-Type',
      ),
    );
    assert.true(
      errorMessage(new ApiError(403, 'forbidden_host', '')).includes(
        'localhost',
      ),
    );
    assert.true(
      errorMessage(new ApiError(422, 'policy_violation', '')).includes(
        '再デプロイ',
      ),
    );
  });

  test('policyViolations lists the findings of a policy violation', function (assert) {
    const error = new ApiError(422, 'policy_violation', 'no longer passes', {
      violations: [
        {
          service: 'web',
          rule: 'privileged',
          message: 'privileged containers are not allowed',
          relaxable: true,
        },
        { service: '', rule: 'ports', message: 'a port is published' },
        { service: 'db' },
        'not an object',
      ],
    });

    assert.deepEqual(policyViolations(error), [
      'web: privileged containers are not allowed',
      'a port is published',
    ]);
    assert.deepEqual(
      policyViolations(new ApiError(422, 'policy_violation', '')),
      [],
    );
    assert.deepEqual(
      policyViolations(
        new ApiError(422, 'validation_failed', '', { violations: [] }),
      ),
      [],
    );
    assert.deepEqual(policyViolations(new Error('boom')), []);
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
    assert.strictEqual(
      unavailability(new ApiError(404, 'snapshot_not_found', '')),
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
