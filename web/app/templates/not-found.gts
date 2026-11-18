import { LinkTo } from '@ember/routing';
import { pageTitle } from 'ember-page-title';
import { buttonClass } from 'koroku-console/components/button';
import EmptyState from 'koroku-console/components/empty-state';

<template>
  {{pageTitle "ページが見つかりません"}}

  <div class="box" data-test-not-found>
    <EmptyState
      @title="ページが見つかりません"
      @description="URL が間違っているか、アプリが削除された可能性があります。"
    >
      <:action>
        <LinkTo
          @route="index"
          class={{buttonClass "default"}}
          data-test-back-to-list
        >
          アプリ一覧へ
        </LinkTo>
      </:action>
    </EmptyState>
  </div>
</template>
