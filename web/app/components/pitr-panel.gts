import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import type Owner from '@ember/owner';
import type ApiService from 'koroku-console/services/api';
import type PollerService from 'koroku-console/services/poller';
import type AppContext from 'koroku-console/utils/app-context';
import { inputValue, preventDefault } from 'koroku-console/utils/dom';
import { fieldErrors } from 'koroku-console/utils/errors';
import { formatDateTimeJst } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import { can } from 'koroku-console/utils/roles';
import type { StatusKind } from 'koroku-console/utils/status';
import type { App, PitrGap, PitrStatus } from 'koroku-console/utils/types';
import Banner from './banner';
import Button from './button';
import ConfirmDialog from './confirm-dialog';
import ErrorMessage from './error-message';
import FormField from './form-field';
import Icon from './icon';
import RelativeTime from './relative-time';
import RoleNote from './role-note';
import StatusIndicator from './status-indicator';

/** The state is followed while the preparation runs. */
const PREPARING_INTERVAL = 5000;

const STATES: Record<PitrStatus['state'], { kind: StatusKind; label: string }> =
  {
    off: { kind: 'stopped', label: '使っていません' },
    unavailable: { kind: 'stopped', label: '使えません' },
    waiting: { kind: 'pending', label: '次のデプロイから' },
    preparing: { kind: 'deploying', label: '準備中' },
    ready: { kind: 'running', label: '有効' },
    failed: { kind: 'failed', label: '準備に失敗' },
  };

const GAP_REASONS: Record<PitrGap['reason'], string> = {
  mount_missing: 'WAL の受け渡しがないリリースが動いていた',
  dropped: '退避できなかった WAL を捨てた',
  version_changed: 'PostgreSQL のメジャーバージョンが変わった',
};

function stateKind(status: PitrStatus): StatusKind {
  return STATES[status.state]?.kind ?? 'stopped';
}

function stateLabel(status: PitrStatus): string {
  return STATES[status.state]?.label ?? status.state;
}

function gapReason(gap: PitrGap): string {
  return GAP_REASONS[gap.reason] ?? gap.reason;
}

function at(time: string | null): string {
  return time ? formatDateTimeJst(time) : '';
}

interface Signature {
  Args: { model: AppContext };
}

/**
 * Point-in-time recovery of a PostgreSQL service (ARCHITECTURE 10.9): the
 * service whose WAL is archived, where the preparation stands, and how far
 * back the database can go.
 */
