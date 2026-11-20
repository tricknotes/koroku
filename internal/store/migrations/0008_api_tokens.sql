-- Read-only API tokens for tools on the developer's machine, such as Claude
-- Code (ARCHITECTURE 10.4). Like sessions, only a SHA-256 hash of the value
-- is stored; id names a token in the console and the API. Revoking a token
-- deletes its row.
CREATE TABLE api_tokens (
    id           TEXT    PRIMARY KEY,
    token_hash   TEXT    NOT NULL UNIQUE,
    name         TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    last_used_ip TEXT    NOT NULL DEFAULT ''
);
