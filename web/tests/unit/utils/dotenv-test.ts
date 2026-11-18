import { module, test } from 'qunit';
import { formatDotenv, parseDotenv } from 'koroku-console/utils/dotenv';

module('Unit | Utility | dotenv', function () {
  test('plain lines, comments, blank lines and the export prefix', function (assert) {
    const text = [
      '# a comment',
      '',
      'RAILS_ENV=production',
      '  export PORT = 3000  ',
      'EMPTY=',
      'URL=postgres://app:secret@db/app?sslmode=disable # trailing comment',
      'COLOR=#fff',
    ].join('\n');

    assert.deepEqual(parseDotenv(text), {
      vars: [
        { key: 'RAILS_ENV', value: 'production' },
        { key: 'PORT', value: '3000' },
        { key: 'EMPTY', value: '' },
        { key: 'URL', value: 'postgres://app:secret@db/app?sslmode=disable' },
        { key: 'COLOR', value: '#fff' },
      ],
      issues: [],
    });
  });

  test('quoted values keep spaces, escapes and line breaks', function (assert) {
    const text = [
      'GREETING="hello  world" # said twice',
      'ESCAPED="a\\nb \\"c\\" \\\\ \\x"',
      "LITERAL='$HOME \\n #not a comment'",
      'KEY="-----BEGIN-----',
      'line two',
      '-----END-----"',
      'AFTER=1',
    ].join('\r\n');

    assert.deepEqual(parseDotenv(text), {
      vars: [
        { key: 'GREETING', value: 'hello  world' },
        { key: 'ESCAPED', value: 'a\nb "c" \\ \\x' },
        { key: 'LITERAL', value: '$HOME \\n #not a comment' },
        { key: 'KEY', value: '-----BEGIN-----\nline two\n-----END-----' },
        { key: 'AFTER', value: '1' },
      ],
      issues: [],
    });
  });

  test('problems are reported per line and the rest is still read', function (assert) {
    const text = [
      'OK=1',
      'no equals sign here',
      '1BAD=x',
      '=value',
      'OK=2',
      'QUOTED="x" trailing',
      'LAST=3',
    ].join('\n');

    const { vars, issues } = parseDotenv(text);

    assert.deepEqual(vars, [
      { key: 'OK', value: '1' },
      { key: 'LAST', value: '3' },
    ]);
    assert.deepEqual(
      issues.map((issue) => issue.line),
      [2, 3, 4, 5, 6],
    );
    assert.true(issues[0]?.message.includes('KEY=VALUE'));
    assert.true(issues[1]?.message.includes('英字かアンダースコア'));
    assert.true(issues[2]?.message.includes('キーがありません'));
    assert.true(issues[3]?.message.includes('1 行目と同じ'));
    assert.true(issues[4]?.message.includes('余分な文字'));
  });

  test('an unclosed quote is reported on the line it opens', function (assert) {
    const { vars, issues } = parseDotenv('A=1\nB="never closed\nC=3');

    assert.deepEqual(vars, [{ key: 'A', value: '1' }]);
    assert.deepEqual(issues, [
      { line: 2, message: '引用符が閉じられていません。' },
    ]);
  });

  test('messages never repeat what was typed', function (assert) {
    const secret = 'hunter2';
    const text = [
      `${secret}`,
      `1${secret}=x`,
      `A="${secret}" ${secret}`,
      `B="${secret}`,
    ].join('\n');

    const { issues } = parseDotenv(text);

    assert.strictEqual(issues.length, 4);
    for (const issue of issues) {
      assert.false(issue.message.includes(secret), `line ${issue.line}`);
    }
  });

  test('formatDotenv quotes only what needs it and reads back exactly', function (assert) {
    const vars = [
      { key: 'PLAIN', value: 'postgres://app:pw@db/app?a=b&c=$d' },
      { key: 'EMPTY', value: '' },
      { key: 'SPACED', value: 'hello world' },
      { key: 'HASH', value: 'a #b' },
      { key: 'QUOTES', value: `it's "quoted"` },
      { key: 'SLASH', value: 'C:\\temp\\new' },
      { key: 'LINES', value: 'one\ntwo\r\n\tthree' },
    ];

    const text = formatDotenv(vars);

    assert.strictEqual(
      text.split('\n')[0],
      'PLAIN=postgres://app:pw@db/app?a=b&c=$d',
    );
    assert.strictEqual(text.split('\n')[1], 'EMPTY=');
    assert.strictEqual(text.split('\n')[2], 'SPACED="hello world"');
    assert.strictEqual(
      text.split('\n').length,
      vars.length + 1,
      'one line per variable',
    );
    assert.deepEqual(parseDotenv(text), { vars, issues: [] });
  });
});
