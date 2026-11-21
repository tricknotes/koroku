-- Automatic deployment that waits for a GitHub Actions workflow
-- (ARCHITECTURE 5.2). auto_deploy_workflow: the workflow file path, for
-- example .github/workflows/image.yml, whose successful run on the tracked
-- branch deploys the app instead of the push. '' deploys on push.
ALTER TABLE apps ADD COLUMN auto_deploy_workflow TEXT NOT NULL DEFAULT '';
