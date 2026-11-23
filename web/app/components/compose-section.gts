import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, uniqueId } from '@ember/helper';
import type Owner from '@ember/owner';
import { LinkTo } from '@ember/routing';
import AsyncView from 'koroku-console/components/async-view';
import Badge from 'koroku-console/components/badge';
import Banner from 'koroku-console/components/banner';
import Button, { buttonClass } from 'koroku-console/components/button';
import CopyButton from 'koroku-console/components/copy-button';
import EmptyState from 'koroku-console/components/empty-state';
import FormField from 'koroku-console/components/form-field';
import RelativeTime from 'koroku-console/components/relative-time';
import StatusIndicator from 'koroku-console/components/status-indicator';
import TabSection from 'koroku-console/components/tab-section';
import focusSection from 'koroku-console/modifiers/focus-section';
import type ApiService from 'koroku-console/services/api';
import {
  containerStatus,
  createdBy,
  imageIdentity,
  memoryLimit,
  memoryReservation,
  mismatchMarkers,
  oomScoreAdj,
  portsText,
  releaseLabel,
} from 'koroku-console/utils/compose-view';
import { inputValue } from 'koroku-console/utils/dom';
import { ApiError } from 'koroku-console/utils/errors';
import Loadable from 'koroku-console/utils/loadable';
import type {
  App,
  ComposeView,
  ContainerInfo,
  ContainerView,
} from 'koroku-console/utils/types';

function errorCode(error: unknown): string {
  return error instanceof ApiError ? error.code : '';
}

function hasMismatches(c: ContainerInfo): boolean {
  return c.mismatches.length > 0;
}

function troubled(view: ContainerView): number {
  return view.containers.filter(hasMismatches).length;
}

interface Signature {
  Args: {
    app: App;
    /** Led here by a link (`?section=compose`): open and in view. */
    focus: boolean;
  };
}

/**
 * The Compose setup of the app, read-only (ARCHITECTURE 9.10), as the last
 * section of the resources tab: its containers compared with the current
 * release, and the override and the merged configuration of the current or
 * a kept release. Every value of a variable is masked by the server. It
 * starts closed and asks Docker only once opened, and then on demand;
 * nothing polls.
 */
export default class ComposeSection extends Component<Signature> {
  @service declare api: ApiService;

  @tracked open: boolean;

  /** The release whose files are shown; null for the current one. */
  @tracked releaseId: number | null = null;

  containers = new Loadable<ContainerView>(() =>
    this.api.getContainers(this.app.slug),
  );

