import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn, uniqueId } from '@ember/helper';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import ErrorMessage from 'koroku-console/components/error-message';
import FormField from 'koroku-console/components/form-field';
import RoleNote from 'koroku-console/components/role-note';
import focusSection from 'koroku-console/modifiers/focus-section';
import type ApiService from 'koroku-console/services/api';
import { inputValue } from 'koroku-console/utils/dom';
import { fieldErrors } from 'koroku-console/utils/errors';
import {
  appWideText,
  memoryField,
  memoryPatch,
  memoryProblem,
  savedValue,
  serviceValues,
  valueText,
  type MemoryKind,
  type MemoryTarget,
} from 'koroku-console/utils/memory-settings';
import { can } from 'koroku-console/utils/roles';
import type { App, ServiceStatus } from 'koroku-console/utils/types';

const LIMIT: MemoryKind = 'limit';
const RESERVATION: MemoryKind = 'reservation';

interface Signature {
  Element: HTMLElement;
  Args: {
    app: App;
    /** The running services, offered as names for a per-service value. */
    services: ServiceStatus[] | null;
    /** Led here by a link (`?section=memory`): in view and focused. */
    focus: boolean;
    /** The app as the server has it after a value was saved. */
    onSaved: (app: App) => void;
  };
}

/**
 * The memory limits and reservations of the app, app-wide and per service
 * (ARCHITECTURE 9.5). This is the one place they are changed: one value per
 * dialog, saved at once and applied from the next deployment. Reservations
 * show on production apps, and on a staging app that still has some set.
 */
export default class MemorySettings extends Component<Signature> {
  @service declare api: ApiService;

  /** What the open dialog changes; `adding` asks for the service's name. */
  @tracked editing: (MemoryTarget & { adding: boolean }) | null = null;
  @tracked serviceName = '';
  @tracked value = '';
  @tracked saving = false;
  @tracked error: unknown = null;
  @tracked problem: string | null = null;
  @tracked serviceError: string | null = null;

  get canEdit(): boolean {
    return can(this.args.app, 'manage');
  }

  get production(): boolean {
    return this.args.app.environment === 'production';
  }

  get showsReservations(): boolean {
    const app = this.args.app;
    return (
      this.production ||
      Boolean(app.memory_reservation) ||
      serviceValues(app, RESERVATION).length > 0
    );
  }

  get limits() {
    return serviceValues(this.args.app, LIMIT);
  }

  get reservations() {
    return serviceValues(this.args.app, RESERVATION);
  }

  get appLimit(): string {
    return appWideText(this.args.app, LIMIT);
  }

  get appReservation(): string {
    return appWideText(this.args.app, RESERVATION);
  }

  get hasAppLimit(): boolean {
    return this.args.app.memory_limit !== '';
  }

  get hasAppReservation(): boolean {
    return Boolean(this.args.app.memory_reservation);
  }

  /** Running services without a value of their own of the dialog's kind. */
  get suggestedNames(): string[] {
    const kind = this.editing?.kind ?? LIMIT;
    const taken = new Set(
      serviceValues(this.args.app, kind).map((v) => v.service),
    );
    return (this.args.services ?? [])
      .map((s) => s.name)
      .filter((name) => !taken.has(name));
  }

  get dialogTitle(): string {
    const editing = this.editing;
    if (!editing) return '';
    const what = editing.kind === LIMIT ? 'メモリ上限' : 'メモリの予約';
    if (editing.adding) return `サービス別の${what}を追加`;
    if (editing.service === null) return `全サービス共通の${what}`;
    return `${editing.service} の${what}`;
  }

  get dialogHint(): string {
    const editing = this.editing;
    if (!editing) return '';
    if (editing.kind === LIMIT) {
      return editing.service === null
        ? '例 512m、1g。空なら基盤の既定値を使います。'
        : '例 512m、1g。';
    }
    return editing.service === null
      ? '例 256m。0 なら予約しません。空なら各サービスの上限の半分です。'
      : '例 256m。0 なら予約しません。6m 以上で、上限を超えない値にしてください。';
  }

  /** Removing a service's value, or emptying the app-wide one. */
  get altLabel(): string | undefined {
    const editing = this.editing;
    if (!editing || editing.adding) return undefined;
    if (editing.service !== null) return '外す';
    const saved = savedValue(this.args.app, editing);
    return saved ? '既定に戻す' : undefined;
  }

