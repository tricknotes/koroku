import type { TOC } from '@ember/component/template-only';
import { STATUS_LABELS, type StatusKind } from 'paas-console/utils/status';
import Icon, { type IconName } from './icon';

const ICONS: Record<StatusKind, IconName> = {
  running: 'status-running',
  deploying: 'status-deploying',
  failed: 'status-failed',
  stopped: 'status-stopped',
  pending: 'status-pending',
};

function icon(kind: StatusKind): IconName {
  return ICONS[kind];
}

function label(kind: StatusKind): string {
  return STATUS_LABELS[kind];
}

/**
 * One of the five states as an icon plus a label, never colour alone. The
 * icon carries the state colour and has its own shape per state; the
 * "deploying" icon spins unless the reader asked for reduced motion.
 *
 * `@label` replaces the default word where the state belongs to something
 * more specific (a deployment that "succeeded", an app "not deployed yet").
 * `@quiet` drops the colour for results that need no attention.
 */
const StatusIndicator: TOC<{
  Element: HTMLSpanElement;
  Args: { kind: StatusKind; label?: string; quiet?: boolean };
}> = <template>
  <span
    class="status status-{{@kind}} {{if @quiet 'status-quiet'}}"
    data-status={{@kind}}
    ...attributes
  >
    <Icon @name={{icon @kind}} />
    <span>{{if @label @label (label @kind)}}</span>
  </span>
</template>;

export default StatusIndicator;
