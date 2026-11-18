import { module, test } from 'qunit';
import { find, render } from '@ember/test-helpers';
import { setupRenderingTest } from 'koroku-console/tests/helpers';
import {
  accessibilityProblems,
  accessibleName,
  bareStatuses,
  brokenReferences,
  duplicateIds,
  unlabelledGraphics,
  unnamedControls,
} from 'koroku-console/tests/helpers/a11y';
import CopyButton from 'koroku-console/components/copy-button';
import FormField from 'koroku-console/components/form-field';
import StatusIndicator from 'koroku-console/components/status-indicator';
import TabSection from 'koroku-console/components/tab-section';
import { installFakes } from 'koroku-console/tests/helpers/fakes';

/**
 * Puts markup on the page that no template of the console would contain
 * (the template linter refuses most of it): written as a string on purpose.
 */
async function mount(html: string): Promise<void> {
  await render(
    <template>
      <div data-stage></div>
    </template>,
  );
  (find('[data-stage]') as HTMLElement).innerHTML = html;
}

function reported(problems: string[], name: string): boolean {
  return problems.some((problem) => problem.includes(`data-case="${name}"`));
}

// The accessibility sweep over the screens is only worth something if the
// checks behind it notice a problem. These feed them markup that is wrong
// on purpose.
module('Integration | Helper | a11y checks', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    installFakes(this.owner);
  });

  test('markup as the console writes it has no problems', async function (assert) {
    await render(
      <template>
        <CopyButton @text="value" @label="CNAME の値" />
        <StatusIndicator @kind="failed" />
        <FormField @label="ホスト名" @hint="例: www.example.org" as |field|>
          <input
            id={{field.id}}
            type="text"
            aria-describedby={{field.describedBy}}
          />
        </FormField>
        <TabSection @title="環境変数">
          <button type="button">変数を追加</button>
        </TabSection>
      </template>,
    );

    assert.deepEqual(accessibilityProblems(), []);
    assert.strictEqual(
      accessibleName(find('[data-test-copy]') as Element),
      'CNAME の値をコピー',
      'an icon-only button is named by its label',
    );
    assert.strictEqual(accessibleName(find('input') as Element), 'ホスト名');
    assert.strictEqual(accessibleName(find('section') as Element), '環境変数');
  });

  test('a control without a name is reported', async function (assert) {
    const cases: Record<string, string> = {
      'icon-only':
        '<button type="button"><svg aria-hidden="true"></svg></button>',
      'hidden-text':
        '<button type="button"><span aria-hidden="true">×</span></button>',
      'empty-label':
        '<button type="button" aria-label=" "><svg aria-hidden="true"></svg></button>',
      'empty-link': '<a href="/somewhere"></a>',
      'placeholder-only': '<input type="text" placeholder="KEY">',
      select: '<select><option>main</option></select>',
      textarea: '<textarea></textarea>',
      listbox: '<div role="listbox" tabindex="0"></div>',
      nav: '<nav><a href="/">アプリ</a></nav>',
      dialog: '<dialog><p>本当に削除しますか？</p></dialog>',
    };
    await mount(
      Object.entries(cases)
        .map(([name, html]) => html.replace('>', ` data-case="${name}">`))
        .join(''),
    );

    const problems = unnamedControls();

    for (const name of Object.keys(cases)) {
      assert.true(reported(problems, name), `${name} is reported`);
    }
    assert.strictEqual(
      problems.length,
      Object.keys(cases).length,
      'and nothing that has a name',
    );
  });

  test('a name can come from a label, the text, a title or another element', async function (assert) {
    await mount(`
      <label for="named-input">キー</label><input id="named-input" type="text">
      <label><input type="checkbox"> volume も削除する</label>
      <button type="button"><svg aria-hidden="true"></svg><span class="visually-hidden">閉じる</span></button>
      <button type="button" title="値を表示"><svg aria-hidden="true"></svg></button>
      <a href="/"><img src="data:," alt="koroku"></a>
      <h2 id="named-title">ログ</h2><div role="region" tabindex="0" aria-labelledby="named-title"></div>
    `);

    assert.deepEqual(unnamedControls(), []);
  });

  test('references to nothing and repeated ids are reported', async function (assert) {
    await mount(`
      <input type="text" aria-label="キー" aria-describedby="known-hint gone-hint">
      <span id="known-hint">英字で始めます</span>
      <section aria-labelledby="gone-title"></section>
      <label for="gone-input">値</label>
      <p id="twice">a</p>
      <p id="twice">b</p>
    `);

    const references = brokenReferences().join('\n');
    assert.strictEqual(brokenReferences().length, 3);
    assert.true(references.includes('gone-hint'));
    assert.true(references.includes('gone-title'));
    assert.true(references.includes('gone-input'));
    assert.false(
      references.includes('"known-hint" has no target'),
      'an id that exists is fine',
    );
    assert.deepEqual(duplicateIds(), ['id "twice" is used more than once']);
  });

  test('graphics that are neither hidden nor named, and states without icon or words', async function (assert) {
    await mount(`
      <svg viewBox="0 0 16 16" data-case="bare"></svg>
      <svg viewBox="0 0 16 16" aria-hidden="true"></svg>
      <svg viewBox="0 0 16 16" aria-label="稼働中"></svg>
      <img src="data:," data-case="no-alt">
      <img src="data:," alt="">
      <span class="status" data-status="failed" data-case="colour-only"></span>
      <span class="status" data-status="failed" data-case="icon-only"><svg aria-hidden="true"></svg></span>
      <span class="status" data-status="failed" data-case="words-only">失敗</span>
      <span class="status" data-status="failed"><svg aria-hidden="true"></svg><span>失敗</span></span>
    `);

    const graphics = unlabelledGraphics();
    assert.strictEqual(graphics.length, 2);
    assert.true(reported(graphics, 'bare'));
    assert.true(reported(graphics, 'no-alt'));

    const statuses = bareStatuses();
    assert.strictEqual(statuses.length, 3);
    assert.true(reported(statuses, 'colour-only'));
    assert.true(reported(statuses, 'icon-only'));
    assert.true(reported(statuses, 'words-only'));
  });
});
