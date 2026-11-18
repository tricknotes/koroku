import type { TOC } from '@ember/component/template-only';
import { errorMessage } from 'paas-console/utils/errors';

/** Alert box for the error of a failed action; renders nothing without one. */
const ErrorMessage: TOC<{
  Args: { error: unknown };
}> = <template>
  {{#if @error}}
    <p class="alert alert-danger" role="alert" data-test-error>
      {{errorMessage @error}}
    </p>
  {{/if}}
</template>;

export default ErrorMessage;
