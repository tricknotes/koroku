-- The container registry credential of an app (ARCHITECTURE 6.1). One per
-- app; the token is encrypted with the master key like env var values and
-- leaves koroku only in the app's own Docker config file.
CREATE TABLE registry_credentials (
    app_id          INTEGER PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    registry        TEXT    NOT NULL,
    username        TEXT    NOT NULL,
    token_encrypted BLOB    NOT NULL,
    updated_at      INTEGER NOT NULL
);
