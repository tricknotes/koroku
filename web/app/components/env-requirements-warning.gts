import Component from '@glimmer/component';
import { LinkTo } from '@ember/routing';
import Banner from 'koroku-console/components/banner';
import { buttonClass } from 'koroku-console/components/button';
import {
  keyList,
  missingVariables,
  sourceFile,
} from 'koroku-console/utils/env-requirements';
import type {
  EnvRequirement,
  EnvRequirements,
} from 'koroku-console/utils/types';

interface Signature {
  Element: HTMLDivElement;
  Args: {
    slug: string;
    requirements: EnvRequirements | null;
    /** Inside a dialog: no link away from it. */
    inDialog?: boolean;
  };
}

/**
 * Warns before a deployment that the chosen commit needs variables that
 * are not saved (ARCHITECTURE 9.4). It only warns: deploying stays
 * possible. Renders nothing when everything required is set or the
 * repository could not be read.
 */
export default class EnvRequirementsWarning extends Component<Signature> {
  get missing(): EnvRequirement[] {
    return missingVariables(this.args.requirements).required;
  }

  get title(): string {
    return `必須の環境変数が ${this.missing.length} 件未設定です`;
  }

  get keys(): string {
    return keyList(this.missing);
  }

  get file(): string {
    return this.args.requirements ? sourceFile(this.args.requirements) : '';
  }

  <template>
    {{#if this.missing.length}}
      <Banner
        @tone="warning"
        @title={{this.title}}
        class="env-reqs-warning"
        data-test-deploy-env-warning
        ...attributes
      >
        <:default>
          <p>
            このコミットの
            <span class="mono">{{this.file}}</span>
            が必要とする
            <span class="mono" data-test-deploy-env-missing>{{this.keys}}</span>
            が設定されていません。このままデプロイすると、起動に失敗するか空の値で動きます。
          </p>
        </:default>
        <:actions>
          {{#unless @inDialog}}
            <LinkTo
              @route="app.env"
              @model={{@slug}}
              class={{buttonClass "default" "small"}}
              data-test-deploy-env-link
            >環境変数を設定</LinkTo>
          {{/unless}}
        </:actions>
      </Banner>
    {{/if}}
  </template>
}
