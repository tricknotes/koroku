import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import type ApiService from 'koroku-console/services/api';
import { inputValue } from 'koroku-console/utils/dom';
import { fieldErrors } from 'koroku-console/utils/errors';
import { formatBytes } from 'koroku-console/utils/format';
import { can } from 'koroku-console/utils/roles';
import {
  fallbackText,
  reservationPlan,
  reservationProblem,
  sourceText,
  suggestedReservation,
  withReservation,
  type ReservationPlan,
} from 'koroku-console/utils/service-reservation';
import type { App, ServiceStatus } from 'koroku-console/utils/types';

const FIELD = 'service_memory_reservations';

function bytesText(bytes: number): string {
  return bytes > 0 ? formatBytes(bytes) : 'なし';
}

interface Signature {
  Element: HTMLDivElement;
  Args: {
    app: App;
    service: ServiceStatus;
    /** The app as the server has it after the reservation was saved. */
    onSaved: (app: App) => void;
  };
}

/**
 * The reservation cell of a service row on the resources tab
 * (ARCHITECTURE 9.5): what the running release reserves, what the next
 * deployment will, and, for the manage role on a production app, a dialog
 * that saves this one service's reservation.
 */
export default class ServiceReservation extends Component<Signature> {
  @service declare api: ApiService;

  @tracked editing = false;
  @tracked value = '';
  @tracked saving = false;
  @tracked error: unknown = null;
  /** The format was refused before sending. */
  @tracked problem: string | null = null;
  @tracked suggestion: string | null = null;

  get plan(): ReservationPlan {
    return reservationPlan(this.args.app, this.args.service);
  }

  get production(): boolean {
    return this.args.app.environment === 'production';
  }

  get canEdit(): boolean {
    return (
      this.production && !this.plan.oneShot && can(this.args.app, 'manage')
    );
  }

  get name(): string {
    return this.args.service.name;
  }

  /** The service has a value of its own, which "既定に戻す" removes. */
  get hasOwn(): boolean {
    return Boolean(this.args.app.service_memory_reservations?.[this.name]);
  }

  get runningText(): string {
    return bytesText(this.plan.runningBytes);
  }

  get nextText(): string {
    const next = this.plan.nextBytes;
    if (next === null) return '';
    if (!this.production) return '次のデプロイで外れます';
    return `次のデプロイから ${bytesText(next)}`;
  }

  get sourceText(): string {
    return sourceText(this.plan);
  }

  get limitText(): string {
    const limit = this.plan.limitBytes;
    return limit === null ? '' : formatBytes(limit);
  }

  get fallbackText(): string {
    return fallbackText(this.args.app);
  }

  get fieldError(): string | null {
    return this.problem ?? fieldErrors(this.error)[FIELD] ?? null;
  }

  get empty(): boolean {
    return this.value.trim() === '';
  }

  get showsSuggestion(): boolean {
    return this.suggestion !== null && this.suggestion !== this.value.trim();
  }

  open = () => {
    this.value = this.args.app.service_memory_reservations?.[this.name] ?? '';
    this.error = null;
    this.problem = null;
    this.suggestion = null;
    this.editing = true;
    void this.loadSuggestion();
  };

  /** The suggestion does not depend on the range; a failure shows none. */
  async loadSuggestion() {
    try {
      const history = await this.api.getMemoryHistory(
        this.args.app.slug,
        '24h',
      );
      if (this.isDestroying || !this.editing) return;
      this.suggestion = suggestedReservation(
        history.services.find((s) => s.name === this.name),
        this.plan.limitBytes,
      );
    } catch {
      this.suggestion = null;
    }
  }

  setValue = (event: Event) => {
    this.value = inputValue(event);
    this.problem = null;
  };

  useSuggestion = () => {
    if (this.suggestion) this.value = this.suggestion;
    this.problem = null;
  };

  cancel = () => {
    if (!this.saving) this.editing = false;
  };

  save = async () => {
    const value = this.value.trim().toLowerCase();
    this.problem = reservationProblem(value);
    if (this.problem) return;
    await this.send(value);
  };

  reset = async () => {
    this.problem = null;
    await this.send(null);
  };

