package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// PendingAutoDeploy is an automatic deployment received while a command ran
// on the app, kept until the command ends (ARCHITECTURE 5.2).
type PendingAutoDeploy struct {
	AppID         int64
	Ref           string
	CommitSHA     string
	CommitMessage string
	Pusher        string
	// Workflow is the workflow the push waited for, or "".
	Workflow   string
	ReceivedAt time.Time
}

const pendingColumns = `app_id, ref, commit_sha, commit_message, pusher, workflow, received_at`

func scanPending(row interface{ Scan(...any) error }) (PendingAutoDeploy, error) {
	var p PendingAutoDeploy
	var received int64
	if err := row.Scan(&p.AppID, &p.Ref, &p.CommitSHA, &p.CommitMessage, &p.Pusher, &p.Workflow, &received); err != nil {
		return PendingAutoDeploy{}, err
	}
	p.ReceivedAt = fromMillis(received)
	return p, nil
}

// PutPendingAutoDeploy keeps p for its app, replacing the one kept before:
// the later push wins.
func (s *Store) PutPendingAutoDeploy(ctx context.Context, p PendingAutoDeploy) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO pending_auto_deploys (`+pendingColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (app_id) DO UPDATE SET
		   ref = excluded.ref,
		   commit_sha = excluded.commit_sha,
		   commit_message = excluded.commit_message,
		   pusher = excluded.pusher,
		   workflow = excluded.workflow,
		   received_at = excluded.received_at`,
		p.AppID, p.Ref, p.CommitSHA, p.CommitMessage, p.Pusher, p.Workflow, toMillis(p.ReceivedAt)); err != nil {
		return fmt.Errorf("keep pending auto-deploy: %w", err)
	}
	return nil
}

// GetPendingAutoDeploy returns the app's pending auto-deploy, or ErrNotFound.
func (s *Store) GetPendingAutoDeploy(ctx context.Context, appID int64) (PendingAutoDeploy, error) {
	p, err := scanPending(s.db.QueryRowContext(ctx,
		`SELECT `+pendingColumns+` FROM pending_auto_deploys WHERE app_id = ?`, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return PendingAutoDeploy{}, fmt.Errorf("pending auto-deploy of app %d: %w", appID, ErrNotFound)
	}
	if err != nil {
		return PendingAutoDeploy{}, fmt.Errorf("get pending auto-deploy: %w", err)
	}
	return p, nil
}

// ListPendingAutoDeploys returns every pending auto-deploy, oldest first.
func (s *Store) ListPendingAutoDeploys(ctx context.Context) ([]PendingAutoDeploy, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+pendingColumns+` FROM pending_auto_deploys ORDER BY received_at, app_id`)
	if err != nil {
		return nil, fmt.Errorf("list pending auto-deploys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PendingAutoDeploy
	for rows.Next() {
		p, err := scanPending(rows)
		if err != nil {
			return nil, fmt.Errorf("list pending auto-deploys: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePendingAutoDeploy forgets the app's pending auto-deploy, if any.
func (s *Store) DeletePendingAutoDeploy(ctx context.Context, appID int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pending_auto_deploys WHERE app_id = ?`, appID); err != nil {
		return fmt.Errorf("delete pending auto-deploy: %w", err)
	}
	return nil
}
