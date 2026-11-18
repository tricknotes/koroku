import { pageTitle } from 'ember-page-title';

<template>
  {{pageTitle "PaaS コンソール"}}

  <header class="app-header">
    <h1 class="app-header-title">PaaS コンソール</h1>
  </header>

  <main class="app-main">
    {{outlet}}
  </main>
</template>
