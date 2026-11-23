import Component from '@glimmer/component';
import { service } from '@ember/service';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import AsyncView from 'koroku-console/components/async-view';
import { buttonClass } from 'koroku-console/components/button';
import CopyButton from 'koroku-console/components/copy-button';
import Icon from 'koroku-console/components/icon';
import MembersPanel from 'koroku-console/components/members-panel';
import StatusIndicator from 'koroku-console/components/status-indicator';
import type ApiService from 'koroku-console/services/api';
import Loadable from 'koroku-console/utils/loadable';
import type { GitHubAppStatus, SystemInfo } from 'koroku-console/utils/types';

const SELECTION_LABELS: Record<string, string> = {
  all: 'すべてのリポジトリ',
  selected: '選んだリポジトリ',
};

function selectionLabel(selection: string): string {
  return SELECTION_LABELS[selection] ?? selection;
}

/**
 * The account IDs for installation_account_ids in config.toml: what is
 * configured, plus the installations koroku does not use yet and the ones
 * it uses through their login alone.
 */
function accountIDs(status: GitHubAppStatus): number[] {
  const ids = [...status.installation_account_ids];
  for (const inst of status.installations) {
    if ((!inst.used || inst.login_only) && !ids.includes(inst.account_id)) {
      ids.push(inst.account_id);
    }
  }
  return ids;
}

/** The installation_account_ids line to put in config.toml. */
function accountIDsLine(status: GitHubAppStatus): string {
  const ids = accountIDs(status);
  const list = ids.length > 0 ? ids.join(', ') : '<アカウントの ID>';
  return `installation_account_ids = [${list}]`;
}

function hasAccountIDs(status: GitHubAppStatus): boolean {
  return accountIDs(status).length > 0;
}

function hasLogins(status: GitHubAppStatus): boolean {
  return status.installation_accounts.length > 0;
}

function idText(id: number): string {
  return String(id);
}

/**
 * 管理 of the system screen (ARCHITECTURE 9.1): the members and their
 * roles (10.7), the GitHub App and its installations (5), the versions.
 */
export default class SystemAdminTemplate extends Component {
  @service declare api: ApiService;

