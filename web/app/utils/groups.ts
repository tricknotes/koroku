// Groups (ARCHITECTURE 6.1): what the console says about them and the
// choice of a group and its services on the settings tab.

import { ENVIRONMENT_LABELS } from './status';
import type { App, Environment, Group, GroupMember } from './types';

/** What a service of a group must be named like: one DNS label. */
export const GROUP_SERVICE_NAME = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

/** The name other members reach a connected service by. */
export function groupHostname(service: string, slug: string): string {
  return `${service}.${slug}.koroku.internal`;
}

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

export function memberHostnames(member: GroupMember): string[] {
  return member.hostnames ?? [];
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

export interface GroupChangeEffect {
  /** Joins a group or connects more services: effective from the next up. */
  widens: boolean;
  /** Leaves or disconnects services: applied when saving. */
  narrows: boolean;
}

/** How a change of group or services changes what can reach what. */
export function groupChangeEffect(
  before: { groupId: number | null; services: string[] },
  after: { groupId: number | null; services: string[] },
): GroupChangeEffect {
  if (before.groupId !== after.groupId) {
    return {
      widens: after.groupId !== null,
      narrows: before.groupId !== null,
    };
  }
  if (after.groupId === null) return { widens: false, narrows: false };
  return {
    widens: after.services.some((s) => !before.services.includes(s)),
    narrows: before.services.some((s) => !after.services.includes(s)),
  };
}
