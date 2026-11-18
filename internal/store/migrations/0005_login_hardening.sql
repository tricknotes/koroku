-- Session list, revocation and login history (ARCHITECTURE 10.3).

-- id identifies a session in the console and the API; the token hash never
-- leaves the server. Existing sessions get a random id here.
ALTER TABLE sessions ADD COLUMN id TEXT NOT NULL DEFAULT '';
UPDATE sessions SET id = lower(hex(randomblob(8))) WHERE id = '';
CREATE UNIQUE INDEX sessions_id ON sessions (id);

ALTER TABLE sessions ADD COLUMN last_seen_at INTEGER NOT NULL DEFAULT 0;
UPDATE sessions SET last_seen_at = created_at;
ALTER TABLE sessions ADD COLUMN ip TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN user_agent TEXT NOT NULL DEFAULT '';

-- Login attempts by a known GitHub user, kept to the newest few hundred.
CREATE TABLE login_events (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    at             INTEGER NOT NULL,
    outcome        TEXT    NOT NULL CHECK (outcome IN
                   ('succeeded', 'not_allowed', 'two_factor_required', 'two_factor_unknown')),
    github_user_id INTEGER NOT NULL,
    login          TEXT    NOT NULL DEFAULT '',
    ip             TEXT    NOT NULL DEFAULT '',
    user_agent     TEXT    NOT NULL DEFAULT ''
);
