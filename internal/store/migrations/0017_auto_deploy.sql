-- Automatic deployment on GitHub pushes (ARCHITECTURE 5.2).
-- auto_deploy: deploy the head of auto_deploy_branch when it is pushed.
-- auto_deploy_branch: '' follows the app's default branch.
ALTER TABLE apps ADD COLUMN auto_deploy INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN auto_deploy_branch TEXT NOT NULL DEFAULT '';
-- trigger_kind: what started a deployment, 'manual' (the console or the
-- API) or 'push'. pusher: the GitHub login that pushed, for 'push'.
ALTER TABLE deployments ADD COLUMN trigger_kind TEXT NOT NULL DEFAULT 'manual';
ALTER TABLE deployments ADD COLUMN pusher TEXT NOT NULL DEFAULT '';
