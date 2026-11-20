-- Rollbacks to a kept release (ARCHITECTURE 7.1) are deployments that
-- point at the release they start again. On success the app's current
-- deployment becomes that release, since the rollback has no files of its
-- own. NULL: an ordinary deployment.
ALTER TABLE deployments ADD COLUMN source_deployment_id INTEGER REFERENCES deployments (id) ON DELETE SET NULL;
