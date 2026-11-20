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

// Task is a named command registered for an app (ARCHITECTURE 9.5).
type Task struct {
	Name    string
	Service string
	Command string
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
}

// ListTasks returns the registered tasks of an app in display order.
func (s *Store) ListTasks(ctx context.Context, appID int64) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, service, command FROM app_tasks WHERE app_id = ? ORDER BY position`, appID)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	tasks := []Task{}
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.Name, &t.Service, &t.Command); err != nil {
			return nil, fmt.Errorf("list tasks: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	return tasks, nil
}

// GetTask returns one registered task by name.
func (s *Store) GetTask(ctx context.Context, appID int64, name string) (Task, error) {
	var t Task
	err := s.db.QueryRowContext(ctx,
		`SELECT name, service, command FROM app_tasks WHERE app_id = ? AND name = ?`, appID, name).
		Scan(&t.Name, &t.Service, &t.Command)
	if errors.Is(err, sql.ErrNoRows) {
		return Task{}, ErrNotFound
	}
	if err != nil {
		return Task{}, fmt.Errorf("get task: %w", err)
	}
	return t, nil
}

// ReplaceTasks replaces the registered tasks of an app in one transaction.
// Names must be unique; a duplicate returns ErrConflict.
func (s *Store) ReplaceTasks(ctx context.Context, appID int64, tasks []Task) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace tasks: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM app_tasks WHERE app_id = ?`, appID); err != nil {
		return fmt.Errorf("replace tasks: %w", err)
	}
	for i, t := range tasks {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO app_tasks (app_id, position, name, service, command) VALUES (?, ?, ?, ?, ?)`,
			appID, i, t.Name, t.Service, t.Command); err != nil {
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
	exit_code, error, started_at, finished_at`

func scanTaskRun(row rowScanner) (TaskRun, error) {
	var (
		r          TaskRun
		deployment sql.NullInt64
		exitCode   sql.NullInt64
		started    int64
		finished   sql.NullInt64
	)
	err := row.Scan(&r.ID, &r.AppID, &r.TaskName, &r.Service, &r.Command, &deployment, &r.Status,
		&exitCode, &r.Error, &started, &finished)
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
	return r, nil
}

// StartTaskRun records a run that starts now.
func (s *Store) StartTaskRun(ctx context.Context, run TaskRun, now time.Time) (TaskRun, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO task_runs (app_id, task_name, service, command, deployment_id, status, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		run.AppID, run.TaskName, run.Service, run.Command, nullInt(run.DeploymentID), StatusRunning, toMillis(now))
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
