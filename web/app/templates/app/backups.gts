import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { service } from '@ember/service';
import { on } from '@ember/modifier';
import { eq, fn } from '@ember/helper';
import type Owner from '@ember/owner';
import AsyncView from 'paas-console/components/async-view';
import Badge from 'paas-console/components/badge';
import ConfirmDialog from 'paas-console/components/confirm-dialog';
import ErrorMessage from 'paas-console/components/error-message';
import type ApiService from 'paas-console/services/api';
import type AppContext from 'paas-console/utils/app-context';
import { formatDateTime } from 'paas-console/utils/format';
import Loadable from 'paas-console/utils/loadable';
import type { Snapshot } from 'paas-console/utils/types';

const KIND_LABELS: Record<Snapshot['kind'], string> = {
  volume: 'volume',
  dump: 'DB ダンプ',
};

function kindLabel(snapshot: Snapshot): string {
  return KIND_LABELS[snapshot.kind] ?? snapshot.kind;
}

interface Signature {
  Args: { model: AppContext };
}

export default class AppBackupsTemplate extends Component<Signature> {
  @service declare api: ApiService;

  snapshots = new Loadable<Snapshot[]>(() => this.api.listSnapshots(this.slug));

  @tracked restoring: Snapshot | null = null;
  @tracked busy = false;
  @tracked restored = false;
  @tracked error: unknown = null;

  constructor(owner: Owner, args: Signature['Args']) {
    super(owner, args);
    void this.snapshots.load();
  }

  get slug(): string {
    return this.args.model.slug;
  }

  get restoreExample(): string {
    return `docker compose -p paas-${this.slug} exec -T db psql -U postgres app < dump.sql`;
  }

  downloadUrl = (snapshot: Snapshot): string =>
    this.api.snapshotDownloadUrl(this.slug, snapshot.id);

  requestRestore = (snapshot: Snapshot) => {
    this.restoring = snapshot;
    this.restored = false;
    this.error = null;
  };

  cancelRestore = () => {
    this.restoring = null;
  };

  confirmRestore = async () => {
    const snapshot = this.restoring;
    if (!snapshot) return;
    this.busy = true;
    try {
      await this.api.restoreSnapshot(this.slug, snapshot.id);
      this.restored = true;
    } catch (error) {
      this.error = error;
    } finally {
      this.busy = false;
      this.restoring = null;
    }
    await this.args.model.reload();
  };

  <template>
    <section class="card" aria-labelledby="backups-title">
      <h3 id="backups-title">バックアップ</h3>
      <p
        class="muted"
      >このアプリのスナップショットです。取得の状況と手動実行は「システム」にあります。volume
        と DB ダンプの対象は「設定」で変えられます。</p>

      <div aria-live="polite">
        {{#if this.restored}}
          <p
            class="notice notice-ok"
            data-test-restored
          >リストアが完了し、アプリを起動しました。</p>
        {{/if}}
        <ErrorMessage @error={{this.error}} />
      </div>

      <AsyncView @data={{this.snapshots}} as |snapshots|>
        {{#if snapshots.length}}
          <div class="table-wrap">
            <table class="table">
              <thead>
                <tr>
                  <th scope="col">取得日時</th>
                  <th scope="col">種類</th>
                  <th scope="col">対象</th>
                  <th scope="col">ID</th>
                  <th scope="col"><span class="visually-hidden">操作</span></th>
                </tr>
              </thead>
              <tbody>
                {{#each snapshots key="id" as |snapshot|}}
                  <tr data-test-snapshot={{snapshot.id}}>
                    <td>{{formatDateTime snapshot.time}}</td>
                    <td><Badge>{{kindLabel snapshot}}</Badge></td>
                    <td>{{snapshot.target}}</td>
                    <td><code>{{snapshot.id}}</code></td>
                    <td class="table-actions">
                      {{#if (eq snapshot.kind "volume")}}
                        <button
                          type="button"
                          class="button button-small button-danger"
                          disabled={{this.busy}}
                          data-test-snapshot-restore
                          {{on "click" (fn this.requestRestore snapshot)}}
                        >リストア</button>
                      {{else}}
                        <a
                          class="button button-small"
                          href={{this.downloadUrl snapshot}}
                          download
                          data-test-snapshot-download
                        >ダウンロード</a>
                      {{/if}}
                    </td>
                  </tr>
                {{/each}}
              </tbody>
            </table>
          </div>
        {{else}}
          <p
            class="muted"
            data-test-no-snapshots
          >スナップショットはまだありません。</p>
        {{/if}}

        <details class="help">
          <summary>DB ダンプを戻すには</summary>
          <p
          >ダンプの流し込みは手動です。ダウンロードしたファイルをサーバーに置き、DB
            のサービスへ標準入力で渡します。PostgreSQL の例:</p>
          <pre class="code">{{this.restoreExample}}</pre>
          <p
            class="muted"
          >サービス名、ユーザー名、データベース名はアプリに合わせて読み替えてください。</p>
        </details>
      </AsyncView>
    </section>

    {{#if this.restoring}}
      <ConfirmDialog
        @title="volume をリストアしますか？"
        @confirmLabel="リストアする"
        @danger={{true}}
        @busy={{this.busy}}
        @onConfirm={{this.confirmRestore}}
        @onCancel={{this.cancelRestore}}
      >
        <p>volume
          <strong>{{this.restoring.target}}</strong>
          を
          {{formatDateTime this.restoring.time}}
          のスナップショット（<code
          >{{this.restoring.id}}</code>）に戻します。</p>
        <ul>
          <li>リストアの間、アプリ
            <strong>{{this.slug}}</strong>
            は<strong>停止</strong>します。</li>
          <li>現在の volume の中身は消さずにサーバー上へ<strong
            >退避</strong>し、7 日間残します。</li>
          <li>終わるとアプリを起動します。完了までこの画面で待ちます。</li>
        </ul>
      </ConfirmDialog>
    {{/if}}
  </template>
}