  /** Adds, replaces or removes this service's entry; the others stay. */
  async send(value: string | null) {
    this.saving = true;
    this.error = null;
    try {
      const app = await this.api.updateApp(this.args.app.slug, {
        service_memory_reservations: withReservation(
          this.args.app,
          this.name,
          value,
        ),
      });
      this.args.onSaved(app);
      this.editing = false;
    } catch (error) {
      this.error = error;
    } finally {
      this.saving = false;
    }
  }

  <template>
    <div
      class="resource-reservation-cell"
      data-test-resource-reservation-cell
      ...attributes
    >
      <span class="cell-label">予約</span>
      <span class="resource-reservation-value">
        <span
          title="ホストのメモリが足りなくなっても、この量までは後回しに回収されます。"
          data-test-resource-reservation
        >{{this.runningText}}</span>
        {{#if this.canEdit}}
          <Button
            @variant="ghost"
            @size="small"
            aria-label="{{this.name}} の予約を変更"
            data-test-reservation-edit={{this.name}}
            {{on "click" this.open}}
          >変更</Button>
        {{/if}}
      </span>
      {{#if this.plan.pending}}
        <span class="resource-reservation-next" data-test-reservation-next>
          {{this.nextText}}
        </span>
      {{/if}}
      {{#if this.production}}
        <span
          class="resource-reservation-source overview-secondary"
          data-test-reservation-source
        >{{this.sourceText}}</span>
      {{/if}}
    </div>

    {{#if this.editing}}
      <ConfirmDialog
        @title="{{this.name}} のメモリの予約"
        @confirmLabel="予約を保存"
        @busy={{this.saving}}
        @disabled={{this.empty}}
        @onConfirm={{this.save}}
        @onCancel={{this.cancel}}
        @altLabel={{if this.hasOwn "既定に戻す"}}
        @onAlt={{this.reset}}
      >
        <p>
          ホストのメモリが足りなくなったとき、予約した量まではほかのアプリより後に回収されます。設定のサービス別の予約に保存します。
        </p>
        <dl class="reservation-facts" data-test-reservation-facts>
          <div>
            <dt>稼働中</dt>
            <dd>{{this.runningText}}</dd>
          </div>
          {{#if this.limitText}}
            <div>
              <dt>上限</dt>
              <dd>{{this.limitText}}</dd>
            </div>
          {{/if}}
          <div>
            <dt>いまの設定</dt>
            <dd>{{this.sourceText}}</dd>
          </div>
        </dl>
        <FormField
          class="field-narrow"
          @label="予約"
          @hint="例 256m。0 なら予約しません。6m 以上で、上限を超えない値にしてください。"
          @error={{this.fieldError}}
          data-test-field-group="reservation"
          as |field|
        >
          <input
            id={{field.id}}
            type="text"
            class="mono"
            spellcheck="false"
            autocomplete="off"
            placeholder="256m"
            value={{this.value}}
            aria-describedby={{field.describedBy}}
            aria-invalid={{field.invalid}}
            data-test-reservation-input
            {{on "input" this.setValue}}
          />
        </FormField>
        {{#if this.showsSuggestion}}
          <p class="reservation-suggestion" data-test-reservation-suggestion>
            目安
            <strong class="mono">{{this.suggestion}}</strong>
            <span class="muted">（直近の 5 分ごとの最大の 95 パーセンタイル）</span>
            <Button
              @size="small"
              data-test-reservation-use-suggestion
              {{on "click" this.useSuggestion}}
            >この値を入れる</Button>
          </p>
        {{/if}}
        {{#if this.hasOwn}}
          <p class="field-hint">
            「既定に戻す」と、このサービスの値を外し、{{this.fallbackText}}を使います。
          </p>
        {{/if}}
        <p class="field-hint" data-test-reservation-notice>
          動いているコンテナはそのままで、次のデプロイから反映されます。システム画面の予約の合計はデプロイするまで稼働中のリリースの値のままで、次のデプロイの値はその横に並びます。
        </p>
        {{#unless this.fieldError}}
          <ErrorMessage @error={{this.error}} />
        {{/unless}}
      </ConfirmDialog>
    {{/if}}
  </template>
}
