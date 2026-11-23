import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { hash } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import type Owner from '@ember/owner';
import Badge from 'koroku-console/components/badge';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import ErrorMessage from 'koroku-console/components/error-message';
import Icon from 'koroku-console/components/icon';
import RelativeTime from 'koroku-console/components/relative-time';
import type ApiService from 'koroku-console/services/api';
import { ApiError } from 'koroku-console/utils/errors';
import { firstLine, shortSha } from 'koroku-console/utils/format';
import Loadable from 'koroku-console/utils/loadable';
import type {
  App,
  Deployment,
  ImageRebuildReason,
  Promotion,
  PromotionImage,
  PromotionReason,
} from 'koroku-console/utils/types';

const UNAVAILABLE: Record<PromotionReason, string> = {
  not_production: 'このアプリは本番ではないため、昇格先になれません。',
  source_not_staging:
    '昇格元のアプリがステージングではなくなっています。設定で昇格元を選び直してください。',
  repository_mismatch:
    '昇格元のアプリのリポジトリが違います。設定で昇格元を選び直してください。',
  no_source_release:
    '昇格元のアプリにはまだ成功したリリースがありません。先にそちらをデプロイしてください。',
};

const REBUILD: Record<ImageRebuildReason, string> = {
  args_differ: 'ビルド引数の値が違う',
  build_differs: 'ビルドの定義が違う',
  not_built_in_source: 'staging ではビルドしていない',
  image_missing: 'staging のイメージが残っていない',
  source_missing: 'staging のリリースのファイルが残っていない',
};

/** Why a service is built again, with the differing keys when known. */
function rebuildReason(image: PromotionImage): string {
  const reason = image.reason ? REBUILD[image.reason] : '';
  if (image.reason === 'args_differ' && image.differing_args.length > 0) {
    return `${reason}: ${image.differing_args.join(', ')}`;
  }
  return reason;
}

function unavailableMessage(reason: PromotionReason | ''): string {
  return reason ? UNAVAILABLE[reason] : '昇格できません。';
}

function commitTitle(deployment: Deployment | null): string {
  if (!deployment) return '';
  return firstLine(deployment.commit_message) || `デプロイ #${deployment.id}`;
}

interface Signature {
  Element: HTMLElement;
  Args: {
    app: App;
    /** Called with the queued deployment once a promotion is accepted. */
    onPromoted: (deployment: Deployment) => unknown;
  };
}

/**
 * "Promote staging's release to production" on a production app's deploy
 * tab (ARCHITECTURE 9.7): what would be deployed, how it differs from what
 * runs now, whether the built images are reused, and the confirmation.
 */
export default class PromotionPanel extends Component<Signature> {
  @service declare api: ApiService;

  preview = new Loadable<Promotion>(() =>
    this.api.getPromotion(this.args.app.slug),
  );

