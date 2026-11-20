package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Deployment statuses. A deployment moves queued -> running -> succeeded |
// failed | canceled, or queued -> canceled.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// InterruptedError is recorded on deployments that were running when koroku stopped.
const InterruptedError = "interrupted: koroku restarted while the deployment was running"

// SupersededError is recorded on queued deployments replaced by a newer one.
const SupersededError = "superseded by a newer deployment"

// ErrInvalidTransition is returned when a status change is not allowed from
// the deployment's current status.
var ErrInvalidTransition = errors.New("invalid deployment status transition")

// CanTransition reports whether a deployment may move from one status to another.
func CanTransition(from, to string) bool {
	switch from {
	case StatusQueued:
		return to == StatusRunning || to == StatusCanceled
	case StatusRunning:
		return to == StatusSucceeded || to == StatusFailed || to == StatusCanceled
	default:
		return false
	}
}

// IsTerminal reports whether a status is final.
func IsTerminal(status string) bool {
	return status == StatusSucceeded || status == StatusFailed || status == StatusCanceled
}

// Deployment is one deploy job.
type Deployment struct {
	ID            int64
	AppID         int64
	Ref           string
	CommitSHA     string
	CommitMessage string
	Status        string
	Error         string
	QueuedAt      time.Time
	StartedAt     *time.Time
	FinishedAt    *time.Time
	// SourceDeploymentID is set on a rollback: the deployment whose kept
	// release it starts again (ARCHITECTURE 7.1).
	SourceDeploymentID *int64
}

// RollbackRef is the ref recorded on rollbacks.
const RollbackRef = "rollback"

const deploymentColumns = `id, app_id, ref, commit_sha, commit_message, status, error,
	queued_at, started_at, finished_at, source_deployment_id`

func scanDeployment(row rowScanner) (Deployment, error) {
	var (
		d        Deployment
		queued   int64
		started  sql.NullInt64
		finished sql.NullInt64
		source   sql.NullInt64
	)
	err := row.Scan(&d.ID, &d.AppID, &d.Ref, &d.CommitSHA, &d.CommitMessage, &d.Status, &d.Error,
		&queued, &started, &finished, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, fmt.Errorf("scan deployment: %w", err)
	}
	d.QueuedAt = fromMillis(queued)
	d.StartedAt = fromNullMillis(started)
	d.FinishedAt = fromNullMillis(finished)
	if source.Valid {
		d.SourceDeploymentID = &source.Int64
	}
	return d, nil
}

// EnqueueDeployment records a new queued deployment. Older queued deployments
// of the same app are canceled because only the newest request matters; their
// IDs are returned.
func (s *Store) EnqueueDeployment(ctx context.Context, appID int64, ref, sha, message string, now time.Time) (Deployment, []int64, error) {
	return s.enqueue(ctx, appID, ref, sha, message, nil, now)
}

// EnqueueRollback records a queued rollback to the kept release of source
// (ARCHITECTURE 7.1), with source's commit. Like EnqueueDeployment, it
// supersedes the app's other queued deployments.
func (s *Store) EnqueueRollback(ctx context.Context, source Deployment, now time.Time) (Deployment, []int64, error) {
	return s.enqueue(ctx, source.AppID, RollbackRef, source.CommitSHA, source.CommitMessage, &source.ID, now)
}

func (s *Store) enqueue(ctx context.Context, appID int64, ref, sha, message string, source *int64, now time.Time) (Deployment, []int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Deployment{}, nil, fmt.Errorf("enqueue deployment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM deployments WHERE app_id = ? AND status = ? ORDER BY id`, appID, StatusQueued)
	if err != nil {
		return Deployment{}, nil, fmt.Errorf("enqueue deployment: %w", err)
	}
	var superseded []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return Deployment{}, nil, fmt.Errorf("enqueue deployment: %w", err)
		}
		superseded = append(superseded, id)
	}
	if err := rows.Close(); err != nil {
		return Deployment{}, nil, fmt.Errorf("enqueue deployment: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE deployments SET status = ?, error = ?, finished_at = ? WHERE app_id = ? AND status = ?`,
		StatusCanceled, SupersededError, toMillis(now), appID, StatusQueued); err != nil {
		return Deployment{}, nil, fmt.Errorf("enqueue deployment: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO deployments (app_id, ref, commit_sha, commit_message, status, queued_at, source_deployment_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`, appID, ref, sha, message, StatusQueued, toMillis(now), nullInt(source))
	if err != nil {
		return Deployment{}, nil, fmt.Errorf("enqueue deployment: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Deployment{}, nil, fmt.Errorf("enqueue deployment: %w", err)
	}
	d, err := scanDeployment(tx.QueryRowContext(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE id = ?`, id))
	if err != nil {
		return Deployment{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return Deployment{}, nil, fmt.Errorf("enqueue deployment: %w", err)
	}
	return d, superseded, nil
}

// GetDeployment returns one deployment.
func (s *Store) GetDeployment(ctx context.Context, id int64) (Deployment, error) {
	return scanDeployment(s.db.QueryRowContext(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE id = ?`, id))
}

// ListDeployments returns the newest deployments of an app.
func (s *Store) ListDeployments(ctx context.Context, appID int64, limit int) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE app_id = ? ORDER BY id DESC LIMIT ?`, appID, limit)
	if err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deployments: %w", err)
	}
	return out, nil
}

// HasActiveDeployment reports whether the app has a queued or running deployment.
func (s *Store) HasActiveDeployment(ctx context.Context, appID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM deployments WHERE app_id = ? AND status IN (?, ?)`,
		appID, StatusQueued, StatusRunning).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check active deployments: %w", err)
	}
	return n > 0, nil
}

