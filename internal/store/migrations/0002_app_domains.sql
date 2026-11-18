-- Custom domains (ARCHITECTURE 8.1). The automatic <slug>.<base_domain>
-- hostname is not stored here; it always exists.

CREATE TABLE app_domains (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id          INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    hostname        TEXT    NOT NULL UNIQUE,
    status          TEXT    NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'active')),
    is_primary      INTEGER NOT NULL DEFAULT 0,
    force_active    INTEGER NOT NULL DEFAULT 0,
    last_checked_at INTEGER,
    last_error      TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    -- Only a hostname that is being served can be the redirect target.
    CHECK (is_primary = 0 OR status = 'active')
);

CREATE INDEX app_domains_app_id ON app_domains (app_id);

-- At most one primary domain per app.
CREATE UNIQUE INDEX app_domains_one_primary ON app_domains (app_id) WHERE is_primary = 1;
