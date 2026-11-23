import { module, test } from 'qunit';
import {
  redeployRef,
  unreachableCommit,
  unreachableCommitTitle,
} from 'koroku-console/utils/unreachable-commit';

// The reasons as the fetch step writes them (internal/gitrepo).
const NOT_REACHABLE =
  'fetch source: commit 9fceb02d is not reachable from "main": a deployment only takes commits on its branch or tag (the commit may have been removed by a force-push, or belong to another branch or to a pull request); deploy from a branch or tag that contains the commit';
const REF_MISSING =
  'fetch source: the repository has no branch or tag named "pull/12/head" to deploy commit 9fceb02d from; deploy from a branch or tag that contains the commit';

module('Unit | Utility | unreachable-commit', function () {
  test('unreachableCommit reads a commit the ref does not reach', function (assert) {
    const commit = unreachableCommit(NOT_REACHABLE);
    assert.deepEqual(commit, {
      sha: '9fceb02d',
      ref: 'main',
      refMissing: false,
    });
    assert.strictEqual(
      unreachableCommitTitle(commit!),
      'コミット 9fceb02d は「main」からたどれないため、デプロイできません',
    );
  });

  test('unreachableCommit reads a ref that is no branch or tag', function (assert) {
    const commit = unreachableCommit(REF_MISSING);
    assert.deepEqual(commit, {
      sha: '9fceb02d',
      ref: 'pull/12/head',
      refMissing: true,
    });
    assert.strictEqual(
      unreachableCommitTitle(commit!),
      'ブランチかタグ「pull/12/head」がリポジトリにないため、デプロイできません',
    );
  });

  test('unreachableCommit leaves other errors alone', function (assert) {
    assert.strictEqual(
      unreachableCommit('fetch source: git fetch: exit status 128'),
      null,
    );
    assert.strictEqual(unreachableCommit(''), null);
    assert.strictEqual(unreachableCommit(undefined), null);
  });

  test('redeployRef is the ref a row was deployed from', function (assert) {
    const history = [
      { id: 45, ref: 'rollback', source_deployment_id: 43 },
      { id: 44, ref: 'main', source_deployment_id: null },
      { id: 43, ref: 'release', source_deployment_id: null },
      { id: 42, ref: '', source_deployment_id: null },
    ];
    assert.strictEqual(redeployRef(history[1]!, history), 'main');
    assert.strictEqual(
      redeployRef(history[3]!, history),
      undefined,
      'a row without a ref leaves the branch to the server',
    );
    assert.strictEqual(
      redeployRef(history[0]!, history),
      'release',
      'a rollback record takes the ref of the release it started',
    );
    assert.strictEqual(
      redeployRef({ ref: 'rollback', source_deployment_id: 7 }, history),
      undefined,
      'never the marker of a rollback',
    );
  });
});
