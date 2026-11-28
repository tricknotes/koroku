-- Restores of a PostgreSQL database to a point in time (ARCHITECTURE 10.9).
CREATE TABLE pitr_restores (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id           INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    service          TEXT    NOT NULL,
    target_time      INTEGER NOT NULL,
    base_snapshot_id TEXT    NOT NULL DEFAULT '',
    -- Where the data before the restore was moved; kept for 7 days.
    trash_path       TEXT    NOT NULL DEFAULT '',
    status           TEXT    NOT NULL DEFAULT 'running'
                     CHECK (status IN ('running', 'succeeded', 'failed')),
    error            TEXT    NOT NULL DEFAULT '',
    actor_user_id    INTEGER,
    actor_login      TEXT    NOT NULL DEFAULT '',
    started_at       INTEGER NOT NULL,
    finished_at      INTEGER
);
CREATE INDEX pitr_restores_app ON pitr_restores (app_id, id DESC);