  view = new Loadable<ComposeView>(() =>
    this.api.getComposeView(this.app.slug, this.releaseId ?? undefined),
  );

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.open = args.focus;
    if (this.open) this.loadAll();
  }

  get app(): App {
    return this.args.app;
  }

  private loadAll(): void {
    void this.containers.load();
    void this.view.load();
  }

  toggle = () => {
    this.open = !this.open;
    if (this.open && !this.containers.loaded && !this.view.loaded) {
      this.loadAll();
    }
  };

  get notDeployed(): boolean {
    return errorCode(this.view.error) === 'not_deployed';
  }

  get releaseGone(): boolean {
    return errorCode(this.view.error) === 'release_unavailable';
  }

  selectRelease = (event: Event) => {
    const id = Number(inputValue(event));
    const current = this.view.value?.current_deployment_id;
    this.releaseId = id === current ? null : id;
    void this.view.load({ quiet: true });
  };

  backToCurrent = () => {
    this.releaseId = null;
    void this.view.load();
  };

  reloadAll = () => {
    void this.containers.load({ quiet: this.containers.loaded });
    void this.view.load({ quiet: this.view.loaded });
  };

  get reloading(): boolean {
    return this.containers.loading || this.view.loading;
  }

  <template>
    {{#let (uniqueId) as |id|}}
      <section
        class="pane compose-section"
        id="compose"
        tabindex="-1"
        aria-labelledby="{{id}}-title"
        data-test-compose-section
        {{focusSection @focus}}
      >
        <header class="pane-header">
          <h2 class="pane-title" id="{{id}}-title">構成</h2>
          <Button
            @size="small"
            aria-expanded={{if this.open "true" "false"}}
            aria-controls="{{id}}-body"
            data-test-compose-toggle
            {{on "click" this.toggle}}
          >{{if this.open "閉じる" "表示"}}</Button>
        </header>
        <p class="pane-description">
          koroku がリポジトリの Compose
          に重ねている設定と、いま動いているコンテナです。読み取り専用で、表示したときに
          Docker
          から読みます。環境変数の値はどこにも出しません（キーだけを出します）。
        </p>
        <div id="{{id}}-body">
          {{#if this.open}}
            <div class="compose-view" data-test-compose>
              <TabSection
                @level={{3}}
                @title="コンテナ"
                data-test-compose-containers
              >
                <:actions>
                  <Button
                    @size="small"
                    @busy={{this.reloading}}
                    @busyLabel="読み込み中"
                    data-test-compose-reload
                    {{on "click" this.reloadAll}}
                  >再読み込み</Button>
                </:actions>
                <:default>
                  <p class="pane-description">
                    <code>docker container inspect</code>
                    で読んだ値を、稼働中のリリースと比べます。違うところや不調には印を付けます。
                  </p>
                  <AsyncView
                    @data={{this.containers}}
                    @errorTitle="コンテナを読み取れませんでした"
                  >
                    <:default as |data|>
                      {{#if data.comparison_error}}
                        <Banner
                          @tone="warning"
                          @title="稼働中のリリースと比べられませんでした"
                          data-test-compose-comparison-error
                        >
                          <span
                            class="mono compose-error-text"
                          >{{data.comparison_error}}</span>
                        </Banner>
                      {{/if}}
                      {{#if data.containers.length}}
                        {{#let (troubled data) as |count|}}
                          {{#if count}}
                            <Banner @tone="warning" data-test-compose-trouble>
                              {{data.containers.length}}
                              個のコンテナのうち
                              {{count}}
                              個に、稼働中のリリースとの違いか不調があります。
                            </Banner>
                          {{/if}}
                        {{/let}}
                        <ul class="compose-containers" aria-label="コンテナ">
                          {{#each data.containers key="name" as |c|}}
                            {{#let (containerStatus c) as |status|}}
                              <li
                                class="compose-container
                                  {{if (hasMismatches c) 'has-mismatch'}}"
                                data-test-container={{c.service}}
                              >
                                <div class="compose-container-head">
                                  <span
                                    class="compose-container-service mono"
                                  >{{c.service}}</span>
                                  <StatusIndicator
                                    @kind={{status.kind}}
                                    @label={{status.label}}
                                    data-test-container-state
                                  />
                                  {{#if (eq c.current true)}}
                                    <Badge
                                      @tone="ok"
                                      data-test-container-current
                                    >稼働中のリリース</Badge>
                                  {{/if}}
                                </div>
                                <p
                                  class="compose-container-name mono"
                                >{{c.name}}</p>
                                {{#if c.mismatches.length}}
                                  <ul class="compose-markers" aria-label="印">
                                    {{#each
                                      (mismatchMarkers c) key="code"
                                      as |m|
                                    }}
                                      <li
                                        title={{m.hint}}
                                        data-test-mismatch={{m.code}}
                                      >
                                        <Badge
                                          @tone={{m.tone}}
                                        >{{m.label}}</Badge>
                                        <span
                                          class="compose-marker-hint"
                                        >{{m.hint}}</span>
                                      </li>
                                    {{/each}}
                                  </ul>
                                {{/if}}
                                <dl class="compose-facts">
                                  <div>
                                    <dt>イメージ</dt>
                                    <dd>
                                      <span class="mono">{{c.image}}</span>
                                      <span
                                        class="compose-secondary mono"
                                        title={{if
                                          c.image_digest
                                          c.image_digest
                                          c.image_id
                                        }}
                                      >{{imageIdentity c}}</span>
                                    </dd>
                                  </div>
                                  <div>
                                    <dt>作成</dt>
                                    <dd>
                                      <RelativeTime @time={{c.created_at}} />
                                      {{#let (createdBy c) as |by|}}
                                        {{#if by}}
                                          <span
                                            class="compose-secondary"
                                            data-test-container-created-by
                                          >{{by}}</span>
                                        {{/if}}
                                      {{/let}}
                                    </dd>
                                  </div>
                                  <div>
                                    <dt>自動の再起動</dt>
                                    <dd
                                      data-test-container-restarts
                                    >{{c.restart_count}}
                                      回</dd>
                                  </div>
                                  {{#let (memoryLimit c) as |v|}}
                                    <div class={{if v.differs "is-different"}}>
                                      <dt>メモリ上限</dt>
                                      <dd data-test-container-limit>
                                        {{v.value}}
                                        {{#if v.differs}}
                                          <span
                                            class="compose-expected"
                                          >リリースでは
                                            {{v.expected}}</span>
                                        {{/if}}
                                      </dd>
                                    </div>
                                  {{/let}}
                                  {{#let (memoryReservation c) as |v|}}
                                    <div class={{if v.differs "is-different"}}>
                                      <dt>メモリの予約</dt>
                                      <dd data-test-container-reservation>
                                        {{v.value}}
                                        {{#if v.differs}}
                                          <span
                                            class="compose-expected"
                                          >リリースでは
                                            {{v.expected}}</span>
                                        {{/if}}
                                      </dd>
                                    </div>
                                  {{/let}}
                                  {{#let (oomScoreAdj c) as |v|}}
                                    <div class={{if v.differs "is-different"}}>
                                      <dt>oom_score_adj</dt>
                                      <dd class="mono" data-test-container-oom>
                                        {{v.value}}
                                        {{#if v.differs}}
                                          <span
                                            class="compose-expected"
                                          >リリースでは
                                            {{v.expected}}</span>
                                        {{/if}}
                                      </dd>
                                    </div>
                                  {{/let}}
                                  {{#if c.ports.length}}
                                    <div>
                                      <dt>公開ポート</dt>
                                      <dd class="mono">{{portsText c}}</dd>
                                    </div>
                                  {{/if}}
                                </dl>
                              </li>
                            {{/let}}
                          {{/each}}
                        </ul>
                      {{else}}
                        <EmptyState
                          @title="コンテナはありません"
                          @description="デプロイすると、サービスごとのコンテナをここに出します。"
                          data-test-compose-containers-empty
                        />
                      {{/if}}
                    </:default>
                  </AsyncView>
                </:default>
              </TabSection>

              <TabSection
                @level={{3}}
                @title="Compose の設定"
                data-test-compose-files
              >
                {{#if this.notDeployed}}
                  <EmptyState
                    @title="まだデプロイされていません"
                    @description="デプロイすると、koroku が重ねた override と、重ねた結果をここに出します。"
                    data-test-compose-not-deployed
                  >
                    <:action>
                      <LinkTo
                        @route="app.deploy"
                        @model={{this.app.slug}}
                        class={{buttonClass "default"}}
                      >デプロイへ</LinkTo>
                    </:action>
                  </EmptyState>
                {{else if this.releaseGone}}
                  <Banner
                    @tone="warning"
                    @title="この版のファイルはもう残っていません"
                    data-test-compose-release-gone
                  >
                    <:default>koroku が保持するのは直近 3 つのリリースだけです。</:default>
                    <:actions>
                      <Button
                        @size="small"
                        {{on "click" this.backToCurrent}}
                      >稼働中のリリースを表示</Button>
                    </:actions>
                  </Banner>
                {{else}}
                  <p class="pane-description">
                    デプロイと同じファイルの組み合わせで
                    <code>docker compose config --no-interpolate
                      --no-env-resolution</code>
                    を実行した結果です。
                    <code>&#36;{VAR}</code>
                    は補間せずに書いたまま、env_file はパスのままで出します。
                    <code>environment</code>
                    と build の
                    <code>args</code>
                    の値は伏せています。
                  </p>
                  <AsyncView
                    @data={{this.view}}
                    @errorTitle="Compose の設定を読み取れませんでした"
                  >
                    <:default as |data|>
                      {{#if data.releases.length}}
                        <FormField
                          class="compose-release"
                          @label="リリース"
                          @hint="保持しているリリース（直近 3 つ）から選べます。"
                          as |field|
                        >
                          <select
                            id={{field.id}}
                            aria-describedby={{field.describedBy}}
                            data-test-compose-release
                            {{on "change" this.selectRelease}}
                          >
                            {{#each data.releases key="deployment_id" as |r|}}
                              <option
                                value={{r.deployment_id}}
                                selected={{eq
                                  r.deployment_id
                                  data.deployment_id
                                }}
                              >{{releaseLabel r}}</option>
                            {{/each}}
                          </select>
                        </FormField>
                      {{/if}}
                      {{#if this.view.stale}}
                        <Banner @tone="warning" data-test-stale>
                          選んだリリースを読み取れませんでした。表示は前に読み取ったものです。
                        </Banner>
                      {{/if}}
                      <div class="compose-files">
                        <div class="compose-file" data-test-compose-override>
                          <div class="compose-file-header">
                            <h4>koroku の override</h4>
                            <span
                              class="compose-file-name mono"
                            >koroku.override.yaml</span>
                            <CopyButton
                              @text={{data.override}}
                              @label="override"
                            />
                          </div>
                          <pre
                            class="compose-code"
                            tabindex="0"
                            aria-label="koroku.override.yaml の内容"
                          >{{data.override}}</pre>
                        </div>
                        <div class="compose-file" data-test-compose-config>
                          <div class="compose-file-header">
                            <h4>重ねた結果</h4>
                            <span
                              class="compose-file-name mono"
                            >{{data.compose_file}}
                              + koroku.override.yaml</span>
                            {{#if data.config}}
                              <CopyButton
                                @text={{data.config}}
                                @label="重ねた結果"
                              />
                            {{/if}}
                          </div>
                          {{#if data.config}}
                            <pre
                              class="compose-code"
                              tabindex="0"
                              aria-label="重ねた結果の内容"
                            >{{data.config}}</pre>
                          {{else}}
                            <Banner
                              @tone="error"
                              @title="docker compose config が失敗しました"
                              data-test-compose-config-error
                            >
                              <span
                                class="mono compose-error-text"
                              >{{data.config_error}}</span>
                            </Banner>
                          {{/if}}
                        </div>
                      </div>
                    </:default>
                  </AsyncView>
                {{/if}}
              </TabSection>
            </div>
          {{/if}}
        </div>
      </section>
    {{/let}}
  </template>
}