  system = new Loadable<SystemInfo>(() => this.api.getSystem());
  github = new Loadable<GitHubAppStatus>(() => this.api.getGitHubApp());

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.system.load();
    void this.github.load();
  }

  <template>
    {{pageTitle "管理"}}

    <MembersPanel />

    {{! A server without this endpoint has nothing to show here. }}
    {{#unless this.github.unavailable}}
      <section
        class="pane"
        id="github-app"
        aria-labelledby="github-title"
        data-test-github-app
      >
        <div class="pane-header">
          <h2 class="pane-title" id="github-title">GitHub App</h2>
        </div>
        <AsyncView
          @data={{this.github}}
          @errorTitle="GitHub App の情報を取得できませんでした"
          as |gh|
        >
          {{#if gh.app}}
            <dl class="readout">
              <dt>App</dt>
              <dd>
                <a
                  href={{gh.app.html_url}}
                  target="_blank"
                  rel="noopener noreferrer"
                  class="mono"
                  data-test-github-app-link
                >{{gh.app.slug}}</a>
              </dd>
            </dl>
            <ul class="box job-rows" data-test-github-installations>
              {{#each gh.installations as |inst|}}
                <li class="job" data-test-github-installation={{inst.account}}>
                  <span class="job-name github-account">
                    <span class="mono">{{inst.account}}</span>
                    <span class="github-account-id">
                      ID
                      <code
                        class="mono"
                        data-test-github-account-id
                      >{{inst.account_id}}</code>
                      <CopyButton
                        @text={{idText inst.account_id}}
                        @label="{{inst.account}} のアカウント ID"
                      />
                    </span>
                  </span>
                  <span class="job-state">
                    {{#if inst.used}}
                      <StatusIndicator
                        @kind="running"
                        @label="使う"
                        @quiet={{true}}
                      />
                    {{else}}
                      <StatusIndicator @kind="pending" @label="使わない" />
                    {{/if}}
                  </span>
                  <span class="job-facts">{{selectionLabel
                      inst.repository_selection
                    }}</span>
                  {{#if inst.login_only}}
                    <p class="job-note" data-test-github-login-only>
                      login だけで一致しています。org の名前を変えると、元の
                      login は別のアカウントが取れます。下の 3 の行で ID
                      を指定してください。
                    </p>
                  {{/if}}
                </li>
              {{else}}
                <li class="job" data-test-github-no-installation>
                  <span
                    class="job-name"
                  >まだどこにもインストールされていません。</span>
                </li>
              {{/each}}
            </ul>

            <div class="github-guide" data-test-github-guide>
              <h3 class="github-guide-title">別の org のリポジトリを使う</h3>
              <p class="github-guide-lead">
                自分が管理する別の org の private
                リポジトリをデプロイするには、App をその org
                にもインストールし、koroku
                が使うインストール先に加えます。public
                リポジトリなら、インストールせずに作成画面で URL
                を指定するだけで使えます。
              </p>
              <ol class="github-guide-steps">
                <li>
                  <span>App
                    をどのアカウントにもインストールできるようにします（App
                    の設定の
                    <strong>Advanced</strong>
                    →
                    <strong>Make public</strong>）。誰でもインストールできるようになりますが、koroku
                    は 3 で加えたアカウントのインストールしか使いません。</span>
                  {{#if gh.app.settings_url}}
                    <a
                      class={{buttonClass "default" "small"}}
                      href="{{gh.app.settings_url}}/advanced"
                      target="_blank"
                      rel="noopener noreferrer"
                      data-test-github-settings
                    >App の設定を開く<Icon @name="external" /></a>
                  {{/if}}
                </li>
                <li>
                  <span>App をその org
                    にインストールし、デプロイするリポジトリを選びます。</span>
                  <a
                    class={{buttonClass "default" "small"}}
                    href={{gh.app.install_url}}
                    target="_blank"
                    rel="noopener noreferrer"
                    data-test-github-install
                  >インストール先を選ぶ<Icon @name="external" /></a>
                </li>
                <li>
                  <span>サーバーの
                    <code>/etc/koroku/config.toml</code>
                    の
                    <code>[github]</code>
                    に次の行を書き、koroku
                    を再起動します。数値は上の一覧のアカウント ID
                    です。「使わない」と表示されているインストール先は、ここに加えると使われます。{{#if
                      (hasLogins gh)
                    }}<span data-test-github-remove-logins>
                        <code>installation_accounts</code>
                        の行は消します。</span>{{/if}}</span>
                  <span class="github-guide-config">
                    <code class="mono" data-test-github-config>{{accountIDsLine
                        gh
                      }}</code>
                    {{#if (hasAccountIDs gh)}}
                      <CopyButton
                        @text={{accountIDsLine gh}}
                        @label="config.toml に書く行"
                      />
                    {{/if}}
                  </span>
                </li>
              </ol>
            </div>
          {{else}}
            <p class="github-unconfigured" data-test-github-unconfigured>
              GitHub App が設定されていません。設定すると、GitHub
              でログインし、private リポジトリをデプロイできます（docs/SETUP.md
              の 4 節）。
            </p>
          {{/if}}
        </AsyncView>
      </section>
    {{/unless}}

    {{#if this.system.value}}
      {{#let this.system.value as |system|}}
        <section class="pane" aria-labelledby="versions-title">
          <div class="pane-header">
            <h2 class="pane-title" id="versions-title">バージョン</h2>
          </div>
          <dl class="readout system-versions">
            <dt>koroku</dt>
            <dd data-test-koroku-version>{{system.version}}</dd>
            <dt>Docker</dt>
            <dd data-test-docker-version>
              {{#if system.docker_version}}
                {{system.docker_version}}
              {{else}}
                <span class="fact-none">取得できません</span>
              {{/if}}
            </dd>
            <dt>ベースドメイン</dt>
            <dd><span class="mono">{{system.base_domain}}</span></dd>
          </dl>
        </section>
      {{/let}}
    {{/if}}
  </template>
}