  get fieldError(): string | null {
    if (this.problem) return this.problem;
    if (!this.editing) return null;
    return fieldErrors(this.error)[memoryField(this.editing)] ?? null;
  }

  isLimit = (kind: MemoryKind): boolean => kind === LIMIT;

  open = (kind: MemoryKind, service: string | null) => {
    this.editing = { kind, service, adding: false };
    this.serviceName = service ?? '';
    this.value = savedValue(this.args.app, { kind, service });
    this.error = null;
    this.problem = null;
    this.serviceError = null;
  };

  add = (kind: MemoryKind) => {
    this.editing = { kind, service: '', adding: true };
    this.serviceName = '';
    this.value = '';
    this.error = null;
    this.problem = null;
    this.serviceError = null;
  };

  setServiceName = (event: Event) => {
    this.serviceName = inputValue(event);
    this.serviceError = null;
  };

  setValue = (event: Event) => {
    this.value = inputValue(event);
    this.problem = null;
  };

  cancel = () => {
    if (!this.saving) this.editing = null;
  };

  save = async () => {
    const editing = this.editing;
    if (!editing) return;
    const service = editing.adding ? this.serviceName.trim() : editing.service;
    this.serviceError =
      editing.adding && !service ? 'サービス名を入力してください。' : null;
    if (this.serviceError) return;
    const target = { kind: editing.kind, service };
    const value = this.value.trim().toLowerCase();
    this.problem = memoryProblem(target, value);
    if (this.problem) return;
    await this.send(target, value);
  };

  remove = async () => {
    const editing = this.editing;
    if (!editing) return;
    this.problem = null;
    await this.send(editing, null);
  };

  private async send(target: MemoryTarget, value: string | null) {
    this.saving = true;
    this.error = null;
    try {
      const app = await this.api.updateApp(
        this.args.app.slug,
        memoryPatch(this.args.app, target, value),
      );
      this.args.onSaved(app);
      this.editing = null;
    } catch (error) {
      this.error = error;
    } finally {
      this.saving = false;
    }
  }

