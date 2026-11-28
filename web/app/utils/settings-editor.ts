import { tracked } from '@glimmer/tracking';
import type ApiService from 'koroku-console/services/api';
import type AppContext from 'koroku-console/utils/app-context';
import { fieldErrors } from 'koroku-console/utils/errors';
import SettingsForm from 'koroku-console/utils/settings-form';
import type { App, AppPatch } from 'koroku-console/utils/types';

/** A change a production app asks about before it is saved. */
export type SettingsConfirmation =
  'protection' | 'auto-deploy' | 'idle-sleep' | 'blue-green';

/** The fields each tab edits (ARCHITECTURE 9.1). */
export const SETTINGS_TAB_FIELDS: (keyof AppPatch)[] = [
  'default_branch',
  'compose_path',
  'compose_source',
  'compose_content',
  'web_service',
  'web_port',
  'healthcheck_path',
  'environment',
  'access_protection',
  'protection_exempt_paths',
];

export const DEPLOY_SETTINGS_FIELDS: (keyof AppPatch)[] = [
  'auto_deploy',
  'auto_deploy_branch',
  'auto_deploy_workflow',
  'promote_from',
  'relax_policy',
  'blue_green',
  'blue_green_drain_seconds',
];

export const IDLE_SLEEP_FIELDS: (keyof AppPatch)[] = [
  'idle_sleep',
  'idle_sleep_minutes',
];

export const ENV_SOURCE_FIELDS: (keyof AppPatch)[] = [
  'env_source',
  'env_example_path',
];

/**
 * The edit state of one tab's settings: a `SettingsForm` of the app, saved
 * with only the fields of that tab (ARCHITECTURE 9.1). Every tab goes
 * through the same checks and the same confirmations for production apps,
 * so moving a setting between tabs does not change how it is saved.
 */
export default class SettingsEditor {
  @tracked form: SettingsForm;
  @tracked saving = false;
  @tracked saved = false;
  @tracked nothingToSave = false;
  @tracked error: unknown = null;
  @tracked errors: Record<string, string> = {};

  /** The save waiting on screen for one or more confirmations. */
  @tracked private pending: {
    patch: AppPatch;
    ask: SettingsConfirmation[];
  } | null = null;
  private settle: ((saved: boolean) => void) | null = null;

  constructor(
    private api: ApiService,
    private context: AppContext,
    readonly fields: readonly (keyof AppPatch)[],
    /** Called with what was saved, once the server has it. */
    private onSaved?: (patch: AppPatch, app: App) => void,
  ) {
    this.form = new SettingsForm(context.app);
  }

  get app(): App {
    return this.context.app;
  }

  /** What saving would send: the changed fields of this tab only. */
  get patch(): AppPatch {
    const all = this.form.patch(this.app);
    const patch: AppPatch = {};
    for (const field of this.fields) {
      if (field in all) {
        (patch as Record<string, unknown>)[field] = all[field];
      }
    }
    return patch;
  }

  get dirty(): boolean {
    return Object.keys(this.patch).length > 0;
  }

  /** The confirmation on screen, if any. */
  get confirming(): SettingsConfirmation | null {
    return this.pending?.ask[0] ?? null;
  }

  /** Errors the server reported for fields this tab does not show. */
  otherErrors(known: Set<string>): string[] {
    return Object.entries(this.errors)
      .filter(([name]) => !known.has(name))
      .map(([name, reason]) => `${name}: ${reason}`);
  }

  touch = () => {
    this.saved = false;
    this.nothingToSave = false;
  };

  /** Starts again from the app as it is saved. */
  reset = () => {
    this.form = new SettingsForm(this.app);
    this.errors = {};
    this.error = null;
    this.saved = false;
    this.nothingToSave = false;
  };

