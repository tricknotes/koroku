import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from 'paas-console/components/async-view';
import Badge from 'paas-console/components/badge';
import ConfirmDialog from 'paas-console/components/confirm-dialog';
import DeploymentSummary from 'paas-console/components/deployment-summary';
import ErrorMessage from 'paas-console/components/error-message';
import type ApiService from 'paas-console/services/api';
import type PollerService from 'paas-console/services/poller';
import type AppContext from 'paas-console/utils/app-context';
import Loadable from 'paas-console/utils/loadable';
import {
  APP_STATE_LABELS,
  APP_STATE_TONES,
  deriveAppState,
  serviceTone,
} from 'paas-console/utils/status';
import type { App, ServiceStatus } from 'paas-console/utils/types';

type ControlAction = 'start' | 'stop' | 'restart';

const ACTION_DONE: Record<ControlAction, string> = {
  start: '起動しました。',
  stop: '停止しました。',
  restart: '再起動しました。',
};

const POLL_INTERVAL = 5000;

function stateLabel(app: App): string {
  return APP_STATE_LABELS[deriveAppState(app)];
}

function stateTone(app: App) {
  return APP_STATE_TONES[deriveAppState(app)];
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppOverviewTemplate extends Component<Signature> {
  @service declare api: ApiService;
  @service declare poller: PollerService;

  services = new Loadable<ServiceStatus[]>(() =>
    this.api.listServices(this.args.model.slug),
  );

  @tracked running: ControlAction | null = null;
  @tracked error: unknown = null;
  @tracked notice = '';
  @tracked confirmingStop = false;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.services.load();
    this.poller.every(this, POLL_INTERVAL, () => this.refreshWhileDeploying());
  }

  get app(): App {
    return this.args.model.app;
  }

  get busy(): boolean {
    return this.running !== null;
  }

  get repository(): string {
    return this.app.repo_full_name || this.app.repo_url;
  }

  /** The latest deployment, when it is not the one that is serving. */
  get latestIfDifferent() {
    const { latest_deployment: latest, current_deployment: current } = this.app;
    return latest && latest.id !== current?.id ? latest : null;
  }

  async refreshWhileDeploying(): Promise<void> {
    if (deriveAppState(this.app) !== 'deploying') return;
    await this.args.model.reload();
    await this.services.load({ quiet: true });
  }

  requestStop = () => {
    if (this.args.model.isProduction) {
      this.confirmingStop = true;
    } else {
      void this.control('stop');
    }
  };

  cancelStop = () => {
    this.confirmingStop = false;
  };

  confirmStop = async () => {
    await this.control('stop');
    this.confirmingStop = false;
  };

  control = async (action: ControlAction) => {
    this.running = action;
    this.error = null;
    this.notice = '';
    try {
      const app = await this.api.controlApp(this.app.slug, action);
      this.args.model.set(app);
      this.notice = ACTION_DONE[action];
      await this.services.load({ quiet: true });
    } catch (error) {
      this.error = error;
    } finally {
      this.running = null;
    }
  };

  <template>
    <section class="card" aria-labelledby="overview-title">
      <h3 id="overview-title">概要</h3>

      <dl class="definition">
        <dt>状態</dt>
        <dd>
          <Badge @tone={{stateTone this.app}} data-test-overview-state>
            {{stateLabel this.app}}
          </Badge>
        </dd>
        <dt>URL</dt>
        <dd>
          {{#if this.app.url}}
            <a href={{this.app.url}} target="_blank" rel="noopener noreferrer">
              {{this.app.url}}
            </a>
          {{/if}}
        </dd>
        <dt>リポジトリ</dt>
        <dd>{{this.repository}}</dd>
        <dt>稼働中のデプロイ</dt>
        <dd data-test-current-deployment>
          <DeploymentSummary @deployment={{this.app.current_deployment}} />
        </dd>
        {{#if this.latestIfDifferent}}
          <dt>最新のデプロイ</dt>
          <dd data-test-latest-deployment>
            <DeploymentSummary
              @deployment={{this.latestIfDifferent}}
              @showStatus={{true}}
            />
          </dd>
        {{/if}}
      </dl>

      <div class="button-row">
        <button
          type="button"
          class="button"
          disabled={{this.busy}}
          data-test-control="start"
          {{on "click" (fn this.control "start")}}
        >起動</button>
        <button
          type="button"
          class="button"
          disabled={{this.busy}}
          data-test-control="stop"
          {{on "click" this.requestStop}}
        >停止</button>
        <button
          type="button"
          class="button"
          disabled={{this.busy}}
          data-test-control="restart"
          {{on "click" (fn this.control "restart")}}
        >再起動</button>
      </div>

      <div aria-live="polite">
        {{#if this.busy}}
          <p class="muted">実行中…（完了まで待ちます）</p>
        {{else if this.notice}}
          <p class="notice" data-test-notice>{{this.notice}}</p>
        {{/if}}
        <ErrorMessage @error={{this.error}} />
      </div>
    </section>

    <section class="card" aria-labelledby="services-title">
      <h3 id="services-title">サービス</h3>
      <AsyncView @data={{this.services}} as |services|>
        {{#if services.length}}
          <div class="table-wrap">
            <table class="table" data-test-services>
              <thead>
                <tr>
                  <th scope="col">名前</th>
                  <th scope="col">状態</th>
                  <th scope="col">詳細</th>
                  <th scope="col">ヘルスチェック</th>
                </tr>
              </thead>
              <tbody>
                {{#each services key="name" as |svc|}}
                  <tr data-test-service={{svc.name}}>
                    <th scope="row">{{svc.name}}</th>
                    <td>
                      <Badge @tone={{serviceTone svc.state svc.health}}>
                        {{svc.state}}
                      </Badge>
                    </td>
                    <td>{{svc.status}}</td>
                    <td>{{if svc.health svc.health "—"}}</td>
                  </tr>
                {{/each}}
              </tbody>
            </table>
          </div>
        {{else}}
          <p class="muted">動いているサービスはありません。</p>
        {{/if}}
      </AsyncView>
    </section>

    {{#if this.confirmingStop}}
      <ConfirmDialog
        @title="本番アプリを停止しますか？"
        @confirmLabel="停止する"
        @danger={{true}}
        @busy={{this.busy}}
        @onConfirm={{this.confirmStop}}
        @onCancel={{this.cancelStop}}
      >
        <p>本番アプリ
          <strong>{{this.app.slug}}</strong>
          を停止します。停止中は 503 のページが返ります。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
