import { module, test } from 'qunit';
import { click, render } from '@ember/test-helpers';
import { on } from '@ember/modifier';
import { tracked } from '@glimmer/tracking';
import { setupRenderingTest } from 'koroku-console/tests/helpers';
import ActionMenu from 'koroku-console/components/action-menu';

class State {
  @tracked busy = false;
  runs = 0;

  run = () => {
    this.runs += 1;
    this.busy = true;
  };
}

module('Integration | Component | action-menu', function (hooks) {
  setupRenderingTest(hooks);

  // An item carries only its own click handler; the menu closes from the
  // panel once the click reaches it. A second handler on the item that
  // closed the menu would be dropped by a real click: the browser renders
  // between the listeners and the item leaves the page.
  test('an item runs its own handler and the menu closes', async function (assert) {
    const state = new State();
    await render(
      <template>
        <ActionMenu @label="その他の操作">
          <button
            type="button"
            class="menu-item"
            disabled={{state.busy}}
            data-test-item
            {{on "click" state.run}}
          >再起動</button>
        </ActionMenu>
      </template>,
    );

    await click('[data-test-action-menu]');
    assert.dom('[data-test-action-menu-panel]').exists();

    await click('[data-test-item]');
    assert.strictEqual(state.runs, 1, 'the item ran');
    assert
      .dom('[data-test-action-menu-panel]')
      .doesNotExist('closed, even though running disabled the item');
  });

  test('a click on the panel outside an item keeps the menu open', async function (assert) {
    await render(
      <template>
        <ActionMenu @label="その他の操作">
          <p data-test-note>説明</p>
        </ActionMenu>
      </template>,
    );

    await click('[data-test-action-menu]');
    await click('[data-test-note]');
    assert.dom('[data-test-action-menu-panel]').exists();
  });
});
