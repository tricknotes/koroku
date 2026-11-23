import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { on } from '@ember/modifier';
import { fn, hash } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import { htmlSafe } from '@ember/template';
import type { SafeString } from '@ember/template';
import { guidFor } from '@ember/object/internals';
import { modifier } from 'ember-modifier';
import { schedule } from '@ember/runloop';
import Badge from 'koroku-console/components/badge';
import onDismiss from 'koroku-console/modifiers/on-dismiss';
import { firstLine, shortSha } from 'koroku-console/utils/format';
import {
  groupLabel,
  groupMarkers,
  markerTime,
  refText,
  resultText,
  triggerText,
  type DeployMarker,
  type MarkerGroup,
} from 'koroku-console/utils/deploy-markers';

/** Follows the track's width, which decides which markers share a button. */
const measure = modifier<{
  Element: HTMLElement;
  Args: { Positional: [(width: number) => void] };
}>((element, [report]) => {
  // The observer reports the first width too, after this render: setting
  // tracked state during it is not allowed.
  if (typeof ResizeObserver === 'undefined') return;
  const observer = new ResizeObserver((entries) => {
    const entry = entries[0];
    if (entry) report(entry.contentRect.width);
  });
  observer.observe(element);
  return () => observer.disconnect();
});

/**
 * Listens on a marker's holder: the button and its popup together, so the
 * pointer and focus can move from one to the other without closing it.
 * The holder itself is not interactive; the button inside it is.
 */
const listen = modifier<{
  Element: HTMLElement;
  Args: { Named: Record<string, (event: Event) => void> };
}>((element, _, handlers) => {
  const entries = Object.entries(handlers);
  for (const [name, handler] of entries) {
    element.addEventListener(name, handler);
  }
  return () => {
    for (const [name, handler] of entries) {
      element.removeEventListener(name, handler);
    }
  };
});

// Built from numbers only, so marking them safe is sound.
function at(left: number): SafeString {
  return htmlSafe(`--at: ${left}%`);
}

/** The popup opens towards the middle of the track. */
function side(left: number): string {
  if (left < 25) return 'is-start';
  if (left > 75) return 'is-end';
  return 'is-middle';
}

function isReverted(item: { kind: string }): boolean {
  return item.kind === 'reverted';
}

function count(group: MarkerGroup): number {
  return group.markers.length;
}

function many(group: MarkerGroup): boolean {
  return group.markers.length > 1;
}

interface Signature {
  Element: HTMLElement;
  Args: {
    slug: string;
    markers: DeployMarker[];
    /** The deployment whose release runs now, to say so. */
    liveId: number | null;
  };
}

/**
 * The deployments within the memory history's range, as one row of buttons
 * on the charts' time axis (ARCHITECTURE 9.5). Each button opens, on hover,
 * focus or a tap, what was deployed, by whom and when, with a link to the
 * deployment. Markers too close to press apart share a button.
 */
export default class DeployMarkers extends Component<Signature> {
  @tracked width = 0;
  /** The index of the open group. */
  @tracked open: number | null = null;
  /** Opened by a click or a tap: stays until dismissed. */
  @tracked pinned = false;

  id = guidFor(this);

  get groups(): MarkerGroup[] {
    return groupMarkers(this.args.markers, this.width);
  }

  setWidth = (width: number) => {
    if (Math.abs(width - this.width) >= 1) this.width = width;
  };

  popupId = (index: number): string => `${this.id}-popup-${index}`;

  isOpen = (index: number): boolean => this.open === index;

  isLive = (marker: DeployMarker): boolean =>
    this.args.liveId === marker.deployment.id;

  enter = (index: number, event: Event) => {
    if ((event as PointerEvent).pointerType === 'touch' || this.pinned) return;
    this.open = index;
  };

  leave = (index: number, event: Event) => {
    if ((event as PointerEvent).pointerType === 'touch' || this.pinned) return;
    if (this.open === index) this.open = null;
  };

  focusIn = (index: number) => {
    if (this.open !== index) {
      this.open = index;
      this.pinned = false;
    }
  };

  focusOut = (index: number, event: Event) => {
    const holder = event.currentTarget as HTMLElement;
    const next = (event as FocusEvent).relatedTarget;
    if (next instanceof Node && holder.contains(next)) return;
    // Hiding the popup while focus is inside it fires focusout during that
    // render, where tracked state cannot change: close after it.
    // eslint-disable-next-line ember/no-runloop
    schedule('afterRender', () => {
      if (this.isDestroying || this.isDestroyed) return;
      if (this.open === index) {
        this.open = null;
        this.pinned = false;
      }
    });
  };

