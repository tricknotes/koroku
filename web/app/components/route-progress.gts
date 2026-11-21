import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { registerDestructor } from '@ember/destroyable';
import type Owner from '@ember/owner';
import type RouterService from '@ember/routing/router-service';
import type Transition from '@ember/routing/transition';
import { service } from '@ember/service';

/**
 * How long a transition may take before the bar shows. Most screens load
 * faster than this, and a bar that flashes for a moment is noise.
 */
export const SHOW_DELAY = 150;
/** The bar fills up and fades out for this long once the screen is there. */
export const FINISH_DURATION = 350;

type Phase = 'idle' | 'loading' | 'finishing';

/**
 * A thin bar along the top of the window while the router waits for the
 * next screen's data (opening an app waits for the app to load). It creeps
 * towards the end while waiting, fills up when the screen is ready, and
 * fades out. Changes of query parameters only (the log filters) do not
 * count: those screens show their own progress.
 *
 * Timers are the browser's, not the run loop's, so tests settle without
 * waiting for the bar to fade.
 */
export default class RouteProgress extends Component {
  @service declare router: RouterService;

  @tracked phase: Phase = 'idle';

  private current: Transition | null = null;
  private timer: ReturnType<typeof setTimeout> | undefined;

  constructor(owner: Owner, args: object) {
    super(owner, args);
    this.router.on('routeWillChange', this.willChange);
    this.router.on('routeDidChange', this.didChange);
    registerDestructor(this, () => {
      this.router.off('routeWillChange', this.willChange);
      this.router.off('routeDidChange', this.didChange);
      clearTimeout(this.timer);
    });
  }

  get visible(): boolean {
    return this.phase !== 'idle';
  }

  private willChange = (transition: Transition) => {
    if (transition.queryParamsOnly) return;
    this.current = transition;
    // A transition that is aborted and not followed by another (a dialog
    // keeping unsaved changes) ends the wait too. A redirect starts a new
    // transition first, so the old one no longer counts when it rejects.
    const ended = () => {
      if (this.current === transition) this.finish();
    };
    transition.then(ended, ended);
    if (this.phase === 'loading') return;
    clearTimeout(this.timer);
    if (this.phase === 'finishing') {
      this.phase = 'loading';
      return;
    }
    this.timer = setTimeout(() => {
      if (this.current) this.phase = 'loading';
    }, SHOW_DELAY);
  };

  private didChange = () => {
    if (this.current) this.finish();
  };

  private finish(): void {
    this.current = null;
    clearTimeout(this.timer);
    if (this.phase !== 'loading') return;
    this.phase = 'finishing';
    this.timer = setTimeout(() => {
      this.phase = 'idle';
    }, FINISH_DURATION);
  }

  <template>
    {{#if this.visible}}
      <div
        class="route-progress {{this.phase}}"
        role="progressbar"
        aria-label="画面を読み込み中"
        data-test-route-progress={{this.phase}}
      >
        <div class="route-progress-bar"></div>
      </div>
    {{/if}}
  </template>
}
