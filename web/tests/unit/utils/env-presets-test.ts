import { module, test } from 'qunit';
import {
  EnvDraft,
  frameworkLabel,
  presetReason,
} from 'koroku-console/utils/env-presets';
import type { EnvPreset } from 'koroku-console/utils/types';

function preset(overrides: Partial<EnvPreset>): EnvPreset {
  return {
    key: 'RAILS_ENV',
    value: 'production',
    generated: false,
    framework: 'rails',
    file: 'Gemfile',
    match: 'rails',
    ...overrides,
  };
}

module('Unit | Utility | env-presets', function () {
  test('reasons read like a sentence', function (assert) {
    assert.strictEqual(presetReason(preset({})), 'Gemfile に rails があるため');
    assert.strictEqual(
      presetReason(preset({ file: 'package.json', match: '' })),
      'package.json があるため',
    );
    assert.strictEqual(
      presetReason(preset({ file: 'mix.exs', match: 'phoenix' })),
      'mix.exs の依存に phoenix があるため',
    );
    assert.strictEqual(
      presetReason(
        preset({
          file: 'config/environments/production.rb',
          match: 'RAILS_LOG_TO_STDOUT',
        }),
      ),
      'config/environments/production.rb が RAILS_LOG_TO_STDOUT を読むため',
    );
    assert.strictEqual(frameworkLabel('node'), 'Node.js');
    assert.strictEqual(frameworkLabel('unknown'), 'unknown');
  });

  test('detecting again keeps what the user touched', function (assert) {
    const draft = new EnvDraft();
    draft.applyPresets([
      preset({ key: 'RAILS_ENV' }),
      preset({ key: 'RACK_ENV' }),
      preset({ key: 'SECRET_KEY_BASE', value: 'first', generated: true }),
    ]);
    const [railsEnv, rackEnv, secret] = draft.rows;
    draft.setValue(railsEnv!, 'staging');
    draft.remove(rackEnv!);
    const own = draft.add();
    draft.setKey(own, 'NODE_ENV');
    draft.setValue(own, 'production');
    assert.true(secret!.masked, 'a generated secret starts hidden');

    draft.applyPresets([
      preset({ key: 'RAILS_ENV' }),
      preset({ key: 'RACK_ENV' }),
      preset({ key: 'SECRET_KEY_BASE', value: 'second', generated: true }),
      preset({ key: 'NODE_ENV', value: 'development' }),
    ]);

    assert.deepEqual(draft.vars, [
      { key: 'SECRET_KEY_BASE', value: 'second' },
      { key: 'RAILS_ENV', value: 'staging' },
      { key: 'NODE_ENV', value: 'production' },
    ]);

    draft.reset();
    draft.applyPresets([preset({ key: 'RACK_ENV' })]);
    assert.deepEqual(
      draft.vars.map((v) => v.key),
      ['RACK_ENV'],
      'another repository starts afresh',
    );
  });

  test('keys follow the API rules', function (assert) {
    const draft = new EnvDraft();
    draft.applyPresets([preset({ key: 'RAILS_ENV' })]);
    const row = draft.add();
    assert.strictEqual(draft.keyError(row), undefined, 'a blank row is fine');
    assert.true(draft.valid);

    draft.setValue(row, 'x');
    assert.strictEqual(draft.keyError(row), 'キーを入力してください。');
    for (const [key, error] of [
      ['1ABC', '英字かアンダースコアで始まる'],
      ['KOROKU_COMMIT_SHA', 'koroku が使う'],
      ['RAILS_ENV', '重複'],
    ] as const) {
      draft.setKey(row, key);
      assert.true(draft.keyError(row)?.includes(error), key);
      assert.false(draft.valid);
    }
    draft.setKey(row, ' PORT ');
    assert.true(draft.valid);
    assert.deepEqual(draft.vars[1], { key: 'PORT', value: 'x' });
  });
});
