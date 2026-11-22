-- Sleeping while idle (ARCHITECTURE 8.4).
-- idle_sleep: stop the app's containers after idle_sleep_minutes without a
-- request through the reverse proxy; the next request wakes it.
-- sleeping_since: when the app went to sleep (Unix milliseconds); NULL
-- while it is awake. A runtime state, apart from desired_state.
ALTER TABLE apps ADD COLUMN idle_sleep INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apps ADD COLUMN idle_sleep_minutes INTEGER NOT NULL DEFAULT 30;
ALTER TABLE apps ADD COLUMN sleeping_since INTEGER;
