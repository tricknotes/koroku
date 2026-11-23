import Component from '@glimmer/component';
import { eq } from '@ember/helper';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import { minutesLabel } from 'koroku-console/utils/settings-form';
import type SettingsEditor from 'koroku-console/utils/settings-editor';

interface Signature {
  Args: { editor: SettingsEditor };
}

/**
 * The questions a production app asks before a settings save goes out
 * (ARCHITECTURE 8.3, 8.4, 5.2), one at a time. Every tab with settings
 * renders this for its editor, so the questions are the same wherever the
 * setting lives.
 */
export default class SettingsConfirm extends Component<Signature> {
  get editor(): SettingsEditor {
    return this.args.editor;
  }

  get slug(): string {
    return this.editor.app.slug;
  }

  get waitsForWorkflow(): boolean {
    return this.editor.form.autoDeployWorkflow.trim() !== '';
  }

  <template>
    {{#if (eq @editor.confirming "protection")}}
      <ConfirmDialog
        @title={{if
          @editor.form.accessProtection
          "本番アプリを保護しますか？"
          "本番アプリの保護を外しますか？"
        }}
        @confirmLabel="保存する"
        @danger={{if @editor.form.accessProtection false true}}
        @busy={{@editor.saving}}
        @onConfirm={{@editor.confirm}}
        @onCancel={{@editor.cancel}}
      >
        {{#if @editor.form.accessProtection}}
          <p data-test-protection-confirm="on">
            保存すると、この管理画面にログインしていない利用者は、本番アプリ
            <strong>{{this.slug}}</strong>
            を開けなくなります。
          </p>
        {{else}}
          <p data-test-protection-confirm="off">
            保存すると、本番アプリ
            <strong>{{this.slug}}</strong>
            は誰でも開けるようになります。
          </p>
        {{/if}}
      </ConfirmDialog>
    {{else if (eq @editor.confirming "auto-deploy")}}
      <ConfirmDialog
        @title="本番アプリを push で自動デプロイしますか？"
        @confirmLabel="保存する"
        @busy={{@editor.saving}}
        @onConfirm={{@editor.confirm}}
        @onCancel={{@editor.cancel}}
      >
        <p data-test-auto-deploy-confirm>
          保存すると、ブランチ
          <strong
            class="mono"
          >{{@editor.form.effectiveAutoDeployBranch}}</strong>
          に push されたコミットが、{{#if this.waitsForWorkflow}}ワークフロー
            <strong class="mono">{{@editor.form.autoDeployWorkflow}}</strong>
            の成功のあと{{/if}}確認なしで本番アプリ
          <strong>{{this.slug}}</strong>
          にデプロイされます。そのブランチに push
          できる人は、本番を更新できるようになります。
        </p>
      </ConfirmDialog>
    {{else if (eq @editor.confirming "idle-sleep")}}
      <ConfirmDialog
        @title="本番アプリをアクセスがない間に止めますか？"
        @confirmLabel="保存する"
        @busy={{@editor.saving}}
        @onConfirm={{@editor.confirm}}
        @onCancel={{@editor.cancel}}
      >
        <p data-test-idle-sleep-confirm>
          保存すると、本番アプリ
          <strong>{{this.slug}}</strong>
          は{{minutesLabel
            @editor.form.idleSleepMinutes
          }}アクセスがなければ止まります。休止のあとの最初の利用者は、アプリの起動を待つことになります。外部の死活監視を当てていると休止しません。
        </p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
