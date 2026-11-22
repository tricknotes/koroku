-- An automatic deployment received while a command ran on the app, kept
-- until the command ends (ARCHITECTURE 5.2). One per app: a later push
-- replaces it. workflow is the workflow the app waited for, '' if none.
CREATE TABLE pending_auto_deploys (
    app_id         INTEGER PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    ref            TEXT    NOT NULL,
    commit_sha     TEXT    NOT NULL,
    commit_message TEXT    NOT NULL DEFAULT '',
    pusher         TEXT    NOT NULL DEFAULT '',
    workflow       TEXT    NOT NULL DEFAULT '',
    received_at    INTEGER NOT NULL
);
