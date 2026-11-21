-- What each backup run did per target (ARCHITECTURE 10.2): one row per
-- volume, dump hook and the system, so an app's screen shows its own
-- failures and not those of other apps. app_id is NULL for the system.
-- A run with no rows but targets_recorded = 1 failed before it backed up
-- anything (the repository could not be opened, or koroku restarted), which
-- concerns every app. Runs recorded before this table have
-- targets_recorded = 0 and are not used per app.
ALTER TABLE backup_runs ADD COLUMN targets_recorded INTEGER NOT NULL DEFAULT 0;

CREATE TABLE backup_run_targets (
    run_id INTEGER NOT NULL REFERENCES backup_runs (id) ON DELETE CASCADE,
    app_id INTEGER REFERENCES apps (id) ON DELETE CASCADE,
    kind   TEXT    NOT NULL CHECK (kind IN ('system', 'volume', 'dump')),
    target TEXT    NOT NULL DEFAULT '',
    status TEXT    NOT NULL CHECK (status IN ('succeeded', 'warning', 'failed')),
    error  TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX backup_run_targets_app ON backup_run_targets (app_id, run_id);
CREATE INDEX backup_run_targets_run ON backup_run_targets (run_id);
