-- Scheduled registered tasks (ARCHITECTURE 9.6).
-- schedule: a five-field cron expression, read in config.toml's timezone;
-- empty when the task only runs by hand.
-- last_skipped_at, last_skip_reason, last_skip_detail: the latest scheduled
-- time that was not run and why (stopped, not_deployed, overlap, busy,
-- error). Only the latest one is kept, so that skips do not crowd the run
-- history.
ALTER TABLE app_tasks ADD COLUMN schedule TEXT NOT NULL DEFAULT '';
ALTER TABLE app_tasks ADD COLUMN last_skipped_at INTEGER;
ALTER TABLE app_tasks ADD COLUMN last_skip_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE app_tasks ADD COLUMN last_skip_detail TEXT NOT NULL DEFAULT '';

-- What started a run: manual (the console or the API) or scheduled.
ALTER TABLE task_runs ADD COLUMN trigger_kind TEXT NOT NULL DEFAULT 'manual';
