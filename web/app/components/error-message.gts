import type { TOC } from '@ember/component/template-only';
import { errorMessage, fieldErrors } from 'paas-console/utils/errors';

function detailLines(error: unknown): string[] {
  return Object.entries(fieldErrors(error)).map(
    ([name, reason]) => `${name}: ${reason}`,
  );
}

/**
 * Alert box for the error of a failed action; renders nothing without one.
 * `@showDetails` lists the per-field reasons of a validation failure, for
 * places that have no field to show them next to.
 */
const ErrorMessage: TOC<{
  Args: { error: unknown; showDetails?: boolean };
}> = <template>
  {{#if @error}}
    <div class="alert alert-danger" role="alert" data-test-error>
      <span>{{errorMessage @error}}</span>
      {{#if @showDetails}}
        {{#each (detailLines @error) as |line|}}
          <span class="alert-detail" data-test-error-detail>{{line}}</span>
        {{/each}}
      {{/if}}
    </div>
  {{/if}}
</template>;

export default ErrorMessage;
