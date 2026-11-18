import Component from '@glimmer/component';
import { service } from '@ember/service';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import { pageTitle } from 'ember-page-title';
import AsyncView from 'paas-console/components/async-view';
import Badge from 'paas-console/components/badge';
import DeploymentSummary from 'paas-console/components/deployment-summary';
import type ApiService from 'paas-console/services/api';
import type PollerService from 'paas-console/services/poller';
import Loadable from 'paas-console/utils/loadable';
import {
  APP_STATE_LABELS,
  APP_STATE_TONES,
  ENVIRONMENT_LABELS,
  deriveAppState,
} from 'paas-console/utils/status';
import type { App } from 'paas-console/utils/types';

const POLL_INTERVAL = 5000;

function stateLabel(app: App): string {
  return APP_STATE_LABELS[deriveAppState(app)];
}

function stateTone(app: App) {
  return APP_STATE_TONES[deriveAppState(app)];
}

function environmentLabel(app: App): string {
  return ENVIRONMENT_LABELS[app.environment] ?? app.environment;
}

export default class IndexTemplate extends Component {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  apps = new Loadable<App[]>(() => this.api.listApps());

  constructor(owner: Owner, args: object) {
    super(owner, args);
    void this.apps.load();
    this.poller.every(this, POLL_INTERVAL, () => this.refreshWhileDeploying());
  }

  /** Keeps the states fresh while some app has a deployment in progress. */
  async refreshWhileDeploying(): Promise<void> {
    const deploying = (this.apps.value ?? []).some(
      (app) => deriveAppState(app) === 'deploying',
    );
    if (deploying) await this.apps.load({ quiet: true });
  }

  <template>
    {{pageTitle "アプリ"}}

    <div class="page-header">
      <h2>アプリ</h2>
      <LinkTo @route="new-app" class="button button-primary" data-test-new-app>
        アプリを作成
      </LinkTo>
    </div>

    <AsyncView @data={{this.apps}} as |apps|>
      {{#if apps.length}}
        <ul class="app-list">
          {{#each apps key="slug" as |app|}}
            <li class="app-row" data-test-app={{app.slug}}>
              <div class="app-row-name">
                <LinkTo @route="app" @model={{app.slug}} class="app-row-link">
                  {{app.slug}}
                </LinkTo>
                <Badge
                  class="badge-env-{{app.environment}}"
                  data-test-environment
                >{{environmentLabel app}}</Badge>
              </div>
              <div class="app-row-state">
                <Badge @tone={{stateTone app}} data-test-state>
                  {{stateLabel app}}
                </Badge>
              </div>
              <div class="app-row-url">
                {{#if app.url}}
                  <a
                    href={{app.url}}
                    target="_blank"
                    rel="noopener noreferrer"
                    data-test-url
                  >{{app.url}}</a>
                {{/if}}
              </div>
              <div class="app-row-deploy" data-test-last-deployment>
                {{#if app.latest_deployment}}
                  <DeploymentSummary @deployment={{app.latest_deployment}} />
                {{else}}
                  <span class="muted">デプロイ履歴なし</span>
                {{/if}}
              </div>
            </li>
          {{/each}}
        </ul>
      {{else}}
        <div class="placeholder" data-test-empty>
          <p class="placeholder-title">アプリはまだありません。</p>
          <p class="muted">「アプリを作成」から GitHub
            リポジトリを選んで始めます。</p>
        </div>
      {{/if}}
    </AsyncView>
  </template>
}