export default class PitrPanel extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  status = new Loadable<PitrStatus>(() => this.api.getPitr(this.slug));

  @tracked serviceName: string;
  @tracked confirming = false;
  @tracked saving = false;
  @tracked saved = false;
  @tracked saveError: unknown = null;
  @tracked errors: Record<string, string> = {};

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.serviceName = this.app.pitr_service ?? '';
    if (!this.available) return;
    void this.status.load();
    this.poller.every(this, PREPARING_INTERVAL, () => this.follow());
  }

  get slug(): string {
    return this.args.model.slug;
  }

  get app(): App {
    return this.args.model.app;
  }

  /** A backend that knows point-in-time recovery reports the field. */
  get available(): boolean {
    return this.app.pitr_service !== undefined;
  }

  get readOnly(): boolean {
    return !can(this.app, 'manage');
  }

  get saved_service(): string {
    return this.app.pitr_service ?? '';
  }

  get changed(): boolean {
    return this.serviceName.trim() !== this.saved_service;
  }

  get turningOff(): boolean {
    return this.serviceName.trim() === '' && this.saved_service !== '';
  }

  private async follow(): Promise<void> {
    const state = this.status.value?.state;
    if (state === 'preparing' || state === 'waiting') {
      await this.status.load({ quiet: true });
    }
  }

  setService = (event: Event) => {
    this.serviceName = inputValue(event);
    this.saved = false;
  };

  /** A production app is asked first: its database restarts. */
  requestSave = () => {
    this.saveError = null;
    if (!this.changed) {
      this.saved = true;
      return;
    }
    if (this.args.model.isProduction) {
      this.confirming = true;
      return;
    }
    void this.save();
  };

  cancel = () => {
    this.confirming = false;
  };

  save = async () => {
    this.saving = true;
    this.errors = {};
    try {
      const app = await this.api.updateApp(this.slug, {
        pitr_service: this.serviceName.trim(),
      });
      this.args.model.set(app);
      this.serviceName = app.pitr_service ?? '';
      this.saved = true;
    } catch (error) {
      this.saveError = error;
      this.errors = fieldErrors(error);
    } finally {
      this.saving = false;
      this.confirming = false;
    }
    await this.status.load({ quiet: true });
  };

  <template>
    {{#if this.available}}
      <section class="pane pitr" aria-labelledby="pitr-title" data-test-pitr>
        <div class="pane-header">
          <h2
            class="pane-title"
            id="pitr-title"
          >時点指定の復旧（PostgreSQL）</h2>
        </div>
        <p class="backup-note">
          PostgreSQL の WAL を退避して、DB を過去 7
          日のうちの指定した時刻に戻せるようにします。データディレクトリは named
          volume に置き、PostgreSQL 14 以上が要ります。
        </p>

        {{#if this.status.value}}
          <div class="box pitr-status" data-test-pitr-status>
            <dl class="facts">
              <div class="fact">
                <dt>状態</dt>
                <dd data-test-pitr-state>
                  <StatusIndicator
                    @kind={{stateKind this.status.value}}
                    @label={{stateLabel this.status.value}}
                  />
                </dd>
              </div>
              {{#if this.status.value.earliest_at}}
                <div class="fact">
                  <dt>戻せる期間</dt>
                  <dd data-test-pitr-range>
                    {{at this.status.value.earliest_at}}
                    〜 現在
                  </dd>
                </div>
              {{/if}}
              {{#if this.status.value.archived_until}}
                <div class="fact">
                  <dt>WAL の退避</dt>
                  <dd data-test-pitr-archived>
                    <RelativeTime @time={{this.status.value.archived_until}} />
                    まで
                  </dd>
                </div>
              {{/if}}
            </dl>
          </div>
          {{#if (eqState this.status.value "waiting")}}
            <p class="muted" data-test-pitr-waiting>
              次のデプロイで WAL の受け渡しを付け、デプロイが終わったあとで
              {{this.status.value.service}}
              の設定を書いて準備します。{{if
                @model.isProduction
                "このとき DB が数秒止まります。"
              }}
            </p>
          {{else if (eqState this.status.value "unavailable")}}
            <p class="muted" data-test-pitr-unavailable>
              バックアップが設定されていないため使えません。
            </p>
          {{else if (eqState this.status.value "failed")}}
            <Banner
              @tone="error"
              @title="準備に失敗しました"
              data-test-pitr-error
            >
              {{this.status.value.error}}
              <span class="banner-detail">
                直したあと、もう一度デプロイすると準備をやり直します。
              </span>
            </Banner>
          {{/if}}
          {{#if this.status.value.warning}}
            <Banner @tone="warning" data-test-pitr-warning>
              {{this.status.value.warning}}
            </Banner>
          {{/if}}
          {{#if this.status.value.gaps.length}}
            <h3 class="subsection-title">戻せない区間</h3>
            <ul class="pitr-gaps" data-test-pitr-gaps>
              {{#each this.status.value.gaps as |gap|}}
                <li data-test-pitr-gap={{gap.reason}}>
                  <span class="mono">{{at gap.from}}
                    〜
                    {{if gap.to (at gap.to) "現在"}}</span>
                  <span class="muted">{{gapReason gap}}</span>
                </li>
              {{/each}}
            </ul>
          {{/if}}
        {{else if this.status.error}}
          <ErrorMessage @error={{this.status.error}} />
        {{/if}}

        {{#if this.readOnly}}
          <RoleNote @needed="manage" @what="時点指定の復旧の設定" />
        {{/if}}
        <form
          class="box pitr-settings"
          novalidate
          data-test-pitr-form
          {{on "submit" (preventDefault this.requestSave)}}
        >
          <fieldset class="role-fieldset" disabled={{this.readOnly}}>
            <FormField
              @label="PostgreSQL のサービス"
              @hint="Compose ファイルのサービス名です。空にすると使いません。設定は次のデプロイから効きます。"
              @error={{this.errors.pitr_service}}
              data-test-field-group="pitr_service"
              as |field|
            >
              <input
                id={{field.id}}
                type="text"
                spellcheck="false"
                autocomplete="off"
                placeholder="db"
                value={{this.serviceName}}
                aria-describedby={{field.describedBy}}
                aria-invalid={{field.invalid}}
                data-test-field="pitr_service"
                {{on "input" this.setService}}
              />
            </FormField>
            <div aria-live="polite">
              <ErrorMessage @error={{this.saveError}} />
            </div>
            <div class="form-footer">
              <Button
                @type="submit"
                @variant="primary"
                @busy={{this.saving}}
                @busyLabel="保存中…"
                data-test-pitr-save
              >保存</Button>
              <p class="form-status" aria-live="polite">
                {{#if this.saved}}
                  <span class="form-saved" data-test-pitr-saved>
                    <Icon @name="check" />
                    保存しました。
                  </span>
                {{/if}}
              </p>
            </div>
          </fieldset>
        </form>
      </section>

      {{#if this.confirming}}
        <ConfirmDialog
          @title={{if
            this.turningOff
            "時点指定の復旧をやめますか？"
            "時点指定の復旧を使いますか？"
          }}
          @confirmLabel={{if this.turningOff "やめる" "使う"}}
          @busy={{this.saving}}
          @onConfirm={{this.save}}
          @onCancel={{this.cancel}}
        >
          {{#if this.turningOff}}
            <p>
              本番アプリ
              <strong>{{this.slug}}</strong>
              の
              <strong>{{this.saved_service}}</strong>
              の WAL の退避をやめます。設定を戻すため、DB をすぐに 1
              回再起動し、数秒止まります。退避済みのバックアップは、期限まで残ります。
            </p>
          {{else}}
            <p>
              本番アプリ
              <strong>{{this.slug}}</strong>
              の
              <strong>{{this.serviceName}}</strong>
              の WAL を退避します。次のデプロイの後に、DB の設定を書いて 1
              回再起動するので、DB が数秒止まります。
            </p>
          {{/if}}
        </ConfirmDialog>
      {{/if}}
    {{/if}}
  </template>
}

function eqState(status: PitrStatus, state: PitrStatus['state']): boolean {
  return status.state === state;
}
