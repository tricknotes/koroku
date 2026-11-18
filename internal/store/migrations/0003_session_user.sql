-- Who a session belongs to, for display in the console. GitHub access
-- tokens are never stored.

ALTER TABLE sessions ADD COLUMN login TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN avatar_url TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;

CREATE INDEX sessions_expires_at ON sessions (expires_at);
