-- Groups of apps whose chosen services share a network (ARCHITECTURE 6.1).
-- The network itself (koroku_group_<id>) is created by koroku from these
-- rows; the database is the only record of who may join it.

CREATE TABLE app_groups (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    environment TEXT    NOT NULL CHECK (environment IN ('production', 'staging')),
    created_at  INTEGER NOT NULL
);

-- An app is in at most one group. A group with members cannot be deleted.
ALTER TABLE apps ADD COLUMN group_id INTEGER REFERENCES app_groups (id) ON DELETE RESTRICT;
-- The services of the app that join the group network, as a JSON array.
ALTER TABLE apps ADD COLUMN group_services TEXT NOT NULL DEFAULT '[]';
