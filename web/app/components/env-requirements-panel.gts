import Component from '@glimmer/component';
import { on } from '@ember/modifier';
import { fn } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import Badge from 'koroku-console/components/badge';
import Banner from 'koroku-console/components/banner';
import Button from 'koroku-console/components/button';
import Icon from 'koroku-console/components/icon';
import {
  missingVariables,
  reasonText,
  sourceFile,
  unlistedKeys,
  type Missing,
} from 'koroku-console/utils/env-requirements';
import { shortSha } from 'koroku-console/utils/format';
import type Loadable from 'koroku-console/utils/loadable';
import type {
  EnvRequirement,
  EnvRequirements,
} from 'koroku-console/utils/types';

interface Signature {
  Element: HTMLElement;
  Args: {
    slug: string;
    requirements: Loadable<EnvRequirements>;
    /** The keys saved right now (the list on screen). */
    savedKeys: string[];
    /** Rows can be added (false while values are hidden). */
    canAdd: boolean;
    onAdd: (key: string) => void;
  };
}

const VAR_SYNTAX = '${VAR}';

function defaultText(v: EnvRequirement): string {
  if (v.default === null) return '';
  return v.default === '' ? '既定値: 空' : `既定値: ${v.default}`;
}

/**
 * The variables the repository needs (ARCHITECTURE 9.4), compared with the
 * saved ones on the environment tab. Quiet when everything is set; a
 * warning when a required variable is missing. Reading the repository
 * can fail without getting in the way of the tab.
 */
export default class EnvRequirementsPanel extends Component<Signature> {
  get value(): EnvRequirements | null {
    return this.args.requirements.value;
  }

  get saved(): Set<string> {
    return new Set(this.args.savedKeys);
  }

  get missing(): Missing {
    return missingVariables(this.value, this.saved);
  }

  get missingList(): EnvRequirement[] {
    return [...this.missing.required, ...this.missing.optional];
  }

  get requiredCount(): number {
    return this.missing.required.length;
  }

  get optionalCount(): number {
    return this.missing.optional.length;
  }

  get title(): string {
    return this.requiredCount
      ? `必須の環境変数が ${this.requiredCount} 件未設定です`
      : `任意の環境変数が ${this.optionalCount} 件未設定です`;
  }

  get unlisted(): string[] {
    return unlistedKeys(this.value, this.args.savedKeys);
  }

  get unlistedText(): string {
    return this.unlisted.join('、');
  }

  get isOk(): boolean {
    return this.value?.status === 'ok';
  }

  get isUnavailable(): boolean {
    return this.value?.status === 'unavailable';
  }

  get total(): number {
    return this.value?.variables.length ?? 0;
  }

  get file(): string {
    return this.value ? sourceFile(this.value) : '';
  }

  get isDotenv(): boolean {
    return this.value?.source === 'dotenv';
  }

  get where(): string {
    const v = this.value;
    if (!v) return '';
    const sha = shortSha(v.commit_sha);
    return sha ? `${v.ref || 'コミット'}（${sha}）` : v.ref;
  }

  get reason(): string {
    return reasonText(this.value?.reason ?? '');
  }

  get loadFailed(): boolean {
    return this.args.requirements.error !== null;
  }

  <template>
    <div class="env-reqs" aria-live="polite" data-test-env-reqs ...attributes>
      {{#if @requirements.loading}}
        <p class="env-reqs-quiet" data-test-env-reqs-loading>
          <Icon @name="spinner" />
          リポジトリが必要とする変数を確認しています…
        </p>
      {{else if this.loadFailed}}
        <p class="env-reqs-quiet" data-test-env-reqs-error>
          <Icon @name="info" />
          リポジトリが必要とする変数を確認できませんでした。
        </p>
      {{else if this.isUnavailable}}
        <p class="env-reqs-quiet" data-test-env-reqs-unavailable>
          <Icon @name="info" />
          <span>
            {{this.where}}
            の
            <span class="mono">{{this.file}}</span>
            と照合できませんでした。{{this.reason}}
            <LinkTo @route="app.settings" @model={{@slug}}>照合元の設定</LinkTo>
          </span>
        </p>
      {{else if this.isOk}}
        {{#if this.missingList.length}}
          <Banner
            @tone={{if this.requiredCount "warning" "info"}}
            @title={{this.title}}
            data-test-env-reqs-missing
          >
            <p class="env-reqs-source">
              {{this.where}}
              の
              <span class="mono">{{this.file}}</span>
              {{#if this.isDotenv}}
                に書かれた変数と照合しています。
              {{else}}
                が参照する変数（<code>{{VAR_SYNTAX}}</code>）と照合しています。
              {{/if}}
              {{#if this.requiredCount}}
                未設定のままデプロイすると、起動に失敗するか空の値で動きます。
              {{else}}
                未設定でも既定値で動きます。
              {{/if}}
            </p>
            <ul class="env-reqs-list" data-test-env-reqs-list>
              {{#each this.missingList key="key" as |v|}}
                <li data-test-env-req={{v.key}}>
                  <span class="env-reqs-key mono">{{v.key}}</span>
                  {{#if v.required}}
                    <Badge @tone="warn" data-test-env-req-required>必須</Badge>
                  {{else}}
                    <Badge data-test-env-req-optional>任意</Badge>
                  {{/if}}
                  <span class="env-reqs-note">
                    {{#if v.description}}{{v.description}}{{/if}}
                    {{#if (defaultText v)}}
                      <span class="mono">{{defaultText v}}</span>
                    {{/if}}
                  </span>
                  {{#if @canAdd}}
                    <Button
                      @size="small"
                      class="env-reqs-add"
                      aria-label="{{v.key}} を追加"
                      data-test-env-req-add
                      {{on "click" (fn @onAdd v.key)}}
                    >追加</Button>
                  {{/if}}
                </li>
              {{/each}}
            </ul>
          </Banner>
        {{else if this.total}}
          <p class="env-reqs-quiet" data-test-env-reqs-ok>
            <Icon @name="check" />
            <span>
              {{this.where}}
              の
              <span class="mono">{{this.file}}</span>
              が必要とする
              {{this.total}}
              件の変数は、すべて設定済みです。
            </span>
          </p>
        {{else}}
          <p class="env-reqs-quiet" data-test-env-reqs-none>
            <Icon @name="check" />
            <span>
              {{this.where}}
              の
              <span class="mono">{{this.file}}</span>
              {{#if this.isDotenv}}
                に変数は書かれていません。
              {{else}}
                は環境変数を参照していません（<code>{{VAR_SYNTAX}}</code>
                がありません）。
              {{/if}}
            </span>
          </p>
        {{/if}}
        {{#if this.unlisted.length}}
          <p class="env-reqs-quiet" data-test-env-reqs-unlisted>
            <Icon @name="info" />
            <span>
              <span class="mono">{{this.file}}</span>
              にない変数:
              <span class="mono">{{this.unlistedText}}</span>。
              アプリが使わなくなった変数なら削除できます。
            </span>
          </p>
        {{/if}}
      {{/if}}
    </div>
  </template>
}
