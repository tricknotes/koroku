import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { on } from '@ember/modifier';
import followTail from 'paas-console/modifiers/follow-tail';
import { logSegments, type LogSegment } from 'paas-console/utils/deploy-steps';
import Button from './button';
import Icon from './icon';

interface StepLogSignature {
  Element: HTMLDivElement;
  Args: {
    lines: readonly string[];
    /** Names the log for assistive technology, e.g. "ビルドのログ". */
    label: string;
    /** More lines are still arriving: stay at the end as they do. */
    live?: boolean;
    /** Marks the lines that look like the cause of a failure. */
    highlightErrors?: boolean;
    /** The reader scrolled away from the end of a live log. */
    onUnfollow?: () => unknown;
  };
}

/**
 * The log of one deploy step. It opens at its end, where a failure shows.
 * While `@live` it follows the tail; scrolling up stops that, and a button
 * brings the reader back to the end and to following.
 */
export default class StepLog extends Component<StepLogSignature> {
  @tracked following = true;

  get segments(): LogSegment[] {
    return logSegments(this.args.lines, Boolean(this.args.highlightErrors));
  }

  /** Offered only where following makes a difference. */
  get canResume(): boolean {
    return Boolean(this.args.live) && !this.following;
  }

  setFollowing = (following: boolean) => {
    if (following === this.following) return;
    this.following = following;
    if (!following && this.args.live) void this.args.onUnfollow?.();
  };

  resume = () => {
    this.following = true;
  };

  <template>
    <div class="step-log" ...attributes>
      {{! A focusable region so the log can be scrolled with the keyboard.
          Not role="log": a busy stream would be read out line by line. }}
      <pre
        class="step-log-text"
        tabindex="0"
        role="region"
        aria-label={{@label}}
        data-following={{if this.following "true" "false"}}
        data-test-step-log
        {{followTail
          @lines.length
          following=this.following
          onChange=this.setFollowing
        }}
      >{{#each this.segments key="@index" as |segment|}}{{#if
            segment.error
          }}<mark
              class="step-log-error"
              data-test-error-line
            >{{segment.text}}</mark>{{else}}<span
              class="step-log-lines"
            >{{segment.text}}</span>{{/if}}{{/each}}</pre>
      {{#if this.canResume}}
        <Button
          @size="small"
          class="step-log-follow"
          data-test-follow
          {{on "click" this.resume}}
        ><Icon @name="chevron-down" />末尾を追う</Button>
      {{/if}}
    </div>
  </template>
}
