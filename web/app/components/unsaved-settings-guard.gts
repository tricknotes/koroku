import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import type Owner from '@ember/owner';
import type RouterService from '@ember/routing/router-service';
import type Transition from '@ember/routing/transition';
import ConfirmDialog from 'koroku-console/components/confirm-dialog';
import type SettingsEditor from 'koroku-console/utils/settings-editor';

const DEPLOY_ROUTE = 'app.deploy';

interface Signature {
  Args: {
    editor: SettingsEditor;
    /** What has unsaved changes, as the dialog names it ("設定"). */
    what: string;
  };
}

/**
 * Going to the deploy tab (its tab or the header's button) with unsaved
 * settings would deploy the saved ones and drop the edits: asks first
 * (ARCHITECTURE 9.1). Render it once on a tab that edits settings.
 */
export default class UnsavedSettingsGuard extends Component<Signature> {
  @service declare router: RouterService;

  /** The move held back because of unsaved changes. */
  @tracked pending: Transition | null = null;
  private approved = false;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    this.router.on('routeWillChange', this.guard);
  }

  willDestroy(): void {
    super.willDestroy();
    this.router.off('routeWillChange', this.guard);
  }

  guard = (transition: Transition) => {
    if (this.approved || transition.isAborted) return;
    if (transition.to?.name !== DEPLOY_ROUTE) return;
    if (!this.args.editor.dirty) return;
    transition.abort();
    this.pending = transition;
  };

  saveAndLeave = async () => {
    const transition = this.pending;
    if (!transition) return;
    this.pending = null;
    // A production change asks once more; the move waits for that answer.
    if (await this.args.editor.save()) this.leave(transition);
  };

  leaveWithoutSaving = () => {
    const transition = this.pending;
    this.pending = null;
    if (transition) this.leave(transition);
  };

  cancel = () => {
    this.pending = null;
  };

  private leave(transition: Transition): void {
    this.approved = true;
    transition.retry();
  }

  <template>
    {{#if this.pending}}
      <ConfirmDialog
        @title="保存していない変更があります"
        @confirmLabel="保存して進む"
        @altLabel="保存せずに進む"
        @busy={{@editor.saving}}
        @onConfirm={{this.saveAndLeave}}
        @onAlt={{this.leaveWithoutSaving}}
        @onCancel={{this.cancel}}
      >
        <p data-test-unsaved-dialog>
          {{@what}}に保存していない変更があります。デプロイは保存済みの設定で行われます。保存してからデプロイの画面に進みますか？保存せずに進むと、変更は破棄されます。
        </p>
      </ConfirmDialog>
    {{/if}}
  </template>
}