  toggle = (index: number) => {
    if (this.open === index && this.pinned) {
      this.open = null;
      this.pinned = false;
    } else {
      this.open = index;
      this.pinned = true;
    }
  };

  key = (event: Event) => {
    if ((event as KeyboardEvent).key !== 'Escape' || this.open === null) return;
    event.stopPropagation();
    const holder = event.currentTarget as HTMLElement;
    this.open = null;
    this.pinned = false;
    holder.querySelector<HTMLElement>('button')?.focus();
  };

  dismiss = () => {
    if (this.pinned) {
      this.open = null;
      this.pinned = false;
    }
  };

  <template>
    <div class="deploy-markers" data-test-deploy-markers ...attributes>
      <span class="deploy-markers-label" aria-hidden="true">デプロイ</span>
      <ul
        class="deploy-markers-track"
        aria-label="期間内のデプロイ"
        {{measure this.setWidth}}
        {{onDismiss this.dismiss}}
      >
        {{#each this.groups as |group index|}}
          <li
            class="deploy-marker-holder"
            data-test-deploy-marker-group={{index}}
            {{listen
              pointerenter=(fn this.enter index)
              pointerleave=(fn this.leave index)
              focusin=(fn this.focusIn index)
              focusout=(fn this.focusOut index)
              keydown=this.key
            }}
          >
            <button
              type="button"
              class="deploy-marker
                {{if (isReverted group) 'is-reverted'}}
                {{if (many group) 'is-group'}}"
              style={{at group.left}}
              aria-label={{groupLabel group}}
              aria-expanded={{if (this.isOpen index) "true" "false"}}
              aria-controls={{this.popupId index}}
              data-test-deploy-marker
              {{on "click" (fn this.toggle index)}}
            >
              {{#if (many group)}}
                <span class="deploy-marker-count">{{count group}}</span>
              {{else}}
                <svg
                  class="deploy-marker-glyph"
                  viewBox="0 0 10 8"
                  aria-hidden="true"
                ><path d="M1 1h8L5 7z" /></svg>
              {{/if}}
            </button>
            <div
              id={{this.popupId index}}
              class="deploy-marker-popup {{side group.left}}"
              style={{at group.left}}
              hidden={{if (this.isOpen index) false true}}
              data-test-deploy-marker-popup
            >
              <ul class="deploy-marker-list">
                {{#each group.markers as |marker|}}
                  <li
                    class="deploy-marker-item"
                    data-test-deploy-marker-item={{marker.deployment.id}}
                  >
                    <p class="deploy-marker-head">
                      <svg
                        class="deploy-marker-key
                          {{if (isReverted marker) 'is-reverted'}}"
                        viewBox="0 0 10 8"
                        aria-hidden="true"
                      ><path d="M1 1h8L5 7z" /></svg>
                      <code
                        class="deploy-marker-sha"
                        title={{marker.deployment.commit_sha}}
                      >{{shortSha marker.deployment.commit_sha}}</code>
                      <span class="deploy-marker-message">{{firstLine
                          marker.deployment.commit_message
                        }}</span>
                    </p>
                    <dl class="deploy-marker-facts">
                      <div class="deploy-marker-fact">
                        <dt>{{if
                            marker.deployment.source_deployment_id
                            "戻す先"
                            "ブランチ"
                          }}</dt>
                        <dd data-test-deploy-marker-ref>{{refText
                            marker.deployment
                          }}</dd>
                      </div>
                      <div class="deploy-marker-fact">
                        <dt>始めた</dt>
                        <dd data-test-deploy-marker-trigger>{{triggerText
                            marker.deployment
                          }}</dd>
                      </div>
                      <div class="deploy-marker-fact">
                        <dt>完了</dt>
                        <dd data-test-deploy-marker-time>{{markerTime
                            marker
                          }}</dd>
                      </div>
                      <div class="deploy-marker-fact">
                        <dt>結果</dt>
                        <dd
                          class={{if (isReverted marker) "is-reverted"}}
                          data-test-deploy-marker-result
                        >
                          {{resultText marker}}
                          {{#if (this.isLive marker)}}
                            <Badge
                              @tone="ok"
                              data-test-deploy-marker-live
                            >稼働中</Badge>
                          {{/if}}
                        </dd>
                      </div>
                    </dl>
                    <LinkTo
                      @route="app.deploy"
                      @model={{@slug}}
                      @query={{hash deployment=marker.deployment.id}}
                      class="deploy-marker-link"
                      data-test-deploy-marker-link
                    >デプロイ #{{marker.deployment.id}} を開く</LinkTo>
                  </li>
                {{/each}}
              </ul>
            </div>
          </li>
        {{/each}}
      </ul>
    </div>
  </template>
}
