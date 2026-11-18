import Component from '@glimmer/component';
import { service } from '@ember/service';
import ConfirmDialog from './confirm-dialog';
import type SessionService from 'koroku-console/services/session';

/**
 * Shown when the API refuses a dangerous operation because the login is
 * older than ten minutes. Logging in again brings the user back here.
 */
export default class ReauthDialog extends Component {
  @service declare session: SessionService;

  confirm = () => this.session.reauthenticate();
  cancel = () => this.session.dismissReauth();

  <template>
    {{#if this.session.reauthPrompt}}
      <ConfirmDialog
        @title="もう一度ログインしてください"
        @confirmLabel="GitHub で再ログイン"
        @onConfirm={{this.confirm}}
        @onCancel={{this.cancel}}
      >
        <p data-test-reauth-dialog>
          この操作には、直近 10 分以内のログインが必要です。セッションの Cookie
          だけが盗まれた場合に、削除や秘密の値の読み出しをされないようにするためです。
        </p>
        <p
        >再ログインすると、この画面に戻ります。操作はもう一度行ってください。</p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
