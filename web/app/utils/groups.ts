// Groups and links (ARCHITECTURE 6.1): what the console says about them,
// the choice of a group and of the exposed services on the settings tab,
// and the links an app makes to the exposed services of other members.

import { ENVIRONMENT_LABELS } from './status';
import type { App, AppLink, Environment, Group, GroupMember } from './types';

/**
 * One DNS label: what an exposed service, a consumer service and the name
 * of a link must look like.
 */
export const DNS_LABEL = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

/**
 * Words in a service name (or image, when one is known) that suggest a
 * database. The API reports no image, so the name is all there is to go
 * on: a hint for a warning, never a refusal.
 */
const DATABASE_WORDS =
  /(^|[^a-z])(db|database|postgres(ql)?|pg|mysql|mariadb|redis|valkey|mongo(db)?)([^a-z]|$)/;

export function looksLikeDatabase(name: string): boolean {
  return DATABASE_WORDS.test(name.toLowerCase());
}

export function groupMembers(group: Group): GroupMember[] {
  return group.members ?? [];
}

export function memberServices(member: GroupMember): string[] {
  return member.services ?? [];
}

export function memberLinks(member: GroupMember): AppLink[] {
  return member.links ?? [];
}

export function isEmptyGroup(group: Group): boolean {
  return groupMembers(group).length === 0;
}

export function appGroupServices(app: App): string[] {
  return app.group_services ?? [];
}

export function appGroupId(app: App): number | null {
  return app.group_id ?? null;
}

/**
 * Why an app cannot join a group, or `null` when it can. Members share one
 * environment, so staging can never reach production.
 */
export function joinBlocker(
  group: Group,
  environment: Environment,
): string | null {
  if (group.environment === environment) return null;
  return `${ENVIRONMENT_LABELS[group.environment]}のグループです。${ENVIRONMENT_LABELS[environment]}のアプリは入れません。`;
}

/** An exposed service of another member: what a link may point at. */
export interface LinkTarget {
  app: string;
  service: string;
  /** `app/service`, the value of the choice. */
  key: string;
}

/** The exposed services of the other members of the group, by app. */
export function linkTargets(group: Group | null, slug: string): LinkTarget[] {
  if (!group) return [];
  return groupMembers(group)
    .filter((member) => member.slug !== slug)
    .flatMap((member) =>
      [...memberServices(member)].sort().map((service) => ({
        app: member.slug,
        service,
        key: `${member.slug}/${service}`,
      })),
    );
}

/** `shop の auth → accounts / auth`: a link with its consumer. */
export function linkLabel(
  owner: string,
  link: Pick<AppLink, 'name' | 'target_app' | 'target_service'>,
): string {
  return `${owner} の ${link.name} → ${link.target_app} / ${link.target_service}`;
}

/** `accounts / auth`: where a link points. */
export function linkTargetLabel(
  link: Pick<AppLink, 'target_app' | 'target_service'>,
): string {
  return `${link.target_app} / ${link.target_service}`;
}

/** A link that a change of group or of exposed services deletes. */
export interface RemovedLink {
  /** The consumer app. */
  owner: string;
  id: number;
  label: string;
}

/**
 * The links that go away when the app `slug`, now a member of `group`,
 * moves to `after`: leaving (or changing groups) deletes its own links and
 * every link to it; un-exposing a service deletes the links to that
 * service. The API deletes them at once (docs/API.md), so the console
 * names them before saving.
 */
export function removedLinks(
  group: Group | null,
  slug: string,
  after: { groupId: number | null; services: string[] },
): RemovedLink[] {
  if (!group) return [];
  const leaving = after.groupId !== group.id;
  const removed: RemovedLink[] = [];
  for (const member of groupMembers(group)) {
    for (const link of memberLinks(member)) {
      const own = member.slug === slug;
      const toApp = link.target_app === slug;
      const goes =
        (leaving && (own || toApp)) ||
        (!leaving && toApp && !after.services.includes(link.target_service));
      if (goes) {
        removed.push({
          owner: member.slug,
          id: link.id,
          label: linkLabel(member.slug, link),
        });
      }
    }
  }
  return removed.sort((a, b) => a.label.localeCompare(b.label));
}

/**
 * Why a link name cannot be used, or `null`. The API checks the same
 * (`link_name_taken`, `details.name`); the console says it before sending.
 */
export function linkNameProblem(
  name: string,
  ownServices: string[],
  takenNames: string[],
): string | null {
  if (name === '') return '名前を入力してください。';
  if (!DNS_LABEL.test(name)) {
    return '英小文字・数字・ハイフンで入力してください（先頭と末尾は英小文字か数字）。';
  }
  if (name.length > 63) return '63 文字以内で入力してください。';
  if (ownServices.includes(name)) {
    return 'このアプリのサービスと同じ名前は使えません。そのサービスが止まっている間に、名前が相手に解決されてしまうためです。';
  }
  if (takenNames.includes(name)) {
    return 'このアプリには、同じ名前の接続がすでにあります。';
  }
  return null;
}
