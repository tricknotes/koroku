-- One-off commands and registered tasks (ARCHITECTURE 9.6).

-- Named commands an app keeps for one-click runs, in display order.
CREATE TABLE app_tasks (
    app_id   INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    position INTEGER NOT NULL,
    name     TEXT    NOT NULL,
    service  TEXT    NOT NULL,
    command  TEXT    NOT NULL,
    PRIMARY KEY (app_id, position),
    UNIQUE (app_id, name)
);

-- Every run of a command, registered or typed. task_name is empty for an
-- ad-hoc command; the service and the command are kept as they were run.
-- Runs never wait in a queue: they start at once or are refused.
CREATE TABLE task_runs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id        INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    task_name     TEXT    NOT NULL DEFAULT '',
    service       TEXT    NOT NULL,
    command       TEXT    NOT NULL,
    deployment_id INTEGER REFERENCES deployments (id) ON DELETE SET NULL,
    status        TEXT    NOT NULL DEFAULT 'running'
                  CHECK (status IN ('running', 'succeeded', 'failed', 'canceled')),
    exit_code     INTEGER,
    error         TEXT    NOT NULL DEFAULT '',
    started_at    INTEGER NOT NULL,
    finished_at   INTEGER
);

CREATE INDEX task_runs_app ON task_runs (app_id, id DESC);
