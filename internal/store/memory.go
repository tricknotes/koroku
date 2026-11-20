package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// MemoryBucketSize is the span one memory_usage row covers.
const MemoryBucketSize = 5 * time.Minute

// MemorySample is one service's memory use at one sampling.
type MemorySample struct {
	AppID   int64
	Service string
	Bytes   int64
	// LimitBytes is nil when the container has no memory limit.
	LimitBytes *int64
	// Restarts is how many automatic restarts happened since the previous
	// sampling.
	Restarts int
}

// MemoryBucket is a service's memory use during one 5-minute bucket.
type MemoryBucket struct {
	Service  string
	Start    time.Time
	MaxBytes int64
	SumBytes int64
	Samples  int64
	// LimitBytes is the limit last seen in the bucket; nil without one.
	LimitBytes *int64
	Restarts   int
}

// RecordMemorySamples adds samples taken at `at` to their 5-minute
// buckets, in one transaction. Samples of apps deleted meanwhile are
// dropped.
func (s *Store) RecordMemorySamples(ctx context.Context, at time.Time, samples []MemorySample) error {
	if len(samples) == 0 {
		return nil
	}
	bucket := toMillis(at.Truncate(MemoryBucketSize))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record memory samples: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO memory_usage (app_id, service, bucket_start, max_bytes, sum_bytes, samples, limit_bytes, restarts)
		SELECT ?, ?, ?, ?, ?, 1, ?, ? WHERE EXISTS (SELECT 1 FROM apps WHERE id = ?)
		ON CONFLICT (app_id, service, bucket_start) DO UPDATE SET
			max_bytes = max(max_bytes, excluded.max_bytes),
			sum_bytes = sum_bytes + excluded.sum_bytes,
			samples = samples + 1,
			limit_bytes = excluded.limit_bytes,
			restarts = restarts + excluded.restarts`)
	if err != nil {
		return fmt.Errorf("record memory samples: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, sample := range samples {
		var limit sql.NullInt64
		if sample.LimitBytes != nil {
			limit = sql.NullInt64{Int64: *sample.LimitBytes, Valid: true}
		}
		if _, err := stmt.ExecContext(ctx, sample.AppID, sample.Service, bucket,
			sample.Bytes, sample.Bytes, limit, sample.Restarts, sample.AppID); err != nil {
			return fmt.Errorf("record memory samples: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("record memory samples: %w", err)
	}
	return nil
}

// ListMemoryUsage returns the app's buckets that start at or after since,
// ordered by service and time.
func (s *Store) ListMemoryUsage(ctx context.Context, appID int64, since time.Time) ([]MemoryBucket, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT service, bucket_start, max_bytes, sum_bytes, samples, limit_bytes, restarts
		FROM memory_usage WHERE app_id = ? AND bucket_start >= ?
		ORDER BY service, bucket_start`, appID, toMillis(since))
	if err != nil {
		return nil, fmt.Errorf("list memory usage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MemoryBucket
	for rows.Next() {
		var b MemoryBucket
		var start int64
		var limit sql.NullInt64
		if err := rows.Scan(&b.Service, &start, &b.MaxBytes, &b.SumBytes, &b.Samples, &limit, &b.Restarts); err != nil {
			return nil, fmt.Errorf("list memory usage: %w", err)
		}
		b.Start = fromMillis(start)
		if limit.Valid {
			v := limit.Int64
			b.LimitBytes = &v
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list memory usage: %w", err)
	}
	return out, nil
}

// DeleteMemoryUsageBefore removes buckets that start before t.
func (s *Store) DeleteMemoryUsageBefore(ctx context.Context, t time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM memory_usage WHERE bucket_start < ?`, toMillis(t))
	if err != nil {
		return 0, fmt.Errorf("delete old memory usage: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete old memory usage: %w", err)
	}
	return n, nil
}
