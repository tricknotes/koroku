import type { TOC } from '@ember/component/template-only';
import { LinkTo } from '@ember/routing';
import {
  hasBackupFailure,
  type AttentionItem,
} from 'koroku-console/utils/attention';
import { formatPercent } from 'koroku-console/utils/format';
import Banner from './banner';
import RelativeTime from './relative-time';

/** One line of the banner: what is wrong and where to deal with it. */
interface Line {
  key: string;
  kind: AttentionItem['kind'];
  /** The app the line is about; empty for the host. */
  subject: string;
  text: string;
  /** A point in time added as a sentence of its own ("最終成功: 2 日前"). */
  time?: string | null;
  route: string;
  models: string[];
  /** Query parameters of the link: the section of a tab (ARCHITECTURE 9.1). */
  query?: Record<string, string>;
  link: string;
}

function queryOf(line: Line): Record<string, string> {
  return line.query ?? {};
}

function line(item: AttentionItem): Line {
  switch (item.kind) {
    case 'disk':
      return {
        key: 'disk',
        kind: item.kind,
        subject: '',
        text: `ディスク使用率が ${formatPercent(item.usedPercent)} です（上限 ${formatPercent(item.limitPercent)}）。下回るまでデプロイを受け付けません。`,
        route: 'system',
        models: [],
        link: 'システムを確認',
      };
    case 'backup':
      return {
        key: 'backup',
        kind: item.kind,
        subject: '',
        text: 'バックアップが 24 時間以上成功していません。',
        time: item.lastSuccessAt,
        route: 'system',
        models: [],
        link: 'システムを確認',
      };
    case 'backup-run':
      return {
        key: 'backup-run',
        kind: item.kind,
        subject: '',
        text: '直近のバックアップの実行が、取得の前に失敗しました。どのアプリも取得できていません。',
        route: 'system',
        models: [],
        link: 'システムを確認',
      };
    case 'backup-system':
      return {
        key: 'backup-system',
        kind: item.kind,
        subject: '',
        text: '基盤（koroku の DB と設定）の直近のバックアップが失敗しました。',
        route: 'system',
        models: [],
        link: 'システムを確認',
      };
    case 'backup-app':
      return {
        key: `backup-app:${item.slug}`,
        kind: item.kind,
        subject: item.slug,
        text: 'の直近のバックアップが失敗しました。',
        route: 'app.backups',
        models: [item.slug],
        link: 'バックアップを確認',
      };
    case 'deployment':
      return {
        key: `deployment:${item.slug}`,
        kind: item.kind,
        subject: item.slug,
        text: 'の直近のデプロイが失敗したままです。',
        route: 'app.deploy',
        models: [item.slug],
        link: 'デプロイを確認',
      };
    case 'domains':
      return {
        key: `domains:${item.slug}`,
        kind: item.kind,
        subject: item.slug,
        text: `に確認待ちのドメインが ${item.count} 件あります。`,
        route: 'app.settings',
        models: [item.slug],
        query: { section: 'domains' },
        link: 'ドメインを確認',
      };
  }
}

/**
 * The host's lines point to the system screen, which only the owner has
 * (ARCHITECTURE 10.7); members get the text without the link.
 */
function lines(items: AttentionItem[], owner: boolean | undefined): Line[] {
  return items.map((item) => {
    const l = line(item);
    return owner === false && l.route === 'system' ? { ...l, route: '' } : l;
  });
}

/**
 * The warning shown above the app list while something needs the owner's
 * attention (an error when a backup failed), one line per item with a link to where it is dealt with.
 * Render it only when there are items: a healthy host shows no banner.
 */
const AttentionBanner: TOC<{
  Args: {
    items: AttentionItem[];
    /** `false` for a member: no links to the owner's system screen. */
    owner?: boolean;
  };
}> = <template>
  <Banner
    @tone={{if (hasBackupFailure @items) "error" "warning"}}
    @title="対応が必要な項目が {{@items.length}} 件あります"
    data-test-attention
  >
    <ul class="banner-list">
      {{#each (lines @items @owner) key="key" as |item|}}
        <li data-test-attention-item={{item.key}}>
          {{#if item.subject}}<strong>{{item.subject}}</strong>
          {{/if}}{{item.text}}
          {{#if item.time}}
            <span>最終成功:
              <RelativeTime @time={{item.time}} /></span>
          {{/if}}
          {{#if item.route}}
            <LinkTo
              @route={{item.route}}
              @models={{item.models}}
              @query={{queryOf item}}
            >
              {{item.link}}
            </LinkTo>
          {{/if}}
        </li>
      {{/each}}
    </ul>
  </Banner>
</template>;

export default AttentionBanner;
