import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from 'paas-console/components/async-view';
import Badge from 'paas-console/components/badge';
import ConfirmDialog from 'paas-console/components/confirm-dialog';
import ErrorMessage from 'paas-console/components/error-message';
import RelativeTime from 'paas-console/components/relative-time';
import type ApiService from 'paas-console/services/api';
import type AppContext from 'paas-console/utils/app-context';
import { preventDefault, setValue } from 'paas-console/utils/dom';
import { fieldErrors } from 'paas-console/utils/errors';
import Loadable from 'paas-console/utils/loadable';
import {
  DOMAIN_STATUS_LABELS,
  DOMAIN_STATUS_TONES,
} from 'paas-console/utils/status';
import type { Domain, DomainList } from 'paas-console/utils/types';

function statusLabel(domain: Domain): string {
  return DOMAIN_STATUS_LABELS[domain.status] ?? domain.status;
}

function statusTone(domain: Domain) {
  return DOMAIN_STATUS_TONES[domain.status] ?? 'muted';
}

function isPending(domain: Domain): boolean {
  return domain.status !== 'active';
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppDomainsTemplate extends Component<Signature> {
  @service declare api: ApiService;

  list = new Loadable<DomainList>(() => this.api.listDomains(this.slug));

  @tracked hostname = '';
  @tracked adding = false;
  @tracked addError: unknown = null;
  @tracked hostnameError = '';

  /** Id of the domain an action is running on. */
  @tracked busyId: number | null = null;
  @tracked error: unknown = null;
  @tracked deleting: Domain | null = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.list.load();
  }

  get slug(): string {
    return this.args.model.slug;
  }

  add = async () => {
    const hostname = this.hostname.trim().toLowerCase();
    this.addError = null;
    this.hostnameError = '';
    if (!hostname) {
      this.hostnameError = 'ホスト名を入力してください。';
      return;
    }
    this.adding = true;
    try {
      await this.api.addDomain(this.slug, hostname);
      this.hostname = '';
      await this.list.load({ quiet: true });
    } catch (error) {
      const fields = fieldErrors(error);
      this.hostnameError = fields['hostname'] ?? '';
      // A field error is shown next to the input; anything else on top.
      this.addError = this.hostnameError ? null : error;
    } finally {
      this.adding = false;
    }
  };

  /** Runs one action on a domain, then refreshes the whole list. */
  private async act(domain: Domain, action: () => Promise<unknown>) {
    this.busyId = domain.id;
    this.error = null;
    try {
      await action();
    } catch (error) {
      this.error = error;
    } finally {
      this.busyId = null;
    }
    // Other rows change too (only one domain is primary).
    await this.list.load({ quiet: true });
  }

  verify = (domain: Domain) =>
    this.act(domain, () => this.api.verifyDomain(this.slug, domain.id));

  togglePrimary = (domain: Domain) =>
    this.act(domain, () =>
      this.api.updateDomain(this.slug, domain.id, {
        is_primary: !domain.is_primary,
      }),
    );

  toggleForce = (domain: Domain) =>
    this.act(domain, () =>
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
    await this.act(domain, () => this.api.deleteDomain(this.slug, domain.id));
    this.deleting = null;
  };

  <template>
    <section class="card" aria-labelledby="domains-title">
      <h3 id="domains-title">ドメイン</h3>

      <AsyncView @data={{this.list}} as |list|>
        <dl class="definition">
          <dt>自動割当のドメイン</dt>
          <dd><code data-test-default-hostname>{{list.default_hostname}}</code>
            <span class="muted">（常に有効）</span></dd>
        </dl>

        <div aria-live="polite">
          <ErrorMessage @error={{this.error}} @showDetails={{true}} />
        </div>

        {{#if list.domains.length}}
          <ul class="domain-list">
            {{#each list.domains key="id" as |domain|}}
              <li class="domain-row" data-test-domain={{domain.hostname}}>
                <div class="domain-row-head">
                  <strong>{{domain.hostname}}</strong>
                  <Badge @tone={{statusTone domain}} data-test-domain-status>
                    {{statusLabel domain}}
                  </Badge>
                  {{#if domain.is_primary}}
                    <Badge @tone="info" data-test-domain-primary>
                      プライマリ
                    </Badge>
                  {{/if}}
                  {{#if domain.force_active}}
                    <Badge @tone="warn" data-test-domain-forced>
                      強制有効化
                    </Badge>
                  {{/if}}
                  {{#if domain.last_checked_at}}
                    <span class="muted">最終確認
                      <RelativeTime @time={{domain.last_checked_at}} /></span>
                  {{/if}}
                </div>

                {{#if domain.last_error}}
                  <p class="alert alert-warn" data-test-domain-error>
                    直近の検証結果:
                    {{domain.last_error}}
                  </p>
                {{/if}}

                {{#if domain.records.length}}
                  <div class="table-wrap">
                    <table class="table table-compact" data-test-domain-records>
                      <caption>設定する DNS レコード（いずれか）</caption>
                      <thead>
                        <tr>
                          <th scope="col">種類</th>
                          <th scope="col">名前</th>
                          <th scope="col">値</th>
                        </tr>
                      </thead>
                      <tbody>
                        {{#each domain.records as |record|}}
                          <tr>
                            <td>{{record.type}}</td>
                            <td><code>{{record.name}}</code></td>
                            <td><code>{{record.value}}</code></td>
                          </tr>
                        {{/each}}
                      </tbody>
                    </table>
                  </div>
                {{/if}}

                <div class="button-row">
                  <button
                    type="button"
                    class="button button-small"
                    disabled={{eq this.busyId domain.id}}
                    data-test-domain-verify
                    {{on "click" (fn this.verify domain)}}
                  >再検証</button>
                  <button
                    type="button"
                    class="button button-small"
                    disabled={{if
                      (eq this.busyId domain.id)
                      true
                      (if domain.is_primary false (isPending domain))
                    }}
                    title={{if
                      (isPending domain)
                      "有効なドメインだけがプライマリになれます"
                    }}
                    data-test-domain-primary-toggle
                    {{on "click" (fn this.togglePrimary domain)}}
                  >{{if
                      domain.is_primary
                      "プライマリを解除"
                      "プライマリにする"
                    }}</button>
                  <button
                    type="button"
                    class="button button-small"
                    disabled={{eq this.busyId domain.id}}
                    data-test-domain-force-toggle
                    {{on "click" (fn this.toggleForce domain)}}
                  >{{if
                      domain.force_active
                      "強制有効化を解除"
                      "強制有効化"
                    }}</button>
                  <button
                    type="button"
                    class="button button-small button-danger"
                    disabled={{eq this.busyId domain.id}}
                    data-test-domain-delete
                    {{on "click" (fn this.requestDelete domain)}}
                  >削除</button>
                </div>
              </li>
            {{/each}}
          </ul>
        {{else}}
          <p
            class="muted"
            data-test-no-domains
          >独自ドメインはまだありません。</p>
        {{/if}}

        <form
          class="form form-inline"
          novalidate
          data-test-domain-form
          {{on "submit" (preventDefault this.add)}}
        >
          <label class="field field-grow" for="domain-hostname">
            <span class="field-label">独自ドメインを追加</span>
            <input
              id="domain-hostname"
              type="text"
              placeholder="www.example.org"
              spellcheck="false"
              autocapitalize="off"
              value={{this.hostname}}
              aria-invalid={{if this.hostnameError "true"}}
              data-test-domain-hostname
              {{on "input" (setValue this "hostname")}}
            />
            {{#if this.hostnameError}}
              <span class="field-error" data-test-domain-hostname-error>
                {{this.hostnameError}}
              </span>
            {{/if}}
          </label>
          <button
            type="submit"
            class="button button-primary"
            disabled={{this.adding}}
            data-test-domain-add
          >{{if this.adding "追加中…" "追加"}}</button>
        </form>
        <div aria-live="polite">
          <ErrorMessage @error={{this.addError}} />
        </div>

        <details class="help">
          <summary>ドメインの設定方法</summary>
          <ol>
            <li>ホスト名を追加すると「確認待ち」になります。</li>
            <li>表示された DNS レコード（サブドメインは CNAME、apex は A）を DNS
              に設定します。</li>
            <li>DNS
              がこのサーバーを向くと「有効」になり、証明書が自動で発行されます。5
              分ごとに自動で再検証します。</li>
            <li
            >プライマリを決めると、ほかのホスト名へのアクセスはプライマリへリダイレクトされます。</li>
            <li>CDN を前段に置くなど IP
              が一致しない構成では「強制有効化」で検証を飛ばせます。</li>
          </ol>
        </details>
      </AsyncView>
    </section>

    {{#if this.deleting}}
      <ConfirmDialog
        @title="ドメインを削除しますか？"
        @confirmLabel="削除する"
        @danger={{true}}
        @busy={{eq this.busyId this.deleting.id}}
        @onConfirm={{this.confirmDelete}}
        @onCancel={{this.cancelDelete}}
      >
        <p><strong>{{this.deleting.hostname}}</strong>
          をこのアプリから外します。このホスト名へのアクセスは応答しなくなります。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
