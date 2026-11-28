-- Point-in-time recovery of a PostgreSQL service (ARCHITECTURE 10.9).

-- The PostgreSQL service whose WAL is archived; '' when disabled.
ALTER TABLE apps ADD COLUMN pitr_service TEXT NOT NULL DEFAULT '';

-- Where the preparation of an app stands: koroku sets the archiving up
-- after a deployment that mounts the archive directory.
CREATE TABLE pitr_states (
    app_id             INTEGER PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    service            TEXT    NOT NULL,
    status             TEXT    NOT NULL CHECK (status IN ('preparing', 'ready', 'failed')),
    error              TEXT    NOT NULL DEFAULT '',
    data_directory     TEXT    NOT NULL DEFAULT '',
    server_version_num INTEGER NOT NULL DEFAULT 0,
    -- pg_stat_archiver.failed_count when the WAL was last shipped; a
    -- higher count since is a warning.
    archiver_failed    INTEGER NOT NULL DEFAULT 0,
    warning            TEXT    NOT NULL DEFAULT '',
    updated_at         INTEGER NOT NULL
);

-- Base backups: where recovery starts.
CREATE TABLE pitr_bases (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id             INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    service            TEXT    NOT NULL,
    snapshot_id        TEXT    NOT NULL,
    started_at         INTEGER NOT NULL,
    finished_at        INTEGER NOT NULL,
    -- The WAL segment in use when the backup started: recovery needs it
    -- and every later one.
    start_segment      TEXT    NOT NULL,
    timeline           INTEGER NOT NULL,
    server_version_num INTEGER NOT NULL
);
CREATE INDEX pitr_bases_app ON pitr_bases (app_id, finished_at);

-- WAL shipped to the repository, one snapshot per batch.
CREATE TABLE pitr_wal_batches (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id           INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    service          TEXT    NOT NULL,
    snapshot_id      TEXT    NOT NULL,
    first_segment    TEXT    NOT NULL,
    last_segment     TEXT    NOT NULL,
    file_count       INTEGER NOT NULL,
    -- The modification time of the newest file: archived up to then.
    last_modified_at INTEGER NOT NULL,
    created_at       INTEGER NOT NULL
);
CREATE INDEX pitr_wal_batches_app ON pitr_wal_batches (app_id, id);

-- Periods the database cannot be brought back to.
CREATE TABLE pitr_gaps (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id     INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    service    TEXT    NOT NULL,
    started_at INTEGER NOT NULL,
    -- NULL while it lasts.
    ended_at   INTEGER,
    reason     TEXT    NOT NULL CHECK (reason IN ('mount_missing', 'dropped', 'version_changed'))
);
CREATE INDEX pitr_gaps_app ON pitr_gaps (app_id, started_at);
