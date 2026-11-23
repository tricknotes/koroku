import Component from '@glimmer/component';
import { hash } from '@ember/helper';
import { LinkTo } from '@ember/routing';
import RelativeTime from 'koroku-console/components/relative-time';
import { firstLine, shortSha } from 'koroku-console/utils/format';
import type { App, Deployment } from 'koroku-console/utils/types';

interface Signature {
  Element: HTMLElement;
  Args: { app: App };
}

/**
 * One line naming the release whose containers the numbers next to it
 * measure (ARCHITECTURE 9.5): its commit, branch, when it was deployed (or
 * rolled back to) and a link to that deployment.
 */
export default class CurrentReleaseLine extends Component<Signature> {
  get release(): Deployment | null {
    return this.args.app.current_deployment;
  }

  /**
   * The latest deployment, when it is a rollback to the running release:
   * the release has run since then, not since it was first deployed.
   */
  get rollback(): Deployment | null {
    const latest = this.args.app.latest_deployment;
    const release = this.release;
    if (
      latest &&
      release &&
      latest.status === 'succeeded' &&
      latest.source_deployment_id === release.id
    ) {
      return latest;
    }
    return null;
  }

  /** The deployment that started what runs now. */
  get started(): Deployment | null {
    return this.rollback ?? this.release;
  }

  get title(): string {
    if (this.args.app.desired_state === 'stopped') return '停止中のリリース';
    if (this.args.app.sleeping_since) return '休止中のリリース';
    return '稼働中のリリース';
  }

  get commitUrl(): string | null {
    const { repo_full_name } = this.args.app;
    const sha = this.release?.commit_sha;
    if (!repo_full_name || !sha) return null;
    return `https://github.com/${repo_full_name}/commit/${sha}`;
  }

  get startedAt(): string | null {
    const d = this.started;
    return d ? (d.finished_at ?? d.started_at ?? d.queued_at) : null;
  }

  <template>
    {{#if this.release}}
      <p class="current-release" data-test-current-release ...attributes>
        <span class="current-release-title">{{this.title}}</span>
        {{#if this.commitUrl}}
          <a
            class="current-release-sha"
            href={{this.commitUrl}}
            target="_blank"
            rel="noopener noreferrer"
            title={{this.release.commit_sha}}
            data-test-current-release-commit
          >{{shortSha this.release.commit_sha}}</a>
        {{else}}
          <code
            class="current-release-sha"
            title={{this.release.commit_sha}}
            data-test-current-release-commit
          >{{shortSha this.release.commit_sha}}</code>
        {{/if}}
        <span class="current-release-message">{{firstLine
            this.release.commit_message
          }}</span>
        {{#if this.release.ref}}
          <span class="current-release-meta">{{this.release.ref}}</span>
        {{/if}}
        <span class="current-release-meta" data-test-current-release-time>
          {{if this.rollback "ロールバック" "デプロイ"}}
          <RelativeTime @time={{this.startedAt}} />
        </span>
        {{#if this.started}}
          <LinkTo
            @route="app.deploy"
            @model={{@app.slug}}
            @query={{hash deployment=this.started.id}}
            class="current-release-link"
            data-test-current-release-link
          >デプロイ #{{this.started.id}}</LinkTo>
        {{/if}}
      </p>
    {{/if}}
  </template>
}
