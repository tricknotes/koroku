import Component from '@glimmer/component';
import { on } from '@ember/modifier';
import type Loadable from 'paas-console/utils/loadable';
import FeatureUnavailable from './feature-unavailable';

interface AsyncViewSignature<T> {
  Args: {
    data: Loadable<T>;
  };
  Blocks: {
    default: [value: T];
  };
}

/**
 * Renders a `Loadable`: the block once the value is there, otherwise a
 * loading line, the "feature unavailable" placeholder or the error with a
 * retry button.
 */
export default class AsyncView<T> extends Component<AsyncViewSignature<T>> {
  get value(): T {
    return this.args.data.value as T;
  }

  <template>
    {{#if @data.unavailable}}
      <FeatureUnavailable @kind={{@data.unavailable}} />
    {{else if @data.error}}
      <div class="alert alert-danger" role="alert" data-test-error>
        <span>{{@data.errorMessage}}</span>
        <button
          type="button"
          class="button button-small"
          {{on "click" @data.reload}}
        >再読み込み</button>
      </div>
    {{else if @data.loaded}}
      {{yield this.value}}
    {{else}}
      <p class="muted" aria-live="polite" data-test-loading>読み込み中…</p>
    {{/if}}
  </template>
}