  /**
   * Checks and saves the tab's changes, asking first where a production app
   * needs it. Resolves true once they are saved (or there was nothing to
   * save), false when a check failed, the save failed or it was cancelled.
   */
  save = async (): Promise<boolean> => {
    this.saved = false;
    this.nothingToSave = false;
    this.error = null;
    const errors = this.form.validate();
    this.errors = Object.fromEntries(
      Object.entries(errors).filter(([name]) =>
        this.fields.includes(name as keyof AppPatch),
      ),
    );
    if (Object.keys(this.errors).length > 0) return false;

    const patch = this.patch;
    if (Object.keys(patch).length === 0) {
      this.nothingToSave = true;
      return true;
    }
    const ask = this.confirmations(patch);
    if (ask.length === 0) return this.commit(patch);
    this.settle?.(false);
    this.pending = { patch, ask };
    return new Promise((resolve) => {
      this.settle = resolve;
    });
  };

  /** The confirmation on screen was accepted: ask the next, or save. */
  confirm = async () => {
    const pending = this.pending;
    if (!pending) return;
    if (pending.ask.length > 1) {
      this.pending = { patch: pending.patch, ask: pending.ask.slice(1) };
      return;
    }
    const saved = await this.commit(pending.patch);
    this.pending = null;
    this.finish(saved);
  };

  cancel = () => {
    this.pending = null;
    this.finish(false);
  };

  private finish(saved: boolean): void {
    const settle = this.settle;
    this.settle = null;
    settle?.(saved);
  }

  /** Whether the app is or becomes a production app with this save. */
  private get savesProduction(): boolean {
    return (
      this.app.environment === 'production' ||
      this.form.environment === 'production'
    );
  }

  private get isProductionForm(): boolean {
    return this.form.environment === 'production';
  }

  /** What a production app asks about before this patch is saved. */
  confirmations(patch: AppPatch): SettingsConfirmation[] {
    const ask: SettingsConfirmation[] = [];
    // Closing a production app locks its users out; opening it publishes it.
    if (patch.access_protection !== undefined && this.savesProduction) {
      ask.push('protection');
    }
    if (this.startsProductionAutoDeploy(patch)) ask.push('auto-deploy');
    if (this.startsProductionIdleSleep(patch)) ask.push('idle-sleep');
    if (this.startsProductionBlueGreen(patch)) ask.push('blue-green');
    return ask;
  }

  /**
   * Whether a save makes a production app deploy pushes, or pushes of
   * another branch, or after another workflow (or none), without anyone
   * pressing a button (ARCHITECTURE 5.2).
   */
  private startsProductionAutoDeploy(patch: AppPatch): boolean {
    if (!this.isProductionForm || !this.form.autoDeploy) return false;
    return (
      patch.auto_deploy === true ||
      patch.environment === 'production' ||
      patch.auto_deploy_branch !== undefined ||
      patch.auto_deploy_workflow !== undefined ||
      patch.default_branch !== undefined
    );
  }

  /**
   * Whether a save lets a production app sleep while idle: its first
   * visitor after a sleep waits for it to start (ARCHITECTURE 8.4).
   */
  private startsProductionIdleSleep(patch: AppPatch): boolean {
    if (!this.isProductionForm || !this.form.idleSleep) return false;
    return patch.idle_sleep === true || patch.environment === 'production';
  }

  /**
   * Whether a save turns on blue/green deploys of a production app: its
   * migrations must then work with the old release's code (ARCHITECTURE
   * 7.2). Turning them off asks nothing.
   */
  private startsProductionBlueGreen(patch: AppPatch): boolean {
    if (!this.isProductionForm || !this.form.blueGreen) return false;
    return patch.blue_green === true || patch.environment === 'production';
  }

  private async commit(patch: AppPatch): Promise<boolean> {
    this.saving = true;
    try {
      const app = await this.api.updateApp(this.app.slug, patch);
      this.context.set(app);
      this.form = new SettingsForm(app);
      this.saved = true;
      this.onSaved?.(patch, app);
      return true;
    } catch (error) {
      this.error = error;
      this.errors = fieldErrors(error);
      return false;
    } finally {
      this.saving = false;
    }
  }
}
