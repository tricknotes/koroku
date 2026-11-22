package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// InterruptedTaskError is recorded on runs that were running when koroku stopped.
const InterruptedTaskError = "interrupted: koroku restarted while the command was running"

// Task is a named command registered for an app (ARCHITECTURE 9.6).
type Task struct {
	Name    string
	Service string
	Command string
	// Schedule is a five-field cron expression; empty when the task only
	// runs by hand.
	Schedule string
	// LastSkippedAt is the latest scheduled time that was not run, with
	// the reason (a TaskSkip constant) and, for TaskSkipError, the
	// problem. Saving the tasks keeps it for a task whose name and
	// schedule stay the same.
	LastSkippedAt  *time.Time
	LastSkipReason string
	LastSkipDetail string
}

// Why a scheduled run did not happen.
const (
	// TaskSkipStopped: the app is stopped.
	TaskSkipStopped = "stopped"
	// TaskSkipNotDeployed: the app has no release to run in.
	TaskSkipNotDeployed = "not_deployed"
	// TaskSkipOverlap: the previous run of the task was still queued or
	// running.
	TaskSkipOverlap = "overlap"
	// TaskSkipBusy: a deployment or another operation held the app for
	// too long.
	TaskSkipBusy = "busy"
	// TaskSkipError: the run could not start (policy, unknown service).
	TaskSkipError = "error"
)

// What started a run.
const (
	TaskTriggerManual    = "manual"
	TaskTriggerScheduled = "scheduled"
)

// ScheduledTask is a registered task with a schedule, with its app.
type ScheduledTask struct {
	AppID int64
	Task
}

// TaskRun is one run of a command in a service of an app. Runs move from
// running to succeeded, failed or canceled; they are never queued.
type TaskRun struct {
	ID           int64
	AppID        int64
	TaskName     string
	Service      string
	Command      string
	DeploymentID *int64
	Status       string
	ExitCode     *int
	Error        string
	StartedAt    time.Time
	FinishedAt   *time.Time
	// Actor is who started the run; nil for older records and for
	// scheduled runs.
	Actor *Actor
	// Trigger is TaskTriggerManual ("" means the same) or
	// TaskTriggerScheduled.
	Trigger string
}

const taskColumns = `name, service, command, schedule, last_skipped_at, last_skip_reason, last_skip_detail`

func scanTask(row rowScanner, extra ...any) (Task, error) {
	var (
		t       Task
		skipped sql.NullInt64
	)
	dest := append([]any{&t.Name, &t.Service, &t.Command, &t.Schedule, &skipped, &t.LastSkipReason, &t.LastSkipDetail}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Task{}, ErrNotFound
		}
		return Task{}, err
	}
	t.LastSkippedAt = fromNullMillis(skipped)
	return t, nil
}

// ListTasks returns the registered tasks of an app in display order.
func (s *Store) ListTasks(ctx context.Context, appID int64) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM app_tasks WHERE app_id = ? ORDER BY position`, appID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tasks := []Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	return tasks, nil
}

// ListScheduledTasks returns the registered tasks of every app that have a
// schedule, ordered by app and display order.
func (s *Store) ListScheduledTasks(ctx context.Context) ([]ScheduledTask, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+`, app_id FROM app_tasks WHERE schedule != '' ORDER BY app_id, position`)
	if err != nil {
		return nil, fmt.Errorf("list scheduled tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tasks := []ScheduledTask{}
	for rows.Next() {
		var appID int64
		t, err := scanTask(rows, &appID)
		if err != nil {
			return nil, fmt.Errorf("list scheduled tasks: %w", err)
		}
		tasks = append(tasks, ScheduledTask{AppID: appID, Task: t})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list scheduled tasks: %w", err)
	}
	return tasks, nil
}

// GetTask returns one registered task by name.
func (s *Store) GetTask(ctx context.Context, appID int64, name string) (Task, error) {
	t, err := scanTask(s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM app_tasks WHERE app_id = ? AND name = ?`, appID, name))
	if errors.Is(err, ErrNotFound) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("get task: %w", err)
	}
	return t, nil
}

// RecordTaskSkip records that the scheduled time at of a task was not run.
// It changes nothing when the task is gone or its schedule changed since.
func (s *Store) RecordTaskSkip(ctx context.Context, appID int64, name, schedule string, at time.Time, reason, detail string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE app_tasks SET last_skipped_at = ?, last_skip_reason = ?, last_skip_detail = ?
		 WHERE app_id = ? AND name = ? AND schedule = ?`,
		toMillis(at), reason, detail, appID, name, schedule)
	if err != nil {
		return fmt.Errorf("record task skip: %w", err)
	}
	return nil
}

// ReplaceTasks replaces the registered tasks of an app in one transaction.
// Names must be unique; a duplicate returns ErrConflict.
func (s *Store) ReplaceTasks(ctx context.Context, appID int64, tasks []Task) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace tasks: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The latest skip stays with a task whose name and schedule stay.
	kept := map[string]Task{}
	rows, err := tx.QueryContext(ctx, `SELECT `+taskColumns+` FROM app_tasks WHERE app_id = ?`, appID)
	if err != nil {
		return fmt.Errorf("replace tasks: %w", err)
	}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("replace tasks: %w", err)
		}
		kept[t.Name] = t
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("replace tasks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_tasks WHERE app_id = ?`, appID); err != nil {
		return fmt.Errorf("replace tasks: %w", err)
	}
	for i, t := range tasks {
		var skipped sql.NullInt64
		reason, detail := "", ""
		if prev, ok := kept[t.Name]; ok && prev.Schedule == t.Schedule && t.Schedule != "" && prev.LastSkippedAt != nil {
			skipped = sql.NullInt64{Int64: toMillis(*prev.LastSkippedAt), Valid: true}
			reason, detail = prev.LastSkipReason, prev.LastSkipDetail
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO app_tasks (app_id, position, name, service, command, schedule,
				last_skipped_at, last_skip_reason, last_skip_detail)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			appID, i, t.Name, t.Service, t.Command, t.Schedule, skipped, reason, detail); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("task %q: %w", t.Name, ErrConflict)
			}
			return fmt.Errorf("replace tasks: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace tasks: %w", err)
	}
	return nil
}

