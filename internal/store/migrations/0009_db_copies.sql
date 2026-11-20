-- Copying a production database into a staging app (ARCHITECTURE 10.5).

-- Load hook: a command run inside a service that reads a dump from stdin.
ALTER TABLE apps ADD COLUMN load_hook_service TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN load_hook_command TEXT NOT NULL DEFAULT '';
-- Command run after a copy has been loaded, before the app is started
-- again (to remove personal data, for example).
ALTER TABLE apps ADD COLUMN post_load_service TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN post_load_command TEXT NOT NULL DEFAULT '';

CREATE TABLE db_copies (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    target_app_id      INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    -- NULL once the source app is deleted; source_slug keeps its name.
    source_app_id      INTEGER REFERENCES apps (id) ON DELETE SET NULL,
    source_slug        TEXT    NOT NULL,
    source_kind        TEXT    NOT NULL CHECK (source_kind IN ('live', 'snapshot')),
    status             TEXT    NOT NULL DEFAULT 'running'
                       CHECK (status IN ('running', 'succeeded', 'failed')),
    error              TEXT    NOT NULL DEFAULT '',
    bytes              INTEGER NOT NULL DEFAULT 0,
    backup_snapshot_id TEXT    NOT NULL DEFAULT '',
    started_at         INTEGER NOT NULL,
    finished_at        INTEGER
);

CREATE INDEX db_copies_target ON db_copies (target_app_id, id DESC);
