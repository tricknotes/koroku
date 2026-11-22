import type { TOC } from '@ember/component/template-only';
import { LinkTo } from '@ember/routing';
import Badge from './badge';
import TabSection from './tab-section';
import { reviewDeploying, reviewFailed } from 'koroku-console/utils/review';
import type { App } from 'koroku-console/utils/types';

function count(app: App): number {
  return app.review_apps?.count ?? 0;
}

/**
 * The parent's overview tells how many review apps there are, and how many
 * failed, and leads to the review apps tab (ARCHITECTURE 9.9).
 */
const ReviewAppsLine: TOC<{ Element: HTMLElement; Args: { app: App } }> =
  <template>
    <TabSection @title="レビューアプリ" data-test-review-line ...attributes>
      <:actions>
        <LinkTo
          @route="app.review-apps"
          @model={{@app.slug}}
          class="overview-more"
          data-test-review-line-link
        >開く</LinkTo>
      </:actions>
      <:default>
        <p class="box overview-panel review-line">
          {{#if (count @app)}}
            <span data-test-review-line-count>{{count @app}} 件</span>
            {{#let (reviewFailed @app.review_apps) as |failed|}}
              {{#if failed}}
                <Badge
                  @tone="danger"
                  data-test-review-line-failed
                >{{failed}}</Badge>
              {{/if}}
            {{/let}}
            {{#let (reviewDeploying @app.review_apps) as |deploying|}}
              {{#if deploying}}
                <span
                  class="review-line-quiet"
                  data-test-review-line-deploying
                >{{deploying}}</span>
              {{/if}}
            {{/let}}
          {{else}}
            <span class="review-line-quiet" data-test-review-line-empty>
              まだありません
            </span>
          {{/if}}
        </p>
      </:default>
    </TabSection>
  </template>;

export default ReviewAppsLine;
