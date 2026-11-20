-- Access protection of apps (ARCHITECTURE 8.3). Staging apps start
-- protected, production apps public; afterwards the flag is independent of
-- the environment. protection_exempt_paths is a JSON array of path prefixes
-- that skip the gate.
ALTER TABLE apps ADD COLUMN access_protection INTEGER NOT NULL DEFAULT 0;
UPDATE apps SET access_protection = 1 WHERE environment = 'staging';
ALTER TABLE apps ADD COLUMN protection_exempt_paths TEXT NOT NULL DEFAULT '[]';

-- One-time codes that carry a console login over to an app host. Only a
-- hash of the code is stored. Deleting the session deletes its codes.
CREATE TABLE gate_codes (
    code_hash          TEXT    PRIMARY KEY,
    hostname           TEXT    NOT NULL,
    session_token_hash TEXT    NOT NULL REFERENCES sessions (token_hash) ON DELETE CASCADE,
    expires_at         INTEGER NOT NULL
);
CREATE INDEX gate_codes_session ON gate_codes (session_token_hash);

-- Gate cookies issued on app hosts. Each grant is valid for one hostname and
-- only while the console session it came from exists.
CREATE TABLE gate_grants (
    token_hash         TEXT    PRIMARY KEY,
    hostname           TEXT    NOT NULL,
    session_token_hash TEXT    NOT NULL REFERENCES sessions (token_hash) ON DELETE CASCADE,
    created_at         INTEGER NOT NULL,
    expires_at         INTEGER NOT NULL
);
CREATE INDEX gate_grants_session ON gate_grants (session_token_hash);
