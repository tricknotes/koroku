import type { TOC } from '@ember/component/template-only';
import { array } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import { pageTitle } from 'ember-page-title';
import Badge from 'paas-console/components/badge';
import Tabs from 'paas-console/components/tabs';
import type AppContext from 'paas-console/utils/app-context';
import { errorMessage } from 'paas-console/utils/errors';
import {
  APP_STATE_LABELS,
  APP_STATE_TONES,
  ENVIRONMENT_LABELS,
  deriveAppState,
} from 'paas-console/utils/status';
import type { App } from 'paas-console/utils/types';

function stateLabel(app: App): string {
  return APP_STATE_LABELS[deriveAppState(app)];
}

function stateTone(app: App) {
  return APP_STATE_TONES[deriveAppState(app)];
}

function environmentLabel(app: App): string {
  return ENVIRONMENT_LABELS[app.environment] ?? app.environment;
}

const AppTemplate: TOC<{
  Args: { model: AppContext };
}> = <template>
  {{pageTitle @model.slug}}

  {{#if @model.loaded}}
    <div class="page-header">
      <div class="app-title">
        <h2 data-test-app-title>{{@model.app.slug}}</h2>
        <Badge
          class="badge-env-{{@model.app.environment}}"
          data-test-app-environment
        >{{environmentLabel @model.app}}</Badge>
        <Badge @tone={{stateTone @model.app}} data-test-app-state>
          {{stateLabel @model.app}}
        </Badge>
      </div>
      {{#if @model.app.url}}
        <a
          href={{@model.app.url}}
          target="_blank"
          rel="noopener noreferrer"
        >{{@model.app.url}}</a>
      {{/if}}
    </div>

    <Tabs @label="アプリのメニュー" as |tabs|>
      <tabs.Tab
        @route="app.index"
        @model={{@model.slug}}
        data-test-tab="index"
      >概要</tabs.Tab>
      <tabs.Tab
        @route="app.deploy"
        @model={{@model.slug}}
        data-test-tab="deploy"
      >デプロイ</tabs.Tab>
      <tabs.Tab
        @route="app.logs"
        @model={{@model.slug}}
        data-test-tab="logs"
      >ログ</tabs.Tab>
      <tabs.Tab
        @route="app.env"
        @model={{@model.slug}}
        data-test-tab="env"
      >環境変数</tabs.Tab>
      <tabs.Tab
        @route="app.domains"
        @model={{@model.slug}}
        data-test-tab="domains"
      >ドメイン</tabs.Tab>
      <tabs.Tab
        @route="app.backups"
        @model={{@model.slug}}
        data-test-tab="backups"
      >バックアップ</tabs.Tab>
      <tabs.Tab
        @route="app.settings"
        @model={{@model.slug}}
        data-test-tab="settings"
      >設定</tabs.Tab>
    </Tabs>

    {{! Going straight from one app to another keeps the same routes, and
        Ember would keep the tab's component (and what it loaded for the
        previous app). Keying on the context gives each app a fresh tab. }}
    {{#each (array @model) key="@identity"}}
      {{outlet}}
    {{/each}}
  {{else}}
    <section class="card" data-test-app-error>
      <h2>{{@model.slug}}</h2>
      <p class="alert alert-danger" role="alert">
        {{errorMessage @model.error}}
      </p>
      <p><LinkTo @route="index">アプリ一覧に戻る</LinkTo></p>
    </section>
  {{/if}}
</template>;

export default AppTemplate;