  <template>
    {{#let (uniqueId) as |id|}}
      <section
        class="pane memory-settings"
        id="memory"
        tabindex="-1"
        aria-labelledby="{{id}}-title"
        data-test-memory-settings
        {{focusSection @focus}}
        ...attributes
      >
        <header class="pane-header">
          <h2 class="pane-title" id="{{id}}-title">メモリの上限と予約</h2>
        </header>
        <p class="pane-description">
          koroku
          が各サービスに付けるメモリの上限と、本番アプリで回収から守る量（予約）です。変更は保存するとすぐ設定に残り、動いているコンテナには次のデプロイから反映されます。
        </p>
        {{#unless this.canEdit}}
          <RoleNote @needed="manage" @what="上限と予約の変更" />
        {{/unless}}

        <div class="memory-settings-groups">
          <div class="memory-settings-group" data-test-memory-limits>
            <h3 class="memory-settings-title">上限</h3>
            <ul class="memory-settings-list" aria-label="メモリ上限">
              <li class="memory-settings-row" data-test-memory-setting="limit">
                <span class="memory-settings-name">全サービス共通</span>
                <span
                  class="memory-settings-value mono
                    {{unless this.hasAppLimit 'is-default'}}"
                  data-test-memory-setting-value
                >{{this.appLimit}}</span>
                {{#if this.canEdit}}
                  <Button
                    @variant="ghost"
                    @size="small"
                    aria-label="全サービス共通の上限を変更"
                    data-test-memory-setting-edit="limit"
                    {{on "click" (fn this.open LIMIT null)}}
                  >変更</Button>
                {{/if}}
              </li>
              {{#each this.limits key="service" as |row|}}
                <li
                  class="memory-settings-row"
                  data-test-memory-setting="limit:{{row.service}}"
                >
                  <span class="memory-settings-name mono">{{row.service}}</span>
                  <span
                    class="memory-settings-value mono"
                    data-test-memory-setting-value
                  >{{row.value}}</span>
                  {{#if this.canEdit}}
                    <Button
                      @variant="ghost"
                      @size="small"
                      aria-label="{{row.service}} の上限を変更"
                      data-test-memory-setting-edit="limit:{{row.service}}"
                      {{on "click" (fn this.open LIMIT row.service)}}
                    >変更</Button>
                  {{/if}}
                </li>
              {{/each}}
            </ul>
            {{#if this.canEdit}}
              <Button
                @size="small"
                data-test-memory-setting-add="limit"
                {{on "click" (fn this.add LIMIT)}}
              >サービス別の上限を追加</Button>
            {{/if}}
          </div>

          {{#if this.showsReservations}}
            <div class="memory-settings-group" data-test-memory-reservations>
              <h3 class="memory-settings-title">予約（本番）</h3>
              {{#unless this.production}}
                <p
                  class="field-hint"
                  data-test-memory-reservation-staging
                >ステージングのアプリには付きません。本番に切り替えると、次のデプロイから付きます。</p>
              {{/unless}}
              <ul class="memory-settings-list" aria-label="メモリの予約">
                <li
                  class="memory-settings-row"
                  data-test-memory-setting="reservation"
                >
                  <span class="memory-settings-name">全サービス共通</span>
                  <span
                    class="memory-settings-value mono
                      {{unless this.hasAppReservation 'is-default'}}"
                    data-test-memory-setting-value
                  >{{this.appReservation}}</span>
                  {{#if this.canEdit}}
                    <Button
                      @variant="ghost"
                      @size="small"
                      aria-label="全サービス共通の予約を変更"
                      data-test-memory-setting-edit="reservation"
                      {{on "click" (fn this.open RESERVATION null)}}
                    >変更</Button>
                  {{/if}}
                </li>
                {{#each this.reservations key="service" as |row|}}
                  <li
                    class="memory-settings-row"
                    data-test-memory-setting="reservation:{{row.service}}"
                  >
                    <span
                      class="memory-settings-name mono"
                    >{{row.service}}</span>
                    <span
                      class="memory-settings-value mono"
                      data-test-memory-setting-value
                    >{{valueText row.value}}</span>
                    {{#if this.canEdit}}
                      <Button
                        @variant="ghost"
                        @size="small"
                        aria-label="{{row.service}} の予約を変更"
                        data-test-memory-setting-edit="reservation:{{row.service}}"
                        {{on "click" (fn this.open RESERVATION row.service)}}
                      >変更</Button>
                    {{/if}}
                  </li>
                {{/each}}
              </ul>
              {{#if this.canEdit}}
                <Button
                  @size="small"
                  data-test-memory-setting-add="reservation"
                  {{on "click" (fn this.add RESERVATION)}}
                >サービス別の予約を追加</Button>
              {{/if}}
            </div>
          {{/if}}
        </div>
      </section>

      {{#if this.editing}}
        <ConfirmDialog
          @title={{this.dialogTitle}}
          @confirmLabel="保存"
          @busy={{this.saving}}
          @onConfirm={{this.save}}
          @onCancel={{this.cancel}}
          @altLabel={{this.altLabel}}
          @onAlt={{this.remove}}
        >
          {{#if this.editing.adding}}
            <FormField
              class="field-narrow"
              @label="サービス"
              @hint="Compose のサービス名です。まだ動いていないサービスも指定できます。"
              @error={{this.serviceError}}
              data-test-field-group="memory-service"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                class="mono"
                spellcheck="false"
                autocomplete="off"
                list="{{id}}-services"
                value={{this.serviceName}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-memory-service-input
                {{on "input" this.setServiceName}}
              />
              <datalist id="{{id}}-services">
                {{#each this.suggestedNames as |name|}}
                  <option value={{name}}></option>
                {{/each}}
              </datalist>
            </FormField>
          {{/if}}
          <FormField
            class="field-narrow"
            @label={{if (this.isLimit this.editing.kind) "上限" "予約"}}
            @hint={{this.dialogHint}}
            @error={{this.fieldError}}
            data-test-field-group="memory-value"
            as |field|
          >
            <input
              id={{field.id}}
              type="text"
              class="mono"
              spellcheck="false"
              autocomplete="off"
              value={{this.value}}
              aria-describedby={{field.describedBy}}
              aria-invalid={{field.invalid}}
              data-test-memory-value-input
              {{on "input" this.setValue}}
            />
          </FormField>
          <p class="field-hint" data-test-memory-setting-notice>
            動いているコンテナはそのままで、次のデプロイから反映されます。
          </p>
          {{#unless this.fieldError}}
            <ErrorMessage @error={{this.error}} />
          {{/unless}}
        </ConfirmDialog>
      {{/if}}
    {{/let}}
  </template>
}
