-- Memory use of each service in 5-minute buckets (ARCHITECTURE 9.5),
-- written by the sampler in `koroku serve` every minute and kept 14 days.
-- bucket_start is Unix milliseconds, a multiple of five minutes. The
-- average is sum_bytes / samples. limit_bytes is the limit last seen in the
-- bucket, NULL when the container had none. restarts counts the automatic
-- restarts (Docker's RestartCount growing) seen during the bucket.
CREATE TABLE memory_usage (
    app_id       INTEGER NOT NULL REFERENCES apps (id) ON DELETE CASCADE,
    service      TEXT    NOT NULL,
    bucket_start INTEGER NOT NULL,
    max_bytes    INTEGER NOT NULL,
    sum_bytes    INTEGER NOT NULL,
    samples      INTEGER NOT NULL,
    limit_bytes  INTEGER,
    restarts     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (app_id, service, bucket_start)
) WITHOUT ROWID;

CREATE INDEX memory_usage_bucket_start ON memory_usage (bucket_start);
