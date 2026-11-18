import Component from '@glimmer/component';
import { on } from '@ember/modifier';
import type Loadable from 'koroku-console/utils/loadable';
import Banner from './banner';
import Button from './button';
import FeatureUnavailable from './feature-unavailable';

interface AsyncViewSignature<T> {
  Args: {
    data: Loadable<T>;
    /** Says what could not be loaded, above the reason. */
    errorTitle?: string;
  };
  Blocks: {
    default: [value: T];
    /** Replaces the plain "loading" line, e.g. with skeleton rows. */
    loading: [];
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
      <Banner @tone="error" @title={{@errorTitle}} data-test-error>
        <:default>{{@data.errorMessage}}</:default>
        <:actions>
          <Button @size="small" {{on "click" @data.reload}}>再読み込み</Button>
        </:actions>
      </Banner>
    {{else if @data.loaded}}
      {{yield this.value}}
    {{else if (has-block "loading")}}
      {{yield to="loading"}}
    {{else}}
      <p class="muted" aria-live="polite" data-test-loading>読み込み中…</p>
    {{/if}}
  </template>
}