// NextQueuedDeployment returns the oldest queued deployment, or ErrNotFound.
func (s *Store) NextQueuedDeployment(ctx context.Context) (Deployment, error) {
	return scanDeployment(s.db.QueryRowContext(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE status = ? ORDER BY id LIMIT 1`, StatusQueued))
}

// StartDeployment moves a queued deployment to running. It returns
// ErrInvalidTransition when the deployment is no longer queued (for example
// because it was canceled in the meantime).
func (s *Store) StartDeployment(ctx context.Context, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?, started_at = ? WHERE id = ? AND status = ?`,
		StatusRunning, toMillis(now), id, StatusQueued)
	if err != nil {
		return fmt.Errorf("start deployment: %w", err)
	}
	return requireTransition(res, "start deployment")
}

// FinishDeployment moves a running deployment to a terminal status. On
// success the app's current deployment is updated in the same transaction:
// to the deployment itself, or for a rollback to the release it started.
func (s *Store) FinishDeployment(ctx context.Context, id int64, status, errMsg string, now time.Time) error {
	if !CanTransition(StatusRunning, status) {
		return fmt.Errorf("finish deployment as %q: %w", status, ErrInvalidTransition)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish deployment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`UPDATE deployments SET status = ?, error = ?, finished_at = ? WHERE id = ? AND status = ?`,
		status, errMsg, toMillis(now), id, StatusRunning)
	if err != nil {
		return fmt.Errorf("finish deployment: %w", err)
	}
	if err := requireTransition(res, "finish deployment"); err != nil {
		return err
	}
	if status == StatusSucceeded {
		if _, err := tx.ExecContext(ctx,
			`UPDATE apps SET current_deployment_id = (SELECT COALESCE(source_deployment_id, id) FROM deployments WHERE id = ?),
			 updated_at = ? WHERE id = (SELECT app_id FROM deployments WHERE id = ?)`, id, toMillis(now), id); err != nil {
			return fmt.Errorf("finish deployment: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish deployment: %w", err)
	}
	return nil
}

// CancelQueuedDeployment cancels a deployment that has not started. It
// returns ErrInvalidTransition when the deployment is not queued.
func (s *Store) CancelQueuedDeployment(ctx context.Context, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?, error = ?, finished_at = ? WHERE id = ? AND status = ?`,
		StatusCanceled, "canceled before it started", toMillis(now), id, StatusQueued)
	if err != nil {
		return fmt.Errorf("cancel deployment: %w", err)
	}
	return requireTransition(res, "cancel deployment")
}

// FailInterruptedDeployments marks deployments left running by a previous
// process as failed. It is called once at startup.
func (s *Store) FailInterruptedDeployments(ctx context.Context, now time.Time) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM deployments WHERE status = ? ORDER BY id`, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("fail interrupted deployments: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("fail interrupted deployments: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("fail interrupted deployments: %w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET status = ?, error = ?, finished_at = ? WHERE status = ?`,
		StatusFailed, InterruptedError, toMillis(now), StatusRunning); err != nil {
		return nil, fmt.Errorf("fail interrupted deployments: %w", err)
	}
	return ids, nil
}

// SetDeploymentCommit records the commit details resolved after checkout.
func (s *Store) SetDeploymentCommit(ctx context.Context, id int64, sha, message string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET commit_sha = ?, commit_message = ? WHERE id = ?`, sha, message, id)
	if err != nil {
		return fmt.Errorf("set deployment commit: %w", err)
	}
	return requireOneRow(res, "set deployment commit")
}

// SucceededDeployments returns the newest succeeded deployments of an app.
func (s *Store) SucceededDeployments(ctx context.Context, appID int64, limit int) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+deploymentColumns+` FROM deployments WHERE app_id = ? AND status = ?
		 ORDER BY id DESC LIMIT ?`, appID, StatusSucceeded, limit)
	if err != nil {
		return nil, fmt.Errorf("list succeeded deployments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list succeeded deployments: %w", err)
	}
	return out, nil
}

func requireTransition(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, ErrInvalidTransition)
	}
	return nil
}

// SetDeploymentEnvFingerprints records the fingerprints of the environment
// variables a deployment rendered, keyed by variable name (ARCHITECTURE 9.2).
func (s *Store) SetDeploymentEnvFingerprints(ctx context.Context, id int64, fingerprints map[string]string) error {
	if fingerprints == nil {
		fingerprints = map[string]string{}
	}
	data, err := json.Marshal(fingerprints)
	if err != nil {
		return fmt.Errorf("set env fingerprints: %w", err)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE deployments SET env_fingerprints = ? WHERE id = ?`, string(data), id)
	if err != nil {
		return fmt.Errorf("set env fingerprints: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("set env fingerprints: %w", err)
	} else if n == 0 {
		return fmt.Errorf("set env fingerprints of deployment %d: %w", id, ErrNotFound)
	}
	return nil
}

// DeploymentEnvFingerprints returns what SetDeploymentEnvFingerprints
// recorded. ok is false when nothing was recorded for the deployment.
func (s *Store) DeploymentEnvFingerprints(ctx context.Context, id int64) (fingerprints map[string]string, ok bool, err error) {
	var data sql.NullString
	err = s.db.QueryRowContext(ctx, `SELECT env_fingerprints FROM deployments WHERE id = ?`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("env fingerprints of deployment %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return nil, false, fmt.Errorf("env fingerprints: %w", err)
	}
	if !data.Valid {
		return nil, false, nil
	}
	if err := json.Unmarshal([]byte(data.String), &fingerprints); err != nil {
		return nil, false, fmt.Errorf("env fingerprints of deployment %d: %w", id, err)
	}
	if fingerprints == nil {
		fingerprints = map[string]string{}
	}
	return fingerprints, true, nil
}
