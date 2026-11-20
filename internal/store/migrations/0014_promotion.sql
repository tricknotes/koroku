-- Promotion of a staging release to production (ARCHITECTURE 9.7).
-- promote_from_app_id: the staging app a production app promotes from.
-- Deleting the staging app removes the pairing.
ALTER TABLE apps ADD COLUMN promote_from_app_id INTEGER
    REFERENCES apps (id) ON DELETE SET NULL;
-- promoted_from_deployment_id: the staging deployment a promotion deployed.
-- NULL for ordinary deployments, and once the source app is deleted.
ALTER TABLE deployments ADD COLUMN promoted_from_deployment_id INTEGER
    REFERENCES deployments (id) ON DELETE SET NULL;
