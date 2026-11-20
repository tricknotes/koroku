-- Links between apps (ARCHITECTURE 6.1). A group bounds who may link; a
-- link says that a name in the consumer's Compose file means a service the
-- target app exposes. koroku creates one network per link from these rows;
-- the database is the only record of who reaches whom.

CREATE TABLE app_groups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    environment TEXT    NOT NULL CHECK (environment IN ('production', 'staging')),
    created_at  INTEGER NOT NULL
);

-- An app is in at most one group. A group with members cannot be deleted.
ALTER TABLE apps ADD COLUMN group_id INTEGER REFERENCES app_groups (id) ON DELETE RESTRICT;
-- The services of the app that other members may link to, as a JSON array.
ALTER TABLE apps ADD COLUMN group_services TEXT NOT NULL DEFAULT '[]';

CREATE TABLE app_links (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id         INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    name           TEXT    NOT NULL,
    target_app_id  INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    target_service TEXT    NOT NULL,
    -- The consumer's services that use the name, as a JSON array.
    services       TEXT    NOT NULL DEFAULT '[]',
    created_at     INTEGER NOT NULL,
    UNIQUE (app_id, name),
    CHECK (app_id <> target_app_id)
);

CREATE INDEX app_links_target ON app_links (target_app_id);

-- Small values of the installation, such as its ID.
CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
