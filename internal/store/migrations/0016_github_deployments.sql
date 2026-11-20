-- The GitHub deployment that mirrors a koroku deployment (ARCHITECTURE 5.1).
-- github_id is GitHub's deployment ID; state is the last status GitHub
-- accepted for it ('' until the first one), so that a final status lost to
-- a restart or a GitHub outage can be sent again.
CREATE TABLE github_deployments (
    deployment_id INTEGER PRIMARY KEY REFERENCES deployments (id) ON DELETE CASCADE,
    github_id     INTEGER NOT NULL,
    state         TEXT    NOT NULL DEFAULT '',
    updated_at    INTEGER NOT NULL
);
