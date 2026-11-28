-- Dumps between the daily runs and restoring an app's own dump
-- (ARCHITECTURE 10.8).

-- Hours between two dumps of the app, counted from the daily run.
ALTER TABLE apps ADD COLUMN dump_interval_hours INTEGER NOT NULL DEFAULT 24
    CHECK (dump_interval_hours IN (1, 2, 3, 4, 6, 8, 12, 24));

-- backup_runs gains the trigger 'interval', which its CHECK refuses, so the
-- table is rebuilt. Dropping it would delete the rows of
-- backup_run_targets through ON DELETE CASCADE, so that table is rebuilt
-- first, against the new parent, and gains the duration of each target.
CREATE TABLE backup_runs_new (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    trigger          TEXT    NOT NULL
                     CHECK (trigger IN ('scheduled', 'manual', 'pre_deploy', 'interval')),
    status           TEXT    NOT NULL DEFAULT 'running'
                     CHECK (status IN ('running', 'succeeded', 'failed')),
    error            TEXT    NOT NULL DEFAULT '',
    started_at       INTEGER NOT NULL,
    finished_at      INTEGER,
    targets_recorded INTEGER NOT NULL DEFAULT 0
);
INSERT INTO backup_runs_new (id, trigger, status, error, started_at, finished_at, targets_recorded)
    SELECT id, trigger, status, error, started_at, finished_at, targets_recorded FROM backup_runs;

CREATE TABLE backup_run_targets_new (
    run_id      INTEGER NOT NULL REFERENCES backup_runs_new (id) ON DELETE CASCADE,
    app_id      INTEGER REFERENCES apps (id) ON DELETE CASCADE,
    kind        TEXT    NOT NULL CHECK (kind IN ('system', 'volume', 'dump')),
    target      TEXT    NOT NULL DEFAULT '',
    status      TEXT    NOT NULL CHECK (status IN ('succeeded', 'warning', 'failed')),
    error       TEXT    NOT NULL DEFAULT '',
    duration_ms INTEGER NOT NULL DEFAULT 0
);
INSERT INTO backup_run_targets_new (run_id, app_id, kind, target, status, error)
    SELECT run_id, app_id, kind, target, status, error FROM backup_run_targets;

DROP TABLE backup_run_targets;
DROP TABLE backup_runs;
ALTER TABLE backup_runs_new RENAME TO backup_runs;
ALTER TABLE backup_run_targets_new RENAME TO backup_run_targets;

CREATE INDEX backup_runs_started_at ON backup_runs (started_at DESC);
CREATE INDEX backup_run_targets_app ON backup_run_targets (app_id, run_id);
CREATE INDEX backup_run_targets_run ON backup_run_targets (run_id);

-- A restore of an app's own dump is recorded as a database copy whose
-- source and target are the app, with the snapshot it loaded.
ALTER TABLE db_copies ADD COLUMN kind TEXT NOT NULL DEFAULT 'copy'
    CHECK (kind IN ('copy', 'restore'));
ALTER TABLE db_copies ADD COLUMN source_snapshot_id TEXT NOT NULL DEFAULT '';
