import { LinkTo } from '@ember/routing';
import { pageTitle } from 'ember-page-title';

<template>
  {{pageTitle "ページが見つかりません"}}

  <section class="card" data-test-not-found>
    <h2>ページが見つかりません</h2>
    <p><LinkTo @route="index">アプリ一覧に戻る</LinkTo></p>
  </section>
</template>
