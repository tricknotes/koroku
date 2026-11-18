import type {
  App,
  BackupRun,
  Deployment,
  DeploymentStatus,
  Domain,
  Environment,
} from './types';

export type AppState =
  'deploying' | 'failed' | 'undeployed' | 'stopped' | 'running';

export type Tone = 'ok' | 'warn' | 'danger' | 'info' | 'muted';

type AppLike = Pick<
  App,
  'desired_state' | 'current_deployment' | 'latest_deployment'
>;

export function isActiveDeployment(
  deployment: Pick<Deployment, 'status'> | null | undefined,
): boolean {
  return deployment?.status === 'queued' || deployment?.status === 'running';
}

/**
 * The single state shown for an app. A deployment in progress wins, then a
 * failed latest deployment (the previous release may still be serving), then
 * whether it was ever deployed and whether it is meant to run.
 */
export function deriveAppState(app: AppLike): AppState {
  const latest = app.latest_deployment;
  if (isActiveDeployment(latest)) return 'deploying';
  if (latest?.status === 'failed') return 'failed';
  if (!app.current_deployment) return 'undeployed';
  if (app.desired_state === 'stopped') return 'stopped';
  return 'running';
}

export const APP_STATE_LABELS: Record<AppState, string> = {
  deploying: 'デプロイ中',
  failed: '直近のデプロイに失敗',
  undeployed: '未デプロイ',
  stopped: '停止中',
  running: '稼働中',
};

export const APP_STATE_TONES: Record<AppState, Tone> = {
  deploying: 'info',
  failed: 'danger',
  undeployed: 'muted',
  stopped: 'warn',
  running: 'ok',
};

export const DEPLOYMENT_STATUS_LABELS: Record<DeploymentStatus, string> = {
  queued: '待機中',
  running: '実行中',
  succeeded: '成功',
  failed: '失敗',
  canceled: 'キャンセル',
};

export const DEPLOYMENT_STATUS_TONES: Record<DeploymentStatus, Tone> = {
  queued: 'info',
  running: 'info',
  succeeded: 'ok',
  failed: 'danger',
  canceled: 'muted',
};

export const ENVIRONMENT_LABELS: Record<Environment, string> = {
  production: '本番',
  staging: 'ステージング',
};

export const DOMAIN_STATUS_LABELS: Record<Domain['status'], string> = {
  pending: '確認待ち',
  active: '有効',
};

export const DOMAIN_STATUS_TONES: Record<Domain['status'], Tone> = {
  pending: 'warn',
  active: 'ok',
};

export const BACKUP_TRIGGER_LABELS: Record<BackupRun['trigger'], string> = {
  scheduled: '定期',
  manual: '手動',
  pre_deploy: 'デプロイ前',
};

export const BACKUP_STATUS_LABELS: Record<BackupRun['status'], string> = {
  running: '実行中',
  succeeded: '成功',
  failed: '失敗',
};

export const BACKUP_STATUS_TONES: Record<BackupRun['status'], Tone> = {
  running: 'info',
  succeeded: 'ok',
  failed: 'danger',
};

/** Tone of one Compose service row from `docker compose ps` style fields. */
export function serviceTone(state: string, health: string): Tone {
  if (health === 'unhealthy') return 'danger';
  if (state === 'running') return health === 'starting' ? 'info' : 'ok';
  if (state === 'restarting' || state === 'created') return 'info';
  return 'warn';
}
