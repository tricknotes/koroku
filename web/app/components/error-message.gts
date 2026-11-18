import type { TOC } from '@ember/component/template-only';
import { errorMessage, fieldErrors } from 'paas-console/utils/errors';
import Banner from './banner';

function detailLines(error: unknown): string[] {
  return Object.entries(fieldErrors(error)).map(
    ([name, reason]) => `${name}: ${reason}`,
  );
}

/**
 * Error banner for the error of a failed action; renders nothing without one.
 * `@showDetails` lists the per-field reasons of a validation failure, for
 * places that have no field to show them next to.
 */
const ErrorMessage: TOC<{
  Args: { error: unknown; showDetails?: boolean };
}> = <template>
  {{#if @error}}
    <Banner @tone="error" data-test-error>
      <span>{{errorMessage @error}}</span>
      {{#if @showDetails}}
        {{#each (detailLines @error) as |line|}}
          <span class="banner-detail" data-test-error-detail>{{line}}</span>
        {{/each}}
      {{/if}}
    </Banner>
  {{/if}}
</template>;

export default ErrorMessage;