  @tracked confirming = false;
  @tracked promoting = false;
  @tracked error: unknown = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.preview.load();
  }

  get promotion(): Promotion | null {
    return this.preview.value;
  }

  get ready(): boolean {
    return this.promotion?.status === 'ready';
  }

  get source(): Deployment | null {
    return this.promotion?.source_deployment ?? null;
  }

  get current(): Deployment | null {
    return this.promotion?.current_deployment ?? null;
  }

  get sameCommit(): boolean {
    return (
      this.source !== null &&
      this.current !== null &&
      this.source.commit_sha === this.current.commit_sha
    );
  }

  get commitsKnown(): boolean {
    return this.promotion?.commits.status === 'ok' && !this.sameCommit;
  }

  get commitCount(): number {
    return this.promotion?.commits.list.length ?? 0;
  }

  get images(): PromotionImage[] {
    return this.promotion?.images.services ?? [];
  }

  get imagesChecked(): boolean {
    return this.promotion?.images.status === 'checked';
  }

  get reused(): PromotionImage[] {
    return this.images.filter((image) => image.reuse);
  }

  get rebuilt(): PromotionImage[] {
    return this.images.filter((image) => !image.reuse);
  }

  /** Every built service reuses staging's image: nothing is built. */
  get reusesAll(): boolean {
    return (
      this.imagesChecked && this.images.length > 0 && this.rebuilt.length === 0
    );
  }

  get onlyInSource(): string[] {
    return this.promotion?.env.only_in_source ?? [];
  }

  get onlyInTarget(): string[] {
    return this.promotion?.env.only_in_target ?? [];
  }

  get envDiffers(): boolean {
    return this.onlyInSource.length > 0 || this.onlyInTarget.length > 0;
  }

  request = () => {
    this.error = null;
    this.confirming = true;
  };

  cancel = () => {
    this.confirming = false;
  };

  confirm = async () => {
    const source = this.source;
    if (!source) return;
    this.promoting = true;
    this.error = null;
    try {
      const deployment = await this.api.createDeployment(this.args.app.slug, {
        promote_from_deployment: source.id,
      });
      this.confirming = false;
      await this.args.onPromoted(deployment);
    } catch (error) {
      this.confirming = false;
      this.error = error;
      if (error instanceof ApiError && error.code === 'promotion_stale') {
        await this.preview.load({ quiet: true });
      }
    } finally {
      this.promoting = false;
    }
  };

  <template>
    <section
      class="promotion"
      aria-labelledby="promotion-title"
      data-test-promotion
      ...attributes
    >
      <h2 id="promotion-title" class="promotion-title">
        staging のリリースを本番へ
      </h2>

      {{#if this.preview.loading}}
        <p
          class="promotion-note"
          role="status"
          data-test-promotion-loading
        >昇格の内容を確認しています…</p>
      {{else if this.preview.error}}
        <p class="promotion-note" data-test-promotion-error>
          昇格の内容を取得できませんでした。{{this.preview.errorMessage}}
        </p>
      {{else if this.promotion}}
        {{#if (eqStatus this.promotion "not_paired")}}
          <p class="promotion-note" data-test-promotion-unpaired>
            同じリポジトリのステージングアプリを
            <LinkTo
              @route="app.deploy"
              @query={{hash section="promotion"}}
              data-test-promotion-settings
            >下の「デプロイの設定」の「昇格元」</LinkTo>
            で選ぶと、staging で確かめたリリースをそのまま本番へ出せます。
          </p>
        {{else if (eqStatus this.promotion "unavailable")}}
          <Banner @tone="warning" data-test-promotion-unavailable>
            {{unavailableMessage this.promotion.reason}}
          </Banner>
        {{else if this.source}}
          <div class="promotion-body">
            <dl class="promotion-releases">
              <div class="promotion-release" data-test-promotion-source>
                <dt>
                  <span class="mono">{{this.promotion.source_app}}</span>
                  の稼働中
                </dt>
                <dd>
                  <span class="deploy-sha" title={{this.source.commit_sha}}>
                    {{shortSha this.source.commit_sha}}
                  </span>
                  <span class="promotion-message">
                    {{commitTitle this.source}}
                  </span>
                  <RelativeTime
                    class="promotion-time"
                    @time={{this.source.finished_at}}
                  />
                </dd>
              </div>
              <div class="promotion-arrow" aria-hidden="true">→</div>
              <div class="promotion-release" data-test-promotion-current>
                <dt>本番の稼働中</dt>
                <dd>
                  {{#if this.current}}
                    <span
                      class="deploy-sha"
                      title={{this.current.commit_sha}}
                    >{{shortSha this.current.commit_sha}}</span>
                    <span class="promotion-message">
                      {{commitTitle this.current}}
                    </span>
                  {{else}}
                    <span class="promotion-message">まだありません</span>
                  {{/if}}
                </dd>
              </div>
            </dl>

            <ul class="promotion-summary">
              <li data-test-promotion-commits>
                {{#if this.sameCommit}}
                  本番はすでにこのコミットです。
                {{else if this.commitsKnown}}
                  <details class="promotion-commits">
                    <summary>新しいコミット {{this.commitCount}} 件</summary>
                    <ul>
                      {{#each this.promotion.commits.list key="sha" as |c|}}
                        <li data-test-promotion-commit>
                          <span class="deploy-sha">{{shortSha c.sha}}</span>
                          <span>{{firstLine c.message}}</span>
                        </li>
                      {{/each}}
                    </ul>
                  </details>
                {{else}}
                  コミットの差は数えられませんでした。上の 2 つの SHA
                  で確かめてください。
                {{/if}}
              </li>

              <li data-test-promotion-images>
                {{#if this.reusesAll}}
                  <Badge @tone="ok" data-test-promotion-reuse>
                    <Icon @name="check" />
                    イメージを再利用
                  </Badge>
                  <span>staging
                    でビルドしたイメージをそのまま使い、ビルドしません。</span>
                {{else if this.imagesChecked}}
                  {{#if this.images.length}}
                    <Badge @tone="warn" data-test-promotion-rebuild>
                      ビルドし直す
                    </Badge>
                    <ul class="promotion-services">
                      {{#each this.rebuilt key="service" as |image|}}
                        <li data-test-promotion-rebuilt={{image.service}}>
                          <span class="mono">{{image.service}}</span>:
                          {{rebuildReason image}}
                        </li>
                      {{/each}}
                      {{#each this.reused key="service" as |image|}}
                        <li data-test-promotion-reused={{image.service}}>
                          <span class="mono">{{image.service}}</span>: staging
                          のイメージを再利用
                        </li>
                      {{/each}}
                    </ul>
                  {{else}}
                    <span>ビルドするサービスはありません（イメージは pull
                      します）。</span>
                  {{/if}}
                {{else}}
                  <span data-test-promotion-images-unknown>
                    イメージを再利用できるかは、デプロイの中で判定します。
                  </span>
                {{/if}}
              </li>

              {{#if this.envDiffers}}
                <li data-test-promotion-env>
                  <span>片方にだけある環境変数:</span>
                  {{#if this.onlyInSource.length}}
                    <span class="promotion-keys">staging のみ
                      {{#each this.onlyInSource as |key|}}
                        <code data-test-promotion-env-source>{{key}}</code>
                      {{/each}}
                    </span>
                  {{/if}}
                  {{#if this.onlyInTarget.length}}
                    <span class="promotion-keys">本番のみ
                      {{#each this.onlyInTarget as |key|}}
                        <code data-test-promotion-env-target>{{key}}</code>
                      {{/each}}
                    </span>
                  {{/if}}
                </li>
              {{/if}}
            </ul>

            <div class="promotion-actions">
              <Button
                @busy={{this.promoting}}
                @busyLabel="開始中…"
                data-test-promote
                {{on "click" this.request}}
              >本番へ昇格</Button>
            </div>
          </div>
        {{/if}}
      {{/if}}

      <div aria-live="polite">
        <ErrorMessage @error={{this.error}} />
      </div>
    </section>

    {{#if this.confirming}}
      <ConfirmDialog
        @title="staging のリリースを本番へ昇格しますか？"
        @confirmLabel="昇格する"
        @busy={{this.promoting}}
        @onConfirm={{this.confirm}}
        @onCancel={{this.cancel}}
      >
        <p>本番アプリ
          <strong>{{@app.slug}}</strong>
          に、<span class="mono">{{this.promotion.source_app}}</span>
          で稼働中の
          <strong data-test-dialog-target>{{shortSha this.source.commit_sha}}
            {{commitTitle this.source}}</strong>
          をデプロイします。</p>
        <p>
          {{#if this.reusesAll}}
            staging でビルドしたイメージを再利用します。
          {{else if this.rebuilt.length}}
            一部のイメージを本番の環境変数でビルドし直します（{{#each
              this.rebuilt key="service"
              as |image index|
            }}{{if index "、"}}{{image.service}}{{/each}}）。
          {{/if}}
          環境変数・volume・ドメインは本番のものを使います。コンテナの入れ替えで数秒の断があり、起動に失敗した場合は直前の成功リリースに戻ります。</p>
        {{#if this.onlyInSource.length}}
          <p class="promotion-dialog-warning" data-test-dialog-env>
            staging にだけある環境変数があります:
            {{#each this.onlyInSource as |key|}}
              <code>{{key}}</code>
            {{/each}}
          </p>
        {{/if}}
      </ConfirmDialog>
    {{/if}}
  </template>
}

function eqStatus(promotion: Promotion, status: Promotion['status']): boolean {
  return promotion.status === status;
}
