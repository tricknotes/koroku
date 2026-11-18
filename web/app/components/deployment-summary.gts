import type { TOC } from '@ember/component/template-only';
import Badge from './badge';
import RelativeTime from './relative-time';
import { firstLine, shortSha } from 'paas-console/utils/format';
import {
  DEPLOYMENT_STATUS_LABELS,
  DEPLOYMENT_STATUS_TONES,
} from 'paas-console/utils/status';
import type { Deployment, DeploymentStatus } from 'paas-console/utils/types';

function label(status: DeploymentStatus): string {
  return DEPLOYMENT_STATUS_LABELS[status] ?? status;
}

function tone(status: DeploymentStatus) {
  return DEPLOYMENT_STATUS_TONES[status] ?? 'muted';
}

function when(deployment: Deployment): string | null {
  return (
    deployment.finished_at ?? deployment.started_at ?? deployment.queued_at
  );
}

/** One line about a deployment: short SHA, message, time, optional status. */
const DeploymentSummary: TOC<{
  Args: { deployment: Deployment | null; showStatus?: boolean };
}> = <template>
  {{#if @deployment}}
    <span class="deployment-summary">
      {{#if @showStatus}}
        <Badge @tone={{tone @deployment.status}}>
          {{label @deployment.status}}
        </Badge>
      {{/if}}
      <code title={{@deployment.commit_sha}}>
        {{shortSha @deployment.commit_sha}}
      </code>
      <span class="deployment-message">
        {{firstLine @deployment.commit_message}}
      </span>
      <span class="muted"><RelativeTime @time={{when @deployment}} /></span>
    </span>
  {{else}}
    <span class="muted">なし</span>
  {{/if}}
</template>;

export default DeploymentSummary;
