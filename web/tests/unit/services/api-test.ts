import { module, test } from 'qunit';
import {
  buildUrl,
  parseErrorBody,
  repoPath,
} from 'koroku-console/services/api';

module('Unit | Service | api (pure parts)', function () {
  test('buildUrl leaves out empty parameters and encodes the rest', function (assert) {
    assert.strictEqual(buildUrl('/api/apps'), '/api/apps');
    assert.strictEqual(buildUrl('/api/apps', {}), '/api/apps');
    assert.strictEqual(
      buildUrl('/api/apps/blog/logs', {
        service: '',
        tail: 200,
        from: undefined,
        to: null,
      }),
      '/api/apps/blog/logs?tail=200',
    );
    assert.strictEqual(
      buildUrl('/api/apps/blog', { volumes: true }),
      '/api/apps/blog?volumes=true',
    );
    assert.strictEqual(
      buildUrl('/api/x', { q: 'a b&c', ref: 'feature/search' }),
      '/api/x?q=a+b%26c&ref=feature%2Fsearch',
    );
  });

  test('repoPath splits owner and repository', function (assert) {
    assert.strictEqual(repoPath('octo/blog'), '/api/repos/octo/blog');
    assert.strictEqual(repoPath('octo/we ird?'), '/api/repos/octo/we%20ird%3F');
  });

  test('parseErrorBody reads the API error shape', function (assert) {
    const error = parseErrorBody(
      422,
      JSON.stringify({
        error: {
          code: 'validation_failed',
          message: 'some fields are invalid',
          details: { web_port: 'must be between 1 and 65535' },
        },
      }),
    );

    assert.strictEqual(error.status, 422);
    assert.strictEqual(error.code, 'validation_failed');
    assert.strictEqual(error.message, 'some fields are invalid');
    assert.deepEqual(error.details, {
      web_port: 'must be between 1 and 65535',
    });
  });

  test('parseErrorBody keeps a list of details under `violations`', function (assert) {
    const error = parseErrorBody(
      422,
      JSON.stringify({
        error: {
          code: 'policy_violation',
          message: 'the release no longer passes the deploy policy',
          details: [{ service: 'web', rule: 'privileged', message: 'no' }],
        },
      }),
    );

    assert.strictEqual(error.code, 'policy_violation');
    assert.deepEqual(error.details, {
      violations: [{ service: 'web', rule: 'privileged', message: 'no' }],
    });
  });

  test('parseErrorBody survives bodies that are not API errors', function (assert) {
    for (const body of ['', '<html>Bad Gateway</html>', '[]', '{"error":1}']) {
      const error = parseErrorBody(502, body);
      assert.strictEqual(error.status, 502);
      assert.strictEqual(error.code, 'unknown');
      assert.strictEqual(error.message, 'HTTP 502');
      assert.deepEqual(error.details, {});
    }
  });
});
