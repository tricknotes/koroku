package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Backup run triggers and statuses.
const (
	BackupTriggerScheduled = "scheduled"
	BackupTriggerManual    = "manual"
	BackupTriggerPreDeploy = "pre_deploy"

	BackupRunning   = "running"
	BackupSucceeded = "succeeded"
	BackupFailed    = "failed"
)

// BackupRun is one execution of the backup job.
type BackupRun struct {
	ID         int64
	Trigger    string
	Status     string
	Error      string
	StartedAt  time.Time
	FinishedAt *time.Time
}

// StartBackupRun records the beginning of a run.
func (s *Store) StartBackupRun(ctx context.Context, trigger string, now time.Time) (BackupRun, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO backup_runs (trigger, status, started_at) VALUES (?, ?, ?)`,
		trigger, BackupRunning, toMillis(now))
	if err != nil {
		return BackupRun{}, fmt.Errorf("start backup run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return BackupRun{}, fmt.Errorf("start backup run: %w", err)
	}
	return BackupRun{ID: id, Trigger: trigger, Status: BackupRunning, StartedAt: fromMillis(toMillis(now))}, nil
}

// FinishBackupRun records the outcome of a run.
func (s *Store) FinishBackupRun(ctx context.Context, id int64, succeeded bool, errMsg string, now time.Time) error {
	status := BackupFailed
	if succeeded {
		status = BackupSucceeded
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE backup_runs SET status = ?, error = ?, finished_at = ? WHERE id = ? AND status = ?`,
		status, errMsg, toMillis(now), id, BackupRunning)
	if err != nil {
		return fmt.Errorf("finish backup run: %w", err)
	}
	return requireOneRow(res, "finish backup run")
}

// FailInterruptedBackupRuns marks runs left running by a previous process
// as failed. It is called once at startup.
func (s *Store) FailInterruptedBackupRuns(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE backup_runs SET status = ?, error = ?, finished_at = ? WHERE status = ?`,
		BackupFailed, "interrupted: koroku restarted during the backup", toMillis(now), BackupRunning)
	if err != nil {
		return fmt.Errorf("fail interrupted backup runs: %w", err)
	}
	return nil
}

// ListBackupRuns returns the newest runs.
func (s *Store) ListBackupRuns(ctx context.Context, limit int) ([]BackupRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, trigger, status, error, started_at, finished_at
		 FROM backup_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list backup runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []BackupRun
	for rows.Next() {
		var (
			r        BackupRun
			started  int64
			finished sql.NullInt64
		)
		if err := rows.Scan(&r.ID, &r.Trigger, &r.Status, &r.Error, &started, &finished); err != nil {
			return nil, fmt.Errorf("list backup runs: %w", err)
		}
		r.StartedAt = fromMillis(started)
		r.FinishedAt = fromNullMillis(finished)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list backup runs: %w", err)
	}
	return out, nil
}

// LastSuccessfulBackup returns when a scheduled or manual run last
// succeeded, or nil when none has.
func (s *Store) LastSuccessfulBackup(ctx context.Context) (*time.Time, error) {
	var finished sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(finished_at) FROM backup_runs WHERE status = ? AND trigger IN (?, ?)`,
		BackupSucceeded, BackupTriggerScheduled, BackupTriggerManual).Scan(&finished)
	if err != nil {
		return nil, fmt.Errorf("read last successful backup: %w", err)
	}
	return fromNullMillis(finished), nil
}
