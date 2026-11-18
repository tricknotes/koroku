import { module, test } from 'qunit';
import {
  click,
  fillIn,
  find,
  render,
  settled,
  triggerEvent,
} from '@ember/test-helpers';
import { on } from '@ember/modifier';
import { tracked } from '@glimmer/tracking';
import { setupRenderingTest } from 'koroku-console/tests/helpers';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';

class State {
  @tracked open = false;
  @tracked busy = false;
  confirms = 0;
  cancels = 0;

  show = () => {
    this.open = true;
  };

  confirm = () => {
    this.confirms += 1;
  };

  cancel = () => {
    this.cancels += 1;
    this.open = false;
  };
}

function dialog(): HTMLDialogElement {
  return find('[data-test-dialog]') as HTMLDialogElement;
}

/** What the browser fires for Escape on a modal dialog. */
function pressEscape(): Event {
  const event = new Event('cancel', { cancelable: true });
  dialog().dispatchEvent(event);
  return event;
}

module('Integration | Component | confirm-dialog', function (hooks) {
  setupRenderingTest(hooks);

  async function renderPlain(): Promise<State> {
    const state = new State();
    await render(
      <template>
        <button type="button" data-test-opener {{on "click" state.show}}>
          デプロイ
        </button>
        {{#if state.open}}
          <ConfirmDialog
            @title="本番アプリにデプロイしますか？"
            @confirmLabel="デプロイする"
            @busy={{state.busy}}
            @onConfirm={{state.confirm}}
            @onCancel={{state.cancel}}
          >
            <p data-test-body>本番アプリ blog にデプロイします。</p>
          </ConfirmDialog>
        {{/if}}
      </template>,
    );
    return state;
  }

  async function renderTyped(): Promise<State> {
    const state = new State();
    await render(
      <template>
        <button type="button" data-test-opener {{on "click" state.show}}>
          削除…
        </button>
        {{#if state.open}}
          <ConfirmDialog
            @title="アプリを削除しますか？"
            @confirmLabel="削除する"
            @danger={{true}}
            @requireText="blog"
            @busy={{state.busy}}
            @onConfirm={{state.confirm}}
            @onCancel={{state.cancel}}
          >
            <p>アプリ blog を削除します。元に戻せません。</p>
          </ConfirmDialog>
        {{/if}}
      </template>,
    );
    return state;
  }

  test('it opens as a modal dialog named by its title', async function (assert) {
    await renderPlain();
    assert.dom('[data-test-dialog]').doesNotExist();

    await click('[data-test-opener]');

    assert.true(dialog().open);
    assert.true(dialog().matches(':modal'), 'the rest of the page is inert');
    const titleId = dialog().getAttribute('aria-labelledby') ?? '';
    assert.notStrictEqual(titleId, '');
    assert
      .dom(document.getElementById(titleId))
      .hasText('本番アプリにデプロイしますか？');
    assert.dom('[data-test-body]').includesText('blog');
    assert
      .dom('[data-test-dialog-confirm]')
      .hasText('デプロイする')
      .hasClass('button-primary')
      .doesNotHaveClass('button-danger')
      .isNotDisabled();
  });

  test('the focus moves into the dialog, to the safe choice, and returns to the opener', async function (assert) {
    const state = await renderPlain();
    await click('[data-test-opener]');

    assert.true(
      dialog().contains(document.activeElement),
      'the focus is inside the dialog',
    );
    assert
      .dom('[data-test-dialog-cancel]')
      .isFocused('Enter right away cancels, it does not confirm');

    await click('[data-test-dialog-cancel]');

    assert.dom('[data-test-dialog]').doesNotExist();
    assert.dom('[data-test-opener]').isFocused();
    assert.strictEqual(state.cancels, 1);
    assert.strictEqual(state.confirms, 0);
  });

  test('confirming calls back once and leaves closing to the owner', async function (assert) {
    const state = await renderPlain();
    await click('[data-test-opener]');

    await click('[data-test-dialog-confirm]');

    assert.strictEqual(state.confirms, 1);
    assert.strictEqual(state.cancels, 0);
    assert.true(dialog().open, 'it stays until the owner removes it');
  });

  test('Escape asks to cancel, and the owner decides', async function (assert) {
    const state = await renderPlain();
    await click('[data-test-opener]');

    const event = pressEscape();
    await settled();

    assert.true(
      event.defaultPrevented,
      'the browser does not close it on its own',
    );
    assert.strictEqual(state.cancels, 1);
    assert.dom('[data-test-dialog]').doesNotExist();
    assert.dom('[data-test-opener]').isFocused();
  });

  test('while the action runs, nothing can be confirmed or canceled', async function (assert) {
    const state = await renderPlain();
    await click('[data-test-opener]');
    state.busy = true;
    await settled();

    assert
      .dom('[data-test-dialog-confirm]')
      .isDisabled()
      .hasAttribute('aria-busy', 'true')
      .hasText('実行中…');
    assert.dom('[data-test-dialog-cancel]').isDisabled();

    // Enter in the form and Escape reach the handlers without the buttons.
    await triggerEvent('[data-test-dialog] form', 'submit');
    const event = pressEscape();
    await settled();

    assert.strictEqual(state.confirms, 0, 'no second submit');
    assert.strictEqual(state.cancels, 0);
    assert.true(event.defaultPrevented);
    assert.true(dialog().open, 'it stays open until the action is over');

    state.busy = false;
    await settled();

    assert
      .dom('[data-test-dialog-confirm]')
      .isNotDisabled()
      .hasText('デプロイする');
    assert.dom('[data-test-dialog-cancel]').isNotDisabled();
  });

  test('a typed name gates the confirm button, exactly', async function (assert) {
    const state = await renderTyped();
    await click('[data-test-opener]');

    assert
      .dom('[data-test-dialog-confirm]')
      .isDisabled()
      .hasClass('button-danger');
    assert.dom('[data-test-dialog-input]').isFocused();
    const input = find('[data-test-dialog-input]') as HTMLInputElement;
    assert.dom(`label[for="${input.id}"]`).includesText('blog');
    assert.dom(input).hasAttribute('autocomplete', 'off');

    for (const almost of ['blo', 'Blog', 'blog ', ' blog', 'blogg']) {
      await fillIn('[data-test-dialog-input]', almost);
      assert
        .dom('[data-test-dialog-confirm]')
        .isDisabled(`"${almost}" is not the name`);
    }

    // Enter in the field submits the form even though the button is off.
    await triggerEvent('[data-test-dialog] form', 'submit');
    assert.strictEqual(state.confirms, 0, 'Enter does not get around it');

    await fillIn('[data-test-dialog-input]', 'blog');
    assert.dom('[data-test-dialog-confirm]').isNotDisabled();

    await triggerEvent('[data-test-dialog] form', 'submit');
    assert.strictEqual(state.confirms, 1, 'Enter confirms once it matches');
  });

  test('the typed name does not survive closing the dialog', async function (assert) {
    const state = await renderTyped();
    await click('[data-test-opener]');
    await fillIn('[data-test-dialog-input]', 'blog');
    await click('[data-test-dialog-cancel]');

    await click('[data-test-opener]');

    assert.dom('[data-test-dialog-input]').hasValue('');
    assert.dom('[data-test-dialog-confirm]').isDisabled();
    assert.strictEqual(state.confirms, 0);
  });

  test('a typed name does not confirm while the action runs', async function (assert) {
    const state = await renderTyped();
    await click('[data-test-opener]');
    await fillIn('[data-test-dialog-input]', 'blog');
    state.busy = true;
    await settled();

    assert.dom('[data-test-dialog-confirm]').isDisabled();

    await triggerEvent('[data-test-dialog] form', 'submit');

    assert.strictEqual(state.confirms, 0);
  });
});
