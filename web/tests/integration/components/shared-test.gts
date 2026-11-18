import { module, test } from 'qunit';
import { click, fillIn, find, render, settled } from '@ember/test-helpers';
import { on } from '@ember/modifier';
import { tracked } from '@glimmer/tracking';
import { setupRenderingTest } from 'paas-console/tests/helpers';
import { installFakes, type Fakes } from 'paas-console/tests/helpers/fakes';
import Banner from 'paas-console/components/banner';
import Button, { buttonClass } from 'paas-console/components/button';
import CopyButton from 'paas-console/components/copy-button';
import EmptyState from 'paas-console/components/empty-state';
import EnvironmentBadge from 'paas-console/components/environment-badge';
import FormField from 'paas-console/components/form-field';
import RelativeTime from 'paas-console/components/relative-time';
import RowList from 'paas-console/components/row-list';
import StatusIndicator from 'paas-console/components/status-indicator';
import { STATUS_LABELS, type StatusKind } from 'paas-console/utils/status';

const KINDS = Object.keys(STATUS_LABELS) as StatusKind[];

module('Integration | Component | shared components', function (hooks) {
  setupRenderingTest(hooks);

  let fakes: Fakes;

  hooks.beforeEach(function () {
    fakes = installFakes(this.owner);
  });

  module('Button', function () {
    test('variants and sizes map to classes; the default type is "button"', async function (assert) {
      await render(
        <template>
          <Button data-test-default>保存</Button>
          <Button @variant="primary" data-test-primary>デプロイ</Button>
          <Button @variant="danger" @size="small" data-test-danger>
            削除
          </Button>
          <Button @type="submit" data-test-submit>送信</Button>
        </template>,
      );

      assert
        .dom('[data-test-default]')
        .hasClass('button')
        .doesNotHaveClass('button-primary')
        .hasAttribute('type', 'button')
        .hasText('保存');
      assert.dom('[data-test-primary]').hasClass('button-primary');
      assert
        .dom('[data-test-danger]')
        .hasClass('button-danger')
        .hasClass('button-small');
      assert.dom('[data-test-submit]').hasAttribute('type', 'submit');
      assert.strictEqual(buttonClass('primary'), 'button button-primary');
      assert.strictEqual(buttonClass(), 'button');
    });

    test('a busy button is disabled, announces it and shows a spinner', async function (assert) {
      class State {
        @tracked busy = false;
        clicks = 0;
        click = () => {
          this.clicks += 1;
        };
      }
      const state = new State();

      await render(
        <template>
          <Button
            @busy={{state.busy}}
            @busyLabel="保存中…"
            {{on "click" state.click}}
          >保存</Button>
        </template>,
      );

      assert.dom('button').isNotDisabled().doesNotHaveAttribute('aria-busy');
      assert.dom('button svg').doesNotExist();

      state.busy = true;
      await settled();

      assert
        .dom('button')
        .isDisabled()
        .hasAttribute('aria-busy', 'true')
        .hasText('保存中…');
      assert.dom('button svg').hasClass('icon-spin');

      state.busy = false;
      await settled();
      await click('button');

      assert.strictEqual(state.clicks, 1);
    });

    test('@disabled disables it without the busy state', async function (assert) {
      await render(
        <template>
          <Button @disabled={{true}}>保存</Button>
        </template>,
      );

      assert.dom('button').isDisabled().doesNotHaveAttribute('aria-busy');
    });
  });

  module('StatusIndicator', function () {
    test('each of the five states has its own icon and a label', async function (assert) {
      await render(
        <template>
          {{#each KINDS as |kind|}}
            <StatusIndicator @kind={{kind}} data-test-status={{kind}} />
          {{/each}}
        </template>,
      );

      const expected: Record<StatusKind, string> = {
        running: '稼働中',
        deploying: 'デプロイ中',
        failed: '失敗',
        stopped: '停止',
        pending: '確認待ち',
      };
      const icons = new Set<string | null>();
      for (const kind of KINDS) {
        const selector = `[data-test-status="${kind}"]`;
        assert.dom(selector).hasText(expected[kind]).hasClass(`status-${kind}`);
        assert.dom(`${selector} svg`).hasAttribute('aria-hidden', 'true');
        icons.add(find(`${selector} svg`)?.getAttribute('data-icon') ?? null);
      }
      assert.strictEqual(icons.size, 5, 'no two states share an icon');
      assert
        .dom('[data-test-status="deploying"] svg')
        .hasClass('icon-spin', 'only "deploying" moves');
      assert.dom('.icon-spin').exists({ count: 1 });
    });

    test('the label can be replaced and the colour dropped', async function (assert) {
      await render(
        <template>
          <StatusIndicator @kind="running" @label="成功" @quiet={{true}} />
        </template>,
      );

      assert
        .dom('.status')
        .hasText('成功')
        .hasClass('status-running')
        .hasClass('status-quiet');
    });
  });

  module('EnvironmentBadge', function () {
    test('production and staging are labelled and styled apart', async function (assert) {
      await render(
        <template>
          <EnvironmentBadge @environment="production" data-test-prod />
          <EnvironmentBadge @environment="staging" data-test-staging />
        </template>,
      );

      assert
        .dom('[data-test-prod]')
        .hasText('本番')
        .hasClass('badge-env-production');
      assert
        .dom('[data-test-staging]')
        .hasText('ステージング')
        .hasClass('badge-env-staging');
    });
  });

  module('Banner', function () {
    test('tones set the role and the icon', async function (assert) {
      await render(
        <template>
          <Banner data-test-info>お知らせ</Banner>
          <Banner @tone="warning" @title="要対応" data-test-warning>
            詳細
          </Banner>
          <Banner @tone="error" data-test-error>
            <:default>失敗しました</:default>
            <:actions><Button>再試行</Button></:actions>
          </Banner>
        </template>,
      );

      assert
        .dom('[data-test-info]')
        .hasAttribute('role', 'status')
        .hasClass('banner-info')
        .hasText('お知らせ');
      assert.dom('[data-test-info] svg').hasAttribute('data-icon', 'info');
      assert.dom('[data-test-info] .banner-actions').doesNotExist();

      assert.dom('[data-test-warning]').hasAttribute('role', 'status');
      assert.dom('[data-test-warning] .banner-title').hasText('要対応');
      assert
        .dom('[data-test-warning] svg')
        .hasAttribute('data-icon', 'warning');

      assert.dom('[data-test-error]').hasAttribute('role', 'alert');
      assert.dom('[data-test-error] svg').hasAttribute('data-icon', 'error');
      assert.dom('[data-test-error] .banner-actions button').hasText('再試行');
    });
  });

  module('EmptyState', function () {
    test('it shows the title, the description and the one action', async function (assert) {
      await render(
        <template>
          <EmptyState
            @title="ドメインはまだありません"
            @description="独自ドメインを追加できます。"
          >
            <:action><Button @variant="primary">追加</Button></:action>
          </EmptyState>
        </template>,
      );

      assert.dom('.empty-state-title').hasText('ドメインはまだありません');
      assert
        .dom('.empty-state-description')
        .hasText('独自ドメインを追加できます。');
      assert.dom('.empty-state-action button').exists({ count: 1 });
    });
  });

  module('FormField', function () {
    test('the label, hint and error are tied to the control', async function (assert) {
      class State {
        @tracked error: string | null = null;
      }
      const state = new State();

      await render(
        <template>
          <FormField
            @label="Slug"
            @hint="英小文字で始めます"
            @error={{state.error}}
            as |field|
          >
            <input
              id={{field.id}}
              type="text"
              aria-describedby={{field.describedBy}}
              aria-invalid={{field.invalid}}
            />
          </FormField>
        </template>,
      );

      const control = find('input') as HTMLInputElement;
      const labelElement = find('label') as HTMLLabelElement;
      const hintElement = find('.field-hint') as HTMLElement;

      assert.dom(labelElement).hasText('Slug');
      assert.strictEqual(
        labelElement.htmlFor,
        control.id,
        'the label names the input',
      );
      assert.dom(hintElement).hasText('英小文字で始めます');
      assert.dom(control).hasAttribute('aria-describedby', hintElement.id);
      assert.dom(control).doesNotHaveAttribute('aria-invalid');
      assert.dom('[data-test-field-error]').doesNotExist();

      state.error = 'この slug はすでに使われています。';
      await settled();

      const errorElement = find('.field-error') as HTMLElement;
      assert.dom(errorElement).hasText('この slug はすでに使われています。');
      assert
        .dom(control)
        .hasAttribute('aria-invalid', 'true')
        .hasAttribute(
          'aria-describedby',
          `${hintElement.id} ${errorElement.id}`,
        );

      await fillIn(control, 'blog');
      assert.dom(control).hasValue('blog');
    });

    test('the label can carry markup', async function (assert) {
      await render(
        <template>
          <FormField>
            <:label>確認のため <code>blog</code> と入力</:label>
            <:default as |field|>
              <input id={{field.id}} type="text" />
            </:default>
          </FormField>
        </template>,
      );

      assert.dom('label code').hasText('blog');
      assert.dom('input').doesNotHaveAttribute('aria-describedby');
    });
  });

  module('RelativeTime', function () {
    test('relative text, absolute JST time on hover, and it follows the clock', async function (assert) {
      const clock = this.owner.lookup('service:clock');
      clock.now = Date.parse('2026-11-18T03:10:00Z');

      await render(
        <template><RelativeTime @time="2026-11-18T03:07:00Z" /></template>,
      );

      assert
        .dom('time')
        .hasText('3 分前')
        .hasAttribute('datetime', '2026-11-18T03:07:00Z')
        .hasAttribute('title', '2026/11/18 12:07 JST');

      clock.now = Date.parse('2026-11-18T05:10:00Z');
      await settled();

      assert.dom('time').hasText('2 時間前');
    });

    test('the clock advances with the poller', async function (assert) {
      const clock = this.owner.lookup('service:clock');
      clock.now = 0;

      await fakes.poller.tick();

      assert.true(clock.now > 0);
    });

    test('nothing is rendered without a time', async function (assert) {
      await render(<template><RelativeTime @time={{null}} /></template>);

      assert.dom('time').doesNotExist();
    });
  });

  module('CopyButton', function () {
    test('it copies the text and confirms it', async function (assert) {
      await render(
        <template>
          <CopyButton @text="blog.dev.example.com" @label="CNAME の値" />
        </template>,
      );

      assert
        .dom('[data-test-copy]')
        .hasAttribute('aria-label', 'CNAME の値をコピー');
      assert.dom('[data-test-copy] svg').hasAttribute('data-icon', 'copy');
      assert.dom('[role="status"]').hasText('');

      await click('[data-test-copy]');

      assert.deepEqual(fakes.browser.copied, ['blog.dev.example.com']);
      assert.dom('[data-test-copy]').hasClass('is-copied');
      assert.dom('[data-test-copy] svg').hasAttribute('data-icon', 'check');
      assert.dom('[role="status"]').hasText('コピーしました');
    });

    test('a refused clipboard is reported', async function (assert) {
      fakes.browser.clipboardBlocked = true;

      await render(<template><CopyButton @text="secret" /></template>);
      await click('[data-test-copy]');

      assert.deepEqual(fakes.browser.copied, []);
      assert.dom('[data-test-copy]').doesNotHaveClass('is-copied');
      assert.dom('[role="status"]').hasText('コピーできませんでした');
    });
  });

  module('RowList', function () {
    test('a labelled list with a visual-only header', async function (assert) {
      await render(
        <template>
          <RowList @label="アプリ" class="app-rows">
            <:header><span>状態</span><span>アプリ</span></:header>
            <:rows as |Row|>
              <Row data-test-row="a">a</Row>
              <Row data-test-row="b">b</Row>
            </:rows>
          </RowList>
        </template>,
      );

      assert.dom('.row-list').hasClass('app-rows');
      assert.dom('ul').hasAttribute('aria-label', 'アプリ');
      assert.dom('ul > li.row').exists({ count: 2 });
      assert
        .dom('.row-list-header')
        .hasAttribute('aria-hidden', 'true')
        .includesText('状態');
    });
  });
});
