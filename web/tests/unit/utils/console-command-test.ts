import { module, test } from 'qunit';
import {
  consoleCommand,
  shellQuote,
} from 'koroku-console/utils/console-command';

module('Unit | Utility | console-command', function () {
  test('without a command it opens a shell', function (assert) {
    assert.strictEqual(
      consoleCommand('blog', 'web', '  '),
      'sudo koroku run blog web',
    );
  });

  test('a plain command is appended as typed', function (assert) {
    assert.strictEqual(
      consoleCommand('blog', 'web', 'bin/rails console'),
      'sudo koroku run blog web bin/rails console',
    );
    assert.strictEqual(
      consoleCommand('blog', 'db', 'psql -U postgres app'),
      'sudo koroku run blog db psql -U postgres app',
    );
    // Balanced quotes are removed by the host shell the same way.
    assert.strictEqual(
      consoleCommand('blog', 'web', "bin/rails runner 'puts User.count'"),
      "sudo koroku run blog web bin/rails runner 'puts User.count'",
    );
  });

  test('what the host shell would interpret goes to sh -c', function (assert) {
    assert.strictEqual(
      consoleCommand('blog', 'web', 'echo $DATABASE_URL'),
      "sudo koroku run blog web sh -c 'echo $DATABASE_URL'",
    );
    assert.strictEqual(
      consoleCommand('blog', 'web', 'ls | wc -l'),
      "sudo koroku run blog web sh -c 'ls | wc -l'",
    );
    assert.strictEqual(
      consoleCommand('blog', 'web', "echo it's"),
      "sudo koroku run blog web sh -c 'echo it'\\''s'",
    );
  });

  test('shellQuote', function (assert) {
    assert.strictEqual(shellQuote("a'b"), "'a'\\''b'");
    assert.strictEqual(shellQuote(''), "''");
  });
});
