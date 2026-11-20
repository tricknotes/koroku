import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import type { TOC } from '@ember/component/template-only';
import { fn, uniqueId } from '@ember/helper';
import { on } from '@ember/modifier';
import {
  stepLabel,
  type DeployStep,
  type StepState,
} from 'koroku-console/utils/deploy-steps';
import { formatDuration } from 'koroku-console/utils/format';
import Icon, { type IconName } from './icon';
import StepLog from './step-log';

const ICONS: Record<StepState, IconName | null> = {
  done: 'check',
  running: 'status-deploying',
  failed: 'status-failed',
  canceled: 'status-stopped',
  // Not reached yet: an empty marker.
  pending: null,
};

/** The state in words: never the marker's colour or shape alone. */
const STATE_LABELS: Record<StepState, string> = {
  done: '完了',
  running: '実行中',
  failed: '失敗',
  canceled: 'キャンセル',
  pending: '未実行',
};

function icon(step: DeployStep): IconName | null {
  return ICONS[step.state];
}

function stateLabel(step: DeployStep): string {
  return STATE_LABELS[step.state];
}

function duration(step: DeployStep): string {
  return formatDuration(step.startedAt, step.endedAt);
}

/** A finished step only shows how long it took; the rest say what they are. */
function showsState(step: DeployStep): boolean {
  return step.state !== 'done';
}

function isLive(step: DeployStep): boolean {
  return step.state === 'running';
}

function isFailed(step: DeployStep): boolean {
  return step.state === 'failed';
}

function logLabel(step: DeployStep, rollback?: boolean): string {
  return `${stepLabel(step.key, rollback)}のログ`;
}

/** Name, koroku's own title and the state / duration of one step. */
const StepSummary: TOC<{ Args: { step: DeployStep; rollback?: boolean } }> =
  <template>
    <span class="step-name">{{stepLabel @step.key @rollback}}</span>
    <span class="step-title">{{@step.title}}</span>
    <span class="step-meta" data-test-step-meta>
      {{#if (showsState @step)}}
        <span class="step-state">{{stateLabel @step}}</span>
      {{else}}
        <span class="visually-hidden">{{stateLabel @step}}</span>
      {{/if}}
      {{#if (duration @step)}}
        <span data-test-step-duration>{{duration @step}}</span>
      {{/if}}
    </span>
  </template>;

interface DeploymentStepsSignature {
  Element: HTMLOListElement;
  Args: {
    steps: DeployStep[];
    /** The steps of a rollback, some of which have names of their own. */
    rollback?: boolean;
  };
}

/**
 * The steps of one deployment as a vertical list: state, name, duration and
 * a log that folds away. The step that is running and the one that failed
 * are open by themselves; everything else stays closed until asked for, so
 * a deployment that went well reads as a short list of durations.
 */
export default class DeploymentSteps extends Component<DeploymentStepsSignature> {
  /** What the reader opened or closed by hand, by step id. */
  @tracked private choices: Record<string, boolean> = {};

  isOpen = (step: DeployStep): boolean => {
    if (step.lines.length === 0) return false;
    return (
      this.choices[step.id] ??
      (step.state === 'running' || step.state === 'failed')
    );
  };

  toggle = (step: DeployStep) => {
    this.choices = { ...this.choices, [step.id]: !this.isOpen(step) };
  };

  /** Someone reading further up keeps the step open when it finishes. */
  keepOpen = (step: DeployStep) => {
    this.choices = { ...this.choices, [step.id]: true };
  };

  <template>
    {{#let (uniqueId) as |prefix|}}
      <ol
        class="steps"
        aria-label="デプロイの手順"
        data-test-steps
        ...attributes
      >
        {{#each @steps key="id" as |step|}}
          <li
            class="step step-{{step.state}}"
            data-state={{step.state}}
            data-test-step={{step.key}}
          >
            <span class="step-marker" aria-hidden="true">
              {{#let (icon step) as |name|}}
                {{#if name}}<Icon @name={{name}} />{{/if}}
              {{/let}}
            </span>

            <div class="step-body">
              {{#if step.lines.length}}
                <button
                  type="button"
                  class="step-header"
                  aria-expanded={{if (this.isOpen step) "true" "false"}}
                  aria-controls="{{prefix}}-{{step.id}}"
                  data-test-step-toggle
                  {{on "click" (fn this.toggle step)}}
                >
                  <StepSummary @step={{step}} @rollback={{@rollback}} />
                  <Icon @name="chevron-down" class="step-chevron" />
                </button>
              {{else}}
                <div class="step-header">
                  <StepSummary @step={{step}} @rollback={{@rollback}} />
                </div>
              {{/if}}

              {{#if (this.isOpen step)}}
                <StepLog
                  id="{{prefix}}-{{step.id}}"
                  @lines={{step.lines}}
                  @label={{logLabel step @rollback}}
                  @live={{isLive step}}
                  @highlightErrors={{isFailed step}}
                  @onUnfollow={{fn this.keepOpen step}}
                />
              {{/if}}
            </div>
          </li>
        {{/each}}
      </ol>
    {{/let}}
  </template>
}
