import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from 'paas-console/components/async-view';
import Badge from 'paas-console/components/badge';
import Banner from 'paas-console/components/banner';
import Button, { buttonClass } from 'paas-console/components/button';
import ConfirmDialog from 'paas-console/components/confirm-dialog';
import CopyButton from 'paas-console/components/copy-button';
import ErrorMessage from 'paas-console/components/error-message';
import FormField from 'paas-console/components/form-field';
import Icon from 'paas-console/components/icon';
import RelativeTime from 'paas-console/components/relative-time';
import StatusIndicator from 'paas-console/components/status-indicator';
import TabSection from 'paas-console/components/tab-section';
import type ApiService from 'paas-console/services/api';
import type PollerService from 'paas-console/services/poller';
import type AppContext from 'paas-console/utils/app-context';
import { preventDefault, setValue } from 'paas-console/utils/dom';
import { fieldErrors } from 'paas-console/utils/errors';
import Loadable from 'paas-console/utils/loadable';
import {
  DOMAIN_STATUS_KINDS,
  DOMAIN_STATUS_LABELS,
} from 'paas-console/utils/status';
import type { DnsRecord, Domain, DomainList } from 'paas-console/utils/types';

/**
 * How often pending domains are re-read. The server re-checks them every
 * five minutes on its own; this only picks the result up without a reload.
 */
const PENDING_INTERVAL = 15000;

const SKELETON_ROWS = [1, 2];

type Action = 'verify' | 'primary' | 'force' | 'delete';

function statusKind(domain: Domain) {
  return DOMAIN_STATUS_KINDS[domain.status] ?? 'pending';
}

function statusLabel(domain: Domain): string {
  return DOMAIN_STATUS_LABELS[domain.status] ?? domain.status;
}

function isPending(domain: Domain): boolean {
  return domain.status !== 'active';
}

function isActive(domain: Domain): boolean {
  return domain.status === 'active';
}

/** Skipping the DNS check is offered while pending, and can be undone. */
function showsForce(domain: Domain): boolean {
  return isPending(domain) || domain.force_active;
}

function records(domain: Domain): DnsRecord[] {
  return domain.records ?? [];
}

function hasChoice(domain: Domain): boolean {
  return records(domain).length > 1;
}

