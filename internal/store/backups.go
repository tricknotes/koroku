package store

import (
	"context"
	"database/sql"
	"errors"
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

// Outcomes of one target of a backup run.
const (
	BackupTargetSucceeded = "succeeded"
	BackupTargetWarning   = "warning"
	BackupTargetFailed    = "failed"
)

// BackupTarget is what one run did to one target (a volume or the dump of
// an app, or the system).
type BackupTarget struct {
	// App is the slug; empty for the system.
	App    string
	Kind   string // system | volume | dump
	Target string
	Status string // succeeded | warning | failed
	Error  string
}

// FinishBackupRunWithTargets records the outcome of a run together with what
// it did to each target, in one transaction. A failed run without targets
// failed before backing anything up, which concerns every app. Targets of
// apps that were deleted meanwhile are dropped.
func (s *Store) FinishBackupRunWithTargets(ctx context.Context, id int64, succeeded bool, errMsg string, now time.Time, targets []BackupTarget) error {
	status := BackupFailed
	if succeeded {
		status = BackupSucceeded
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish backup run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE backup_runs SET status = ?, error = ?, finished_at = ?, targets_recorded = 1
		 WHERE id = ? AND status = ?`,
		status, errMsg, toMillis(now), id, BackupRunning)
	if err != nil {
		return fmt.Errorf("finish backup run: %w", err)
	}
	if err := requireOneRow(res, "finish backup run"); err != nil {
		return err
	}
	for _, t := range targets {
		if t.App == "" {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO backup_run_targets (run_id, app_id, kind, target, status, error)
				 VALUES (?, NULL, ?, ?, ?, ?)`, id, t.Kind, t.Target, t.Status, t.Error)
		} else {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO backup_run_targets (run_id, app_id, kind, target, status, error)
				 SELECT ?, id, ?, ?, ?, ? FROM apps WHERE slug = ?`,
				id, t.Kind, t.Target, t.Status, t.Error, t.App)
		}
		if err != nil {
			return fmt.Errorf("record backup target: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish backup run: %w", err)
	}
	return nil
}

// BackupOutcome is the latest finished run that concerns an app (or the
// system), with what it did to that app's targets. Targets is empty when
// the run failed before backing anything up.
type BackupOutcome struct {
	Run     BackupRun
	Targets []BackupTarget
}

// LatestAppBackupOutcome returns the newest finished run that backed up the
// app or failed before backing up anything; nil when there is none.
func (s *Store) LatestAppBackupOutcome(ctx context.Context, appID int64) (*BackupOutcome, error) {
	return s.latestBackupOutcome(ctx,
		`EXISTS (SELECT 1 FROM backup_run_targets t WHERE t.run_id = r.id AND t.app_id = ?)`,
		`t.app_id = ?`, appID)
}

// LatestSystemBackupOutcome is LatestAppBackupOutcome for the system.
func (s *Store) LatestSystemBackupOutcome(ctx context.Context) (*BackupOutcome, error) {
	return s.latestBackupOutcome(ctx,
		`EXISTS (SELECT 1 FROM backup_run_targets t WHERE t.run_id = r.id AND t.kind = 'system')`,
		`t.kind = 'system'`)
}

func (s *Store) latestBackupOutcome(ctx context.Context, covers, mine string, args ...any) (*BackupOutcome, error) {
	query := `SELECT r.id, r.trigger, r.status, r.error, r.started_at, r.finished_at
		FROM backup_runs r
		WHERE r.targets_recorded = 1 AND r.status != 'running' AND (` + covers + `
		  OR (r.status = 'failed' AND NOT EXISTS (SELECT 1 FROM backup_run_targets t WHERE t.run_id = r.id)))
		ORDER BY r.id DESC LIMIT 1`
	var (
		run      BackupRun
		started  int64
		finished sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&run.ID, &run.Trigger, &run.Status, &run.Error, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest backup outcome: %w", err)
	}
	run.StartedAt = fromMillis(started)
	run.FinishedAt = fromNullMillis(finished)
	out := &BackupOutcome{Run: run}
	rows, err := s.db.QueryContext(ctx,
		`SELECT t.kind, t.target, t.status, t.error FROM backup_run_targets t
		 WHERE t.run_id = ? AND `+mine+` ORDER BY t.kind, t.target`,
		append([]any{run.ID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("read latest backup outcome: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var t BackupTarget
		if err := rows.Scan(&t.Kind, &t.Target, &t.Status, &t.Error); err != nil {
			return nil, fmt.Errorf("read latest backup outcome: %w", err)
		}
		out.Targets = append(out.Targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read latest backup outcome: %w", err)
	}
	return out, nil
}

// LastAppBackupSuccess returns when a run last stored every target of the
// app (warnings count as stored), or nil when none has.
func (s *Store) LastAppBackupSuccess(ctx context.Context, appID int64) (*time.Time, error) {
	var finished sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(r.finished_at) FROM backup_runs r
		 WHERE r.targets_recorded = 1
		   AND EXISTS (SELECT 1 FROM backup_run_targets t WHERE t.run_id = r.id AND t.app_id = ?)
		   AND NOT EXISTS (SELECT 1 FROM backup_run_targets t
		                   WHERE t.run_id = r.id AND t.app_id = ? AND t.status = 'failed')`,
		appID, appID).Scan(&finished)
	if err != nil {
		return nil, fmt.Errorf("read last app backup success: %w", err)
	}
	return fromNullMillis(finished), nil
}

// FailInterruptedBackupRuns marks runs left running by a previous process
// as failed. It is called once at startup. They count as having failed
// before backing anything up: what they stored is not known.
func (s *Store) FailInterruptedBackupRuns(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE backup_runs SET status = ?, error = ?, finished_at = ?, targets_recorded = 1 WHERE status = ?`,
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
