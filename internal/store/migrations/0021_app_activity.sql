-- What people did to an app (ARCHITECTURE 10.7): settings, environment
-- variables (keys only, never values), domains, start/stop, restores, task
-- and database copy starts, roles. actor_user_id is NULL for what koroku
-- did by itself; the login is kept as it was at the time. details is a
-- small JSON object whose shape depends on action. Rows go with the app,
-- and only the newest ones per app are kept (see RecordActivity).
CREATE TABLE app_activity (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    app_id        INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    created_at    INTEGER NOT NULL,
    actor_user_id INTEGER,
    actor_login   TEXT    NOT NULL DEFAULT '',
    action        TEXT    NOT NULL,
    details       TEXT    NOT NULL DEFAULT '{}'
);

CREATE INDEX app_activity_app ON app_activity (app_id, id);
