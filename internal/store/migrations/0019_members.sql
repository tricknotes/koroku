-- Members and their per-app roles (ARCHITECTURE 10.7). The owner is the
-- [github] allowed_user_id of config.toml and has no row here. GitHub user
-- IDs are what count; login and avatar_url are for display and are
-- refreshed when the member logs in.
CREATE TABLE members (
    github_user_id INTEGER PRIMARY KEY,
    login          TEXT    NOT NULL,
    avatar_url     TEXT    NOT NULL DEFAULT '',
    added_at       INTEGER NOT NULL
);

-- One role per member per app. A member without any role cannot log in.
CREATE TABLE app_roles (
    app_id         INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    github_user_id INTEGER NOT NULL REFERENCES members (github_user_id) ON DELETE CASCADE,
    role           TEXT    NOT NULL CHECK (role IN ('view', 'deploy', 'manage')),
    PRIMARY KEY (app_id, github_user_id)
);

CREATE INDEX app_roles_user ON app_roles (github_user_id);

-- API tokens belong to a user and act with that user's permissions. Tokens
-- issued before members existed have 0 here; koroku serve assigns them to
-- the owner at startup.
ALTER TABLE api_tokens ADD COLUMN github_user_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE api_tokens ADD COLUMN login TEXT NOT NULL DEFAULT '';
CREATE INDEX api_tokens_user ON api_tokens (github_user_id);

-- Who started an operation. NULL when nobody did (older rows, or koroku
-- itself); the login is kept as it was at the time.
ALTER TABLE deployments ADD COLUMN actor_user_id INTEGER;
ALTER TABLE deployments ADD COLUMN actor_login TEXT NOT NULL DEFAULT '';
ALTER TABLE task_runs ADD COLUMN actor_user_id INTEGER;
ALTER TABLE task_runs ADD COLUMN actor_login TEXT NOT NULL DEFAULT '';
ALTER TABLE db_copies ADD COLUMN actor_user_id INTEGER;
ALTER TABLE db_copies ADD COLUMN actor_login TEXT NOT NULL DEFAULT '';

CREATE INDEX login_events_user ON login_events (github_user_id);
CREATE INDEX sessions_user ON sessions (github_user_id);
