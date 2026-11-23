import { module, test } from 'qunit';

import {
  repositoryMismatch,
  repositoryMismatchTitle,
} from 'koroku-console/utils/repository-mismatch';

const MISMATCH =
  'fetch source: octo/blog is not the repository this app was set up with: ' +
  'GitHub has repository 200 under that name, the app was set up with repository 100. Nothing was fetched. ' +
  "If the app should deploy the repository that has the name now, the owner sets the app's repository again (repo_id 200)";

module('Unit | Utility | repository-mismatch', function () {
  test('repositoryMismatch reads the name and both IDs', function (assert) {
    const mismatch = repositoryMismatch(MISMATCH);
    assert.deepEqual(mismatch, {
      name: 'octo/blog',
      found: '200',
      expected: '100',
    });
    assert.strictEqual(
      repositoryMismatchTitle(mismatch!),
      '「octo/blog」が、アプリに設定したものとは別のリポジトリを指しているため、デプロイできません',
    );
  });

  test('repositoryMismatch leaves other errors alone', function (assert) {
    assert.strictEqual(
      repositoryMismatch(
        'fetch source: cannot confirm that octo/blog is the repository this app was set up with: ' +
          'GitHub responded 403: API rate limit exceeded. Nothing was fetched; deploy again once GitHub answers',
      ),
      null,
      'a check that could not be made is not a mismatch',
    );
    assert.strictEqual(
      repositoryMismatch('fetch source: git fetch: exit status 128'),
      null,
    );
    assert.strictEqual(repositoryMismatch(''), null);
    assert.strictEqual(repositoryMismatch(undefined), null);
  });
});
