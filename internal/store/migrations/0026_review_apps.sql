-- Review apps (ARCHITECTURE 9.9): an app that runs one pull request of its
-- parent's repository. Its members' roles are the parent's.
-- data_copy: '' (not asked), waiting (copy once the first deployment
-- succeeds), started (see db_copies), skipped or failed (why in
-- data_copy_detail).
-- The parent cannot be deleted while it has review apps; the API refuses
-- it, and RESTRICT keeps the database from orphaning one.
CREATE TABLE review_apps (
    app_id INTEGER PRIMARY KEY REFERENCES apps (id) ON DELETE CASCADE,
    parent_app_id INTEGER NOT NULL REFERENCES apps (id) ON DELETE RESTRICT,
    pr_number INTEGER NOT NULL,
    pr_title TEXT NOT NULL DEFAULT '',
    head_branch TEXT NOT NULL DEFAULT '',
    pr_state TEXT NOT NULL DEFAULT 'open',
    pr_checked_at INTEGER,
    data_copy TEXT NOT NULL DEFAULT '',
    data_copy_detail TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    UNIQUE (parent_app_id, pr_number)
);
CREATE INDEX review_apps_parent ON review_apps (parent_app_id);
