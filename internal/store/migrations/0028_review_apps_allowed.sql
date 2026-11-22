-- Whether the owner allows review apps of this app (ARCHITECTURE 9.9).
-- Off by default: a review app hands the parent's variables and data to
-- the code of a pull request. Parents that already have review apps start
-- allowed, so that what worked keeps working.
ALTER TABLE apps ADD COLUMN review_apps_allowed INTEGER NOT NULL DEFAULT 0;
UPDATE apps SET review_apps_allowed = 1 WHERE id IN (SELECT parent_app_id FROM review_apps);
