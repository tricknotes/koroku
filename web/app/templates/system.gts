import { pageTitle } from 'ember-page-title';
import Tabs from 'koroku-console/components/tabs';

/**
 * The owner's system screen (ARCHITECTURE 9.1): three tabs, each loading
 * only what it shows. 状態 has the host and its containers, 定期処理 the
 * background jobs and backup runs, 管理 the members, the GitHub App and the
 * versions.
 */
<template>
  {{pageTitle "システム"}}

  <div class="page-header system-header">
    <h1>システム</h1>
  </div>

  <Tabs @label="システムのメニュー" class="system-tabs" as |tabs|>
    <tabs.Tab @route="system.index" data-test-system-tab="index">状態</tabs.Tab>
    <tabs.Tab
      @route="system.jobs"
      data-test-system-tab="jobs"
    >定期処理</tabs.Tab>
    <tabs.Tab @route="system.admin" data-test-system-tab="admin">管理</tabs.Tab>
  </Tabs>

  {{outlet}}
</template>
