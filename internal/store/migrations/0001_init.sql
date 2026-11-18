-- Core tables (ARCHITECTURE chapter 9). Timestamps are Unix milliseconds in UTC.

CREATE TABLE apps (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    slug                  TEXT    NOT NULL UNIQUE,
    repo_id               INTEGER,
    repo_full_name        TEXT    NOT NULL DEFAULT '',
    -- Explicit clone URL. Empty means https://github.com/<repo_full_name>.git.
    repo_url              TEXT    NOT NULL DEFAULT '',
    default_branch        TEXT    NOT NULL DEFAULT 'main',
    -- Empty means auto-detect (compose.yaml, then docker-compose.yml).
    compose_path          TEXT    NOT NULL DEFAULT '',
    web_service           TEXT    NOT NULL DEFAULT 'web',
    web_port              INTEGER NOT NULL,
    host_port             INTEGER NOT NULL UNIQUE,
    healthcheck_path      TEXT    NOT NULL DEFAULT '/',
    desired_state         TEXT    NOT NULL DEFAULT 'running'
                          CHECK (desired_state IN ('running', 'stopped')),
    environment           TEXT    NOT NULL DEFAULT 'staging'
                          CHECK (environment IN ('production', 'staging')),
    relax_policy          INTEGER NOT NULL DEFAULT 0,
    -- Empty means the configured default. Applied to every service.
    memory_limit          TEXT    NOT NULL DEFAULT '',
    -- JSON object of per-service overrides, e.g. {"db": "1g"}.
    service_memory_limits TEXT    NOT NULL DEFAULT '{}',
    current_deployment_id INTEGER REFERENCES deployments (id) ON DELETE SET NULL,
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);

CREATE TABLE env_vars (
    app_id          INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    key             TEXT    NOT NULL,
    value_encrypted BLOB    NOT NULL,
    PRIMARY KEY (app_id, key)
);

CREATE TABLE deployments (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id         INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    ref            TEXT    NOT NULL DEFAULT '',
    commit_sha     TEXT    NOT NULL,
    commit_message TEXT    NOT NULL DEFAULT '',
    status         TEXT    NOT NULL DEFAULT 'queued'
                   CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'canceled')),
    error          TEXT    NOT NULL DEFAULT '',
    queued_at      INTEGER NOT NULL,
    started_at     INTEGER,
    finished_at    INTEGER
);

CREATE INDEX deployments_app_id_id ON deployments (app_id, id DESC);
CREATE INDEX deployments_status ON deployments (status);

CREATE TABLE sessions (
    token_hash     TEXT    PRIMARY KEY,
    github_user_id INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL
);
