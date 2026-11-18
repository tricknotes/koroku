import Component from '@glimmer/component';
import {
  diskLimitPercent,
  diskOverLimit,
} from 'koroku-console/utils/attention';
import { formatBytes, formatPercent } from 'koroku-console/utils/format';
import type Loadable from 'koroku-console/utils/loadable';
import type { BackupOverview, SystemInfo } from 'koroku-console/utils/types';
import Icon from './icon';
import RelativeTime from './relative-time';

type Level = 'ok' | 'warn' | 'danger' | 'unknown';

/** One figure of the line: a plain text, or a point in time. */
interface Item {
  name: string;
  label: string;
  level: Level;
  text?: string;
  time?: string;
  /** Tooltip with the detail the line has no room for. */
  hint?: string;
  /** Still loading for the first time. */
  pending?: boolean;
}

/** Shown for a figure the server did not provide. */
const UNKNOWN = { text: '—', hint: '取得できませんでした。' };

function isPending(data: Loadable<unknown>): boolean {
  return !data.loaded && !data.error;
}

function levelClass(level: Level): string {
  return level === 'ok' ? '' : `is-${level}`;
}

function needsIcon(level: Level): boolean {
  return level === 'warn' || level === 'danger';
}

interface HostStatusSignature {
  Element: HTMLDListElement;
  Args: {
    system: Loadable<SystemInfo>;
    backups: Loadable<BackupOverview>;
  };
}

/**
 * The health of the one host every app shares, in one line: disk, memory,
 * the last backup and the last log archive. A figure only takes on a colour
 * (with a warning icon) when it needs attention.
 */
export default class HostStatus extends Component<HostStatusSignature> {
  get items(): Item[] {
    return [this.disk, this.memory, this.backup, this.logArchive];
  }

  get disk(): Item {
    const item: Item = {
      name: 'disk',
      label: 'ディスク',
      level: 'unknown',
      pending: isPending(this.args.system),
    };
    const system = this.args.system.value;
    if (!system?.disk) return { ...item, ...UNKNOWN };
    const { disk } = system;
    const over = diskOverLimit(system);
    return {
      ...item,
      level: over ? 'danger' : 'ok',
      text: formatPercent(disk.used_percent),
      hint: `${formatBytes(disk.used_bytes)} / ${formatBytes(disk.total_bytes)}。${diskLimitPercent(system)}% を超えるとデプロイを受け付けません。`,
    };
  }

  get memory(): Item {
    const item: Item = {
      name: 'memory',
      label: 'メモリ',
      level: 'unknown',
      pending: isPending(this.args.system),
    };
    const memory = this.args.system.value?.memory;
    if (!memory) return { ...item, ...UNKNOWN };
    const used = Math.max(0, memory.total_bytes - memory.available_bytes);
    return {
      ...item,
      level: 'ok',
      text: `${formatBytes(used)} / ${formatBytes(memory.total_bytes)}`,
      hint: '使用中 / 搭載',
    };
  }

  get backup(): Item {
    const item: Item = {
      name: 'backup',
      label: '最終バックアップ',
      level: 'unknown',
      pending: isPending(this.args.backups),
    };
    const backups = this.args.backups.value;
    if (!backups) return { ...item, ...UNKNOWN };
    if (!backups.configured) return { ...item, text: '未設定' };
    const level = backups.warning ? 'warn' : 'ok';
    const hint = backups.warning
      ? '24 時間以上、バックアップが成功していません。'
      : undefined;
    if (!backups.last_success_at) {
      return { ...item, level, hint, text: 'まだありません' };
    }
    return { ...item, level, hint, time: backups.last_success_at };
  }

  get logArchive(): Item {
    const item: Item = {
      name: 'log-archive',
      label: 'ログ退避',
      level: 'unknown',
      pending: isPending(this.args.system),
    };
    const archive = this.args.system.value?.log_archive;
    if (!archive) return { ...item, ...UNKNOWN };
    if (!archive.configured) return { ...item, text: '未設定' };
    const failed = archive.last_error !== '';
    const level = failed ? 'warn' : 'ok';
    const hint = failed
      ? `直近のログ退避に失敗しました: ${archive.last_error}`
      : undefined;
    if (!archive.last_success_at) {
      return { ...item, level, hint, text: 'まだありません' };
    }
    return { ...item, level, hint, time: archive.last_success_at };
  }

  <template>
    <dl
      class="host-status"
      aria-label="ホストの状態"
      data-test-host-status
      ...attributes
    >
      {{#each this.items key="name" as |item|}}
        <div
          class="host-status-item {{levelClass item.level}}"
          title={{item.hint}}
          data-test-host={{item.name}}
          data-level={{item.level}}
        >
          <dt>{{item.label}}</dt>
          <dd>
            {{#if item.pending}}
              <span class="skeleton"></span>
              <span class="visually-hidden">読み込み中</span>
            {{else}}
              {{#if (needsIcon item.level)}}
                <Icon @name="warning" />
              {{/if}}
              {{#if item.time}}
                <RelativeTime @time={{item.time}} />
              {{else}}
                {{item.text}}
              {{/if}}
            {{/if}}
          </dd>
        </div>
      {{/each}}
    </dl>
  </template>
}
