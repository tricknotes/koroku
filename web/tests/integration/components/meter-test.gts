import { module, test } from 'qunit';
import { find, render } from '@ember/test-helpers';
import { setupRenderingTest } from 'paas-console/tests/helpers';
import { installFakes } from 'paas-console/tests/helpers/fakes';
import Meter from 'paas-console/components/meter';
import TimeUntil from 'paas-console/components/time-until';

module('Integration | Component | meter and time-until', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    installFakes(this.owner);
  });

  test('Meter exposes the value to assistive technology', async function (assert) {
    await render(
      <template>
        <Meter @label="ディスクの使用率" @percent={{41.26}} @mark={{85}} />
      </template>,
    );

    assert
      .dom('[role="meter"]')
      .hasAttribute('aria-label', 'ディスクの使用率')
      .hasAttribute('aria-valuemin', '0')
      .hasAttribute('aria-valuemax', '100')
      .hasAttribute('aria-valuenow', '41.3')
      .hasAttribute('aria-valuetext', '41.3%')
      .doesNotHaveClass('is-danger');
    assert.strictEqual(
      (find('.meter-bar-fill') as HTMLElement).style.width,
      '41.3%',
    );
    assert.strictEqual(
      (find('.meter-bar-mark') as HTMLElement).style.left,
      '85%',
    );
  });

  test('Meter clamps what is out of range and takes a tone', async function (assert) {
    await render(
      <template>
        <Meter @label="over" @percent={{140}} @tone="danger" data-test-over />
        <Meter @label="under" @percent={{-3}} @tone="warn" data-test-under />
      </template>,
    );

    assert
      .dom('[data-test-over]')
      .hasAttribute('aria-valuenow', '100')
      .hasClass('is-danger');
    assert
      .dom('[data-test-under]')
      .hasAttribute('aria-valuenow', '0')
      .hasClass('is-warn');
    assert.dom('.meter-bar-mark').doesNotExist('no mark unless asked for');
  });

  test('TimeUntil shows the distance and the absolute time on hover', async function (assert) {
    const soon = new Date(Date.now() + 3 * 60 * 60 * 1000 + 5000).toISOString();

    await render(
      <template>
        <TimeUntil @time={{soon}} data-test-soon />
        <TimeUntil @time="2026-11-18T18:00:00Z" data-test-fixed />
        <span data-test-none><TimeUntil @time={{null}} /></span>
      </template>,
    );

    assert.dom('[data-test-soon]').hasText('3 時間後');
    assert
      .dom('[data-test-fixed]')
      .hasAttribute('datetime', '2026-11-18T18:00:00Z')
      .hasAttribute('title', '2026/11/19 03:00 JST');
    assert.dom('[data-test-none] time').doesNotExist();
  });
});