function url(hostname: string): string {
  return `https://${hostname}`;
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppDomainsTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  list = new Loadable<DomainList>(() => this.api.listDomains(this.slug));

  @tracked hostname = '';
  @tracked adding = false;
  @tracked addError: unknown = null;
  @tracked hostnameError = '';

  /** The action in progress, and the domain it runs on. */
  @tracked running: { id: number; action: Action } | null = null;
  @tracked error: unknown = null;
  /** What the last action did, in a sentence. */
  @tracked notice = '';
  @tracked deleting: Domain | null = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.list.load();
    this.poller.every(this, PENDING_INTERVAL, () => this.refreshPending());
  }

  get slug(): string {
    return this.args.model.slug;
  }

  get domains(): Domain[] {
    return this.list.value?.domains ?? [];
  }

  get pendingCount(): number {
    return this.domains.filter(isPending).length;
  }

  get busy(): boolean {
    return this.running !== null;
  }

  /** Picks up a pending domain turning active (or the reason it did not). */
  async refreshPending(): Promise<void> {
    if (this.pendingCount === 0 || this.busy) return;
    const before = this.pendingCount;
    await this.list.load({ quiet: true });
    // The header shows the primary URL and counts pending domains.
    if (this.pendingCount !== before) await this.args.model.reload();
  }

  add = async () => {
    const hostname = this.hostname.trim().toLowerCase();
    this.addError = null;
    this.hostnameError = '';
    this.notice = '';
    if (!hostname) {
      this.hostnameError = 'ホスト名を入力してください。';
      return;
    }
    this.adding = true;
    try {
      const created = await this.api.addDomain(this.slug, hostname);
      this.hostname = '';
      this.notice = isActive(created)
        ? `${created.hostname} を追加しました。DNS を確認でき、有効になっています。`
        : `${created.hostname} を追加しました。表示された DNS レコードを設定してください。`;
      await this.reload();
    } catch (error) {
      const fields = fieldErrors(error);
      this.hostnameError = fields['hostname'] ?? '';
      // A field error is shown next to the input; anything else on top.
      this.addError = this.hostnameError ? null : error;
    } finally {
      this.adding = false;
    }
  };

  private async reload(): Promise<void> {
    await Promise.all([
      this.list.load({ quiet: true }),
      this.args.model.reload(),
    ]);
  }

  /**
   * Runs one action on a domain, then refreshes the whole list: other rows
   * change too (only one domain is primary). Resolves to the action's
   * result, or `null` when it failed.
   */
  private async act<T>(
    domain: Domain,
    action: Action,
    run: () => Promise<T>,
  ): Promise<T | null> {
    this.running = { id: domain.id, action };
    this.error = null;
    this.notice = '';
    let result: T | null = null;
    try {
      result = await run();
    } catch (error) {
      this.error = error;
    }
    await this.reload();
    this.running = null;
    return result;
  }

  verify = async (domain: Domain) => {
    const checked = await this.act(domain, 'verify', () =>
      this.api.verifyDomain(this.slug, domain.id),
    );
    if (!checked) return;
    this.notice = isActive(checked)
      ? `${checked.hostname} を確認でき、有効になりました。`
      : `${checked.hostname} はまだ確認できませんでした。`;
  };

  togglePrimary = async (domain: Domain) => {
    const makePrimary = !domain.is_primary;
    const updated = await this.act(domain, 'primary', () =>
      this.api.updateDomain(this.slug, domain.id, { is_primary: makePrimary }),
    );
    if (!updated) return;
    this.notice = makePrimary
      ? `${domain.hostname} をプライマリにしました。ほかのホスト名へのアクセスはここへリダイレクトされます。`
      : `${domain.hostname} のプライマリ指定を外しました。`;
  };

  toggleForce = (domain: Domain) =>
    this.act(domain, 'force', () =>
      this.api.updateDomain(this.slug, domain.id, {
        force_active: !domain.force_active,
      }),
    );

  requestDelete = (domain: Domain) => {
    this.deleting = domain;
  };

  cancelDelete = () => {
    this.deleting = null;
  };

  confirmDelete = async () => {
    const domain = this.deleting;
    if (!domain) return;
    const deleted = await this.act(domain, 'delete', async () => {
      await this.api.deleteDomain(this.slug, domain.id);
      return true;
    });
    if (deleted) this.notice = `${domain.hostname} を削除しました。`;
    this.deleting = null;
  };

  isRunning = (domain: Domain, action: Action): boolean =>
    this.running?.id === domain.id && this.running.action === action;

  <template>
    <TabSection @title="ドメイン" data-test-domains>
      <p class="tab-section-hint">
        このアプリに届くホスト名です。プライマリを決めると、ほかのホスト名へのアクセスはプライマリへリダイレクトされます。
      </p>

      {{#if this.list.stale}}
        <Banner @tone="warning" data-test-stale>
          最新の状態を取得できていません。表示は最後に取得できた内容です。
        </Banner>
      {{/if}}

      <div class="domain-feedback" aria-live="polite">
        {{#if this.notice}}
          <p class="domain-notice" data-test-domain-notice>
            <Icon @name="info" />{{this.notice}}
          </p>
        {{/if}}
        <ErrorMessage @error={{this.error}} @showDetails={{true}} />
      </div>

      <AsyncView
        @data={{this.list}}
        @errorTitle="ドメインの一覧を取得できませんでした"
      >
        <:loading>
          <div
            class="row-list"
            role="status"
            aria-label="読み込み中"
            data-test-loading
          >
            {{#each SKELETON_ROWS as |n|}}
              <div class="row" aria-hidden="true" data-skeleton={{n}}>
                <span class="skeleton domain-skeleton"></span>
              </div>
            {{/each}}
          </div>
        </:loading>

        <:default as |list|>
          <ul class="row-list domain-items" aria-label="ドメイン">
            <li class="domain-item" data-test-domain-default>
              <div class="domain-item-head">
                <div class="domain-item-name">
                  <span class="domain-hostname" data-test-default-hostname>
                    {{list.default_hostname}}
                  </span>
                  <CopyButton
                    @text={{list.default_hostname}}
                    @label="ホスト名"
                  />
                  <Badge>自動割当</Badge>
                </div>
                <div class="domain-item-status">
                  <StatusIndicator @kind="running" @label="有効" />
                </div>
                <div class="domain-item-actions">
                  <a
                    class={{buttonClass "ghost" "small"}}
                    href={{url list.default_hostname}}
                    target="_blank"
                    rel="noopener noreferrer"
                    aria-label="{{list.default_hostname}} を新しいタブで開く"
                  >開く<Icon @name="external" /></a>
                </div>
              </div>
            </li>

            {{#each this.domains key="id" as |domain|}}
              <li
                class="domain-item"
                data-test-domain={{domain.hostname}}
                data-status={{domain.status}}
              >
                <div class="domain-item-head">
                  <div class="domain-item-name">
                    <span class="domain-hostname">{{domain.hostname}}</span>
                    <CopyButton @text={{domain.hostname}} @label="ホスト名" />
                    {{#if domain.is_primary}}
                      <Badge @tone="info" data-test-domain-primary>
                        プライマリ
                      </Badge>
                    {{/if}}
                    {{#if domain.force_active}}
                      <Badge data-test-domain-forced>強制有効化</Badge>
                    {{/if}}
                  </div>
                  <div class="domain-item-status">
                    <StatusIndicator
                      @kind={{statusKind domain}}
                      @label={{statusLabel domain}}
                      data-test-domain-status
                    />
                  </div>
                  <div class="domain-item-actions">
                    {{#if (isActive domain)}}
                      <Button
                        @size="small"
                        @variant={{if domain.is_primary "ghost" "default"}}
                        @busy={{this.isRunning domain "primary"}}
                        @disabled={{this.busy}}
                        data-test-domain-primary-toggle
                        {{on "click" (fn this.togglePrimary domain)}}
                      >{{if
                          domain.is_primary
                          "プライマリを解除"
                          "プライマリにする"
                        }}</Button>
                    {{/if}}
                    <Button
                      @size="small"
                      @variant={{if (isActive domain) "ghost" "default"}}
                      @busy={{this.isRunning domain "verify"}}
                      @busyLabel="確認中…"
                      @disabled={{this.busy}}
                      aria-label="{{domain.hostname}} の DNS を再確認"
                      data-test-domain-verify
                      {{on "click" (fn this.verify domain)}}
                    >再確認</Button>
                    <button
                      type="button"
                      class="{{buttonClass 'default' 'small'}}
                        button-quiet-danger domain-delete"
                      disabled={{this.busy}}
                      aria-label="{{domain.hostname}} を削除"
                      data-test-domain-delete
                      {{on "click" (fn this.requestDelete domain)}}
                    >削除</button>
                  </div>
                </div>

                {{#if (isPending domain)}}
                  <div class="domain-detail" data-test-domain-pending>
                    <p class="domain-detail-lead">
                      DNS に次のレコードを設定してください。{{#if
                        (hasChoice domain)
                      }}どちらか一方で構いません。{{/if}}
                    </p>

                    {{#if (records domain)}}
                      <div class="domain-records" data-test-domain-records>
                        <div
                          class="domain-record domain-record-header"
                          aria-hidden="true"
                        >
                          <span>種類</span>
                          <span>ホスト</span>
                          <span>値</span>
                        </div>
                        <ul
                          class="domain-record-list"
                          aria-label="設定する DNS レコード"
                        >
                          {{#each (records domain) as |record|}}
                            <li
                              class="domain-record"
                              data-test-domain-record={{record.type}}
                            >
                              <div class="domain-record-field">
                                <span class="domain-record-label">種類</span>
                                <span class="domain-record-type">
                                  {{record.type}}
                                </span>
                              </div>
                              <div class="domain-record-field">
                                <span class="domain-record-label">ホスト</span>
                                <span class="domain-record-value">
                                  <span>{{record.name}}</span>
                                  <CopyButton
                                    @text={{record.name}}
                                    @label="{{record.type}} レコードのホスト"
                                    data-test-copy-record-name
                                  />
                                </span>
                              </div>
                              <div class="domain-record-field">
                                <span class="domain-record-label">値</span>
                                <span class="domain-record-value">
                                  <span>{{record.value}}</span>
                                  <CopyButton
                                    @text={{record.value}}
                                    @label="{{record.type}} レコードの値"
                                    data-test-copy-record-value
                                  />
                                </span>
                              </div>
                            </li>
                          {{/each}}
                        </ul>
                      </div>
                    {{/if}}

                    {{#if domain.last_error}}
                      <p class="domain-check is-failed" data-test-domain-error>
                        <Icon @name="warning" />
                        <span>
                          <span class="domain-check-label">直近の確認結果</span>
                          <span class="domain-check-reason">
                            {{domain.last_error}}
                          </span>
                          {{#if domain.last_checked_at}}
                            <span class="domain-check-time">
                              <RelativeTime @time={{domain.last_checked_at}} />
                            </span>
                          {{/if}}
                        </span>
                      </p>
                    {{else if domain.last_checked_at}}
                      <p class="domain-check" data-test-domain-checked>
                        最終確認:
                        <RelativeTime @time={{domain.last_checked_at}} />
                      </p>
                    {{else}}
                      <p class="domain-check" data-test-domain-checked>
                        まだ確認していません。
                      </p>
                    {{/if}}

                    <p class="domain-detail-note">
                      5 分ごとに自動で再確認します。DNS
                      がこのサーバーを向くと有効になり、証明書が発行されます。
                    </p>
                  </div>
                {{/if}}

                {{#if (showsForce domain)}}
                  <div class="domain-detail domain-force">
                    <p class="domain-detail-note">
                      {{#if domain.force_active}}
                        DNS の確認を省略して有効にしています。
                      {{else}}
                        CDN を前段に置くなど、IP
                        が一致しない構成では確認を省略できます。
                      {{/if}}
                    </p>
                    <Button
                      @size="small"
                      @busy={{this.isRunning domain "force"}}
                      @disabled={{this.busy}}
                      data-test-domain-force-toggle
                      {{on "click" (fn this.toggleForce domain)}}
                    >{{if
                        domain.force_active
                        "強制有効化を解除"
                        "強制有効化"
                      }}</Button>
                  </div>
                {{/if}}
              </li>
            {{else}}
              <li class="domain-item domain-none" data-test-no-domains>
                独自ドメインはまだありません。下のフォームから追加できます。
              </li>
            {{/each}}
          </ul>
        </:default>
      </AsyncView>
    </TabSection>

    {{#if this.list.loaded}}
      <TabSection @title="独自ドメインを追加" data-test-domain-add-section>
        <form
          class="domain-form"
          novalidate
          data-test-domain-form
          {{on "submit" (preventDefault this.add)}}
        >
          <FormField
            @label="ホスト名"
            @hint="追加すると「確認待ち」になり、設定する DNS レコードが表示されます（サブドメインは CNAME、apex は A レコード）。"
            @error={{this.hostnameError}}
            as |field|
          >
            <div class="domain-form-line">
              <input
                id={{field.id}}
                type="text"
                class="mono"
                placeholder="www.example.org"
                spellcheck="false"
                autocapitalize="off"
                autocomplete="off"
                value={{this.hostname}}
                aria-invalid={{field.invalid}}
                aria-describedby={{field.describedBy}}
                data-test-domain-hostname
                {{on "input" (setValue this "hostname")}}
              />
              <Button
                @type="submit"
                @variant="primary"
                @busy={{this.adding}}
                @busyLabel="追加中…"
                data-test-domain-add
              >追加</Button>
            </div>
          </FormField>
        </form>
        <div aria-live="polite">
          <ErrorMessage @error={{this.addError}} />
        </div>
      </TabSection>
    {{/if}}

    {{#if this.deleting}}
      <ConfirmDialog
        @title="ドメインを削除しますか？"
        @confirmLabel="削除する"
        @danger={{true}}
        @busy={{this.busy}}
        @onConfirm={{this.confirmDelete}}
        @onCancel={{this.cancelDelete}}
      >
        <p><strong>{{this.deleting.hostname}}</strong>
          をこのアプリから外します。このホスト名へのアクセスは応答しなくなります。</p>
        {{#if this.deleting.is_primary}}
          <p
            data-test-dialog-primary
          >プライマリの指定も外れ、ほかのホスト名からのリダイレクトは行われなくなります。</p>
        {{/if}}
      </ConfirmDialog>
    {{/if}}
  </template>
}
