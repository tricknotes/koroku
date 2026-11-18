-- Backup settings per app and the history of backup runs (ARCHITECTURE 10.2).

-- JSON array of volume names (as written in the Compose file) to skip.
ALTER TABLE apps ADD COLUMN backup_exclude_volumes TEXT NOT NULL DEFAULT '[]';
-- Dump hook: a command run inside a service whose stdout is backed up.
ALTER TABLE apps ADD COLUMN dump_hook_service TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN dump_hook_command TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN backup_before_deploy INTEGER NOT NULL DEFAULT 0;

CREATE TABLE backup_runs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    trigger     TEXT    NOT NULL CHECK (trigger IN ('scheduled', 'manual', 'pre_deploy')),
    status      TEXT    NOT NULL DEFAULT 'running'
                CHECK (status IN ('running', 'succeeded', 'failed')),
    error       TEXT    NOT NULL DEFAULT '',
    started_at  INTEGER NOT NULL,
    finished_at INTEGER
);

CREATE INDEX backup_runs_started_at ON backup_runs (started_at DESC);
