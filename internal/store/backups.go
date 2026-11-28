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
	// BackupTriggerInterval is a run between the daily ones that takes the
	// dumps of the apps due at that hour (ARCHITECTURE 10.8).
	BackupTriggerInterval = "interval"

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
	// Duration is how long the target took; zero when it was not measured.
	Duration time.Duration
	// FinishedAt is when the run that produced it finished. It is set on
	// reads only.
	FinishedAt *time.Time
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
				`INSERT INTO backup_run_targets (run_id, app_id, kind, target, status, error, duration_ms)
				 VALUES (?, NULL, ?, ?, ?, ?, ?)`, id, t.Kind, t.Target, t.Status, t.Error, t.Duration.Milliseconds())
		} else {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO backup_run_targets (run_id, app_id, kind, target, status, error, duration_ms)
				 SELECT ?, id, ?, ?, ?, ?, ? FROM apps WHERE slug = ?`,
				id, t.Kind, t.Target, t.Status, t.Error, t.Duration.Milliseconds(), t.App)
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
//
// Runs between the daily ones take dumps only, so they are not that run:
// the outcome is the newest other run, with each of its dump targets
// replaced by a newer result of an interval run, so that an hourly dump
// does not hide how the volumes did (ARCHITECTURE 10.8). A dump that only
// interval runs have taken is added.
func (s *Store) LatestAppBackupOutcome(ctx context.Context, appID int64) (*BackupOutcome, error) {
	out, err := s.latestBackupOutcome(ctx,
		`EXISTS (SELECT 1 FROM backup_run_targets t WHERE t.run_id = r.id AND t.app_id = ?)`,
		`t.app_id = ?`, appID)
	if err != nil {
		return nil, err
	}
	var after int64
	if out != nil {
		after = out.Run.ID
	}
	newer, err := s.intervalDumps(ctx, appID, after)
	if err != nil {
		return nil, err
	}
	if len(newer) == 0 {
		return out, nil
	}
	if out == nil {
		run, err := s.backupRun(ctx, newer[0].runID)
		if err != nil {
			return nil, err
		}
		out = &BackupOutcome{Run: run}
	}
	for _, d := range newer {
		replaced := false
		for i, t := range out.Targets {
			if t.Kind == d.target.Kind && t.Target == d.target.Target {
				out.Targets[i], replaced = d.target, true
			}
		}
		if !replaced {
			out.Targets = append(out.Targets, d.target)
		}
	}
	return out, nil
}

type intervalDump struct {
	runID  int64
	target BackupTarget
}

// intervalDumps returns, per dump target of the app, its newest result from
// an interval run after the given run, newest run first.
func (s *Store) intervalDumps(ctx context.Context, appID, afterRunID int64) ([]intervalDump, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT r.id, r.finished_at, t.kind, t.target, t.status, t.error, t.duration_ms
		 FROM backup_run_targets t JOIN backup_runs r ON r.id = t.run_id
		 WHERE t.app_id = ? AND t.kind = 'dump' AND r.trigger = ? AND r.status != 'running'
		   AND r.targets_recorded = 1 AND r.id > ?
		 ORDER BY r.id DESC`, appID, BackupTriggerInterval, afterRunID)
	if err != nil {
		return nil, fmt.Errorf("read interval dumps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []intervalDump
	seen := map[string]bool{}
	for rows.Next() {
		var (
			d        intervalDump
			finished sql.NullInt64
			ms       int64
		)
		if err := rows.Scan(&d.runID, &finished, &d.target.Kind, &d.target.Target, &d.target.Status, &d.target.Error, &ms); err != nil {
			return nil, fmt.Errorf("read interval dumps: %w", err)
		}
		if seen[d.target.Target] {
			continue
		}
		seen[d.target.Target] = true
		d.target.Duration = time.Duration(ms) * time.Millisecond
		d.target.FinishedAt = fromNullMillis(finished)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read interval dumps: %w", err)
	}
	return out, nil
}

// backupRun reads one run.
func (s *Store) backupRun(ctx context.Context, id int64) (BackupRun, error) {
	var (
		run      BackupRun
		started  int64
		finished sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, trigger, status, error, started_at, finished_at FROM backup_runs WHERE id = ?`, id).
		Scan(&run.ID, &run.Trigger, &run.Status, &run.Error, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return BackupRun{}, ErrNotFound
	}
	if err != nil {
		return BackupRun{}, fmt.Errorf("read backup run: %w", err)
	}
	run.StartedAt = fromMillis(started)
	run.FinishedAt = fromNullMillis(finished)
	return run, nil
}

// LatestAppDump returns the newest result of the app's dump, from any run,
// or nil when no run has taken it.
func (s *Store) LatestAppDump(ctx context.Context, appID int64) (*BackupTarget, error) {
	var (
		t        BackupTarget
		finished sql.NullInt64
		ms       int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT r.finished_at, t.kind, t.target, t.status, t.error, t.duration_ms
		 FROM backup_run_targets t JOIN backup_runs r ON r.id = t.run_id
		 WHERE t.app_id = ? AND t.kind = 'dump' AND r.status != 'running' AND r.targets_recorded = 1
		 ORDER BY r.id DESC LIMIT 1`, appID).
		Scan(&finished, &t.Kind, &t.Target, &t.Status, &t.Error, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read latest app dump: %w", err)
	}
	t.Duration = time.Duration(ms) * time.Millisecond
	t.FinishedAt = fromNullMillis(finished)
	return &t, nil
}

// LastAppDumpSuccess returns when the app's dump was last stored, by any
// run, or nil when it never was.
func (s *Store) LastAppDumpSuccess(ctx context.Context, appID int64) (*time.Time, error) {
	var finished sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(r.finished_at) FROM backup_run_targets t JOIN backup_runs r ON r.id = t.run_id
		 WHERE t.app_id = ? AND t.kind = 'dump' AND t.status != 'failed' AND r.targets_recorded = 1`,
		appID).Scan(&finished)
	if err != nil {
		return nil, fmt.Errorf("read last app dump success: %w", err)
	}
	return fromNullMillis(finished), nil
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
		WHERE r.targets_recorded = 1 AND r.status != 'running' AND r.trigger != 'interval' AND (` + covers + `
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
		`SELECT t.kind, t.target, t.status, t.error, t.duration_ms FROM backup_run_targets t
		 WHERE t.run_id = ? AND `+mine+` ORDER BY t.kind, t.target`,
		append([]any{run.ID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("read latest backup outcome: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			t  BackupTarget
			ms int64
		)
		if err := rows.Scan(&t.Kind, &t.Target, &t.Status, &t.Error, &ms); err != nil {
			return nil, fmt.Errorf("read latest backup outcome: %w", err)
		}
		t.Duration = time.Duration(ms) * time.Millisecond
		t.FinishedAt = run.FinishedAt
		out.Targets = append(out.Targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read latest backup outcome: %w", err)
	}
	return out, nil
}

// LastAppBackupSuccess returns when a run last stored every target of the
// app (warnings count as stored), or nil when none has. Interval runs take
// the dump only, so they do not count (ARCHITECTURE 10.8).
func (s *Store) LastAppBackupSuccess(ctx context.Context, appID int64) (*time.Time, error) {
	var finished sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(r.finished_at) FROM backup_runs r
		 WHERE r.targets_recorded = 1 AND r.trigger != 'interval'
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

// ListBackupRuns returns the newest runs. Interval runs, up to one an hour,
// would push the daily ones out of the list; they are left out, and the
// apps' screens show their results (ARCHITECTURE 10.8).
func (s *Store) ListBackupRuns(ctx context.Context, limit int) ([]BackupRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, trigger, status, error, started_at, finished_at
		 FROM backup_runs WHERE trigger != ? ORDER BY id DESC LIMIT ?`, BackupTriggerInterval, limit)
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
