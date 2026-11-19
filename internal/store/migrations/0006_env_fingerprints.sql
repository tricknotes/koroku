-- Keyed fingerprints of the environment variables a deployment rendered,
-- as a JSON object from key to fingerprint (ARCHITECTURE 9.2). The console
-- compares the running release's fingerprints with the saved variables to
-- show which changes are not deployed yet. NULL: not recorded (deployments
-- made before this column, or ones that stopped before rendering the env).
ALTER TABLE deployments ADD COLUMN env_fingerprints TEXT;