const taskRunColumns = `id, app_id, task_name, service, command, deployment_id, status,
	exit_code, error, started_at, finished_at, actor_user_id, actor_login, trigger_kind`

func scanTaskRun(row rowScanner) (TaskRun, error) {
	var (
		r          TaskRun
		deployment sql.NullInt64
		exitCode   sql.NullInt64
		started    int64
		finished   sql.NullInt64
		actorID    sql.NullInt64
		actor      string
	)
	err := row.Scan(&r.ID, &r.AppID, &r.TaskName, &r.Service, &r.Command, &deployment, &r.Status,
		&exitCode, &r.Error, &started, &finished, &actorID, &actor, &r.Trigger)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskRun{}, ErrNotFound
	}
	if err != nil {
		return TaskRun{}, fmt.Errorf("scan task run: %w", err)
	}
	if deployment.Valid {
		r.DeploymentID = &deployment.Int64
	}
	if exitCode.Valid {
		code := int(exitCode.Int64)
		r.ExitCode = &code
	}
	r.StartedAt = fromMillis(started)
	r.FinishedAt = fromNullMillis(finished)
	r.Actor = actorFromColumns(actorID, actor)
	return r, nil
}

// StartTaskRun records a run that starts now.
func (s *Store) StartTaskRun(ctx context.Context, run TaskRun, now time.Time) (TaskRun, error) {
	actorID, actorLogin := actorColumns(ctx)
	trigger := run.Trigger
	if trigger == "" {
		trigger = TaskTriggerManual
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO task_runs (app_id, task_name, service, command, deployment_id, status, started_at,
			actor_user_id, actor_login, trigger_kind)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.AppID, run.TaskName, run.Service, run.Command, nullInt(run.DeploymentID), StatusRunning, toMillis(now),
		actorID, actorLogin, trigger)
	if err != nil {
		return TaskRun{}, fmt.Errorf("start task run: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return TaskRun{}, fmt.Errorf("start task run: %w", err)
	}
	return s.GetTaskRun(ctx, id)
}

// GetTaskRun returns one run.
func (s *Store) GetTaskRun(ctx context.Context, id int64) (TaskRun, error) {
	return scanTaskRun(s.db.QueryRowContext(ctx, `SELECT `+taskRunColumns+` FROM task_runs WHERE id = ?`, id))
}

// ListTaskRuns returns the newest runs of an app.
func (s *Store) ListTaskRuns(ctx context.Context, appID int64, limit int) ([]TaskRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskRunColumns+` FROM task_runs WHERE app_id = ? ORDER BY id DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("list task runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := []TaskRun{}
	for rows.Next() {
		r, err := scanTaskRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list task runs: %w", err)
	}
	return runs, nil
}

// FinishTaskRun moves a running run to a terminal status. exitCode is nil
// when the command did not report one.
func (s *Store) FinishTaskRun(ctx context.Context, id int64, status string, exitCode *int, errMsg string, now time.Time) error {
	if status != StatusSucceeded && status != StatusFailed && status != StatusCanceled {
		return fmt.Errorf("finish task run: %w", ErrInvalidTransition)
	}
	var code sql.NullInt64
	if exitCode != nil {
		code = sql.NullInt64{Int64: int64(*exitCode), Valid: true}
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE task_runs SET status = ?, exit_code = ?, error = ?, finished_at = ? WHERE id = ? AND status = ?`,
		status, code, errMsg, toMillis(now), id, StatusRunning)
	if err != nil {
		return fmt.Errorf("finish task run: %w", err)
	}
	return requireTransition(res, "finish task run")
}

// FailInterruptedTaskRuns marks runs left running by a previous process as
// failed and returns them.
func (s *Store) FailInterruptedTaskRuns(ctx context.Context, now time.Time) ([]TaskRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("fail interrupted task runs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT `+taskRunColumns+` FROM task_runs WHERE status = ? ORDER BY id`, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("fail interrupted task runs: %w", err)
	}
	var runs []TaskRun
	for rows.Next() {
		r, err := scanTaskRun(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		runs = append(runs, r)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("fail interrupted task runs: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE task_runs SET status = ?, error = ?, finished_at = ? WHERE status = ?`,
		StatusFailed, InterruptedTaskError, toMillis(now), StatusRunning); err != nil {
		return nil, fmt.Errorf("fail interrupted task runs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fail interrupted task runs: %w", err)
	}
	return runs, nil
}

// PruneTaskRuns deletes the finished runs of an app beyond the newest keep
// runs and returns the IDs it deleted, so that their logs can go too.
func (s *Store) PruneTaskRuns(ctx context.Context, appID int64, keep int) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("prune task runs: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM task_runs WHERE app_id = ? ORDER BY id DESC LIMIT -1 OFFSET ?`, appID, keep)
	if err != nil {
		return nil, fmt.Errorf("prune task runs: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("prune task runs: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("prune task runs: %w", err)
	}
	var deleted []int64
	for _, id := range ids {
		// A run still going is never removed, however old.
		res, err := tx.ExecContext(ctx, `DELETE FROM task_runs WHERE id = ? AND status != ?`, id, StatusRunning)
		if err != nil {
			return nil, fmt.Errorf("prune task runs: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			deleted = append(deleted, id)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("prune task runs: %w", err)
	}
	return deleted, nil
}
