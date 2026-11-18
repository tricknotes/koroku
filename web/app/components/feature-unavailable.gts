import type { TOC } from '@ember/component/template-only';
import {
  UNAVAILABLE_MESSAGES,
  type Unavailability,
} from 'koroku-console/utils/errors';

const HINTS: Record<Unavailability, string> = {
  not_configured:
    '必要な設定（GitHub App、オブジェクトストレージなど）がサーバーにありません。',
  unavailable: 'サーバーがこの機能にまだ対応していません。',
};

function message(kind: Unavailability): string {
  return UNAVAILABLE_MESSAGES[kind];
}

function hint(kind: Unavailability): string {
  return HINTS[kind];
}

/** Placeholder for a feature the backend cannot offer (yet). */
const FeatureUnavailable: TOC<{
  Args: { kind: Unavailability };
}> = <template>
  <div class="placeholder" role="status" data-test-unavailable={{@kind}}>
    <p class="placeholder-title">{{message @kind}}</p>
    <p class="muted">{{hint @kind}}</p>
  </div>
</template>;

export default FeatureUnavailable;
