package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// States of a review app's pull request (ARCHITECTURE 9.9).
const (
	PRStateOpen   = "open"
	PRStateClosed = "closed"
	PRStateMerged = "merged"
)

// States of the copy of the parent's data into a review app.
const (
	// DataCopyNone: no copy was asked for.
	DataCopyNone = ""
	// DataCopyWaiting: the copy starts once the first deployment succeeded.
	DataCopyWaiting = "waiting"
	// DataCopyStarted: the copy was started; its record is in db_copies.
	DataCopyStarted = "started"
	// DataCopySkipped: the parent has no dump or load hook.
	DataCopySkipped = "skipped"
	// DataCopyFailed: the copy could not be started.
	DataCopyFailed = "failed"
)

var (
	// ErrReviewAppExists is returned when the parent already has a review
	// app of the pull request.
	ErrReviewAppExists = errors.New("the pull request already has a review app")
	// ErrRoleFollowsParent is returned when a role is set on a review app:
	// its roles are the parent's.
	ErrRoleFollowsParent = errors.New("the roles of a review app are those of its parent")
)

// ReviewApp is what makes an app a review app: the parent it was made
// from and the pull request it runs.
type ReviewApp struct {
	AppID       int64
	ParentAppID int64
	PRNumber    int
	PRTitle     string
	HeadBranch  string
	PRState     string
	// PRCheckedAt is when the pull request was last read from GitHub.
	PRCheckedAt    *time.Time
	DataCopy       string
	DataCopyDetail string
	CreatedAt      time.Time
}

// NewReviewApp is everything CreateReviewApp writes. Env and Registry
// return the parent's variables and registry login sealed for the new
// app's ID, which is known only inside the transaction.
type NewReviewApp struct {
	App      App
	Review   ReviewApp
	Env      func(appID int64) ([]EnvVar, error)
	Registry func(appID int64) (*RegistryCredential, error)
}

// CreateReviewApp creates a review app with its variables, registry login
// and review row in one transaction, so that it never exists without what
// it was copied from its parent. It returns ErrConflict when the slug is
// taken and ErrReviewAppExists when the pull request has a review app.
func (s *Store) CreateReviewApp(ctx context.Context, in NewReviewApp, portStart, portEnd int, now time.Time) (App, error) {
	return s.createApp(ctx, in.App, portStart, portEnd, now, in.Env, func(tx *sql.Tx, appID int64) error {
		if in.Registry != nil {
			cred, err := in.Registry(appID)
			if err != nil {
				return fmt.Errorf("create review app: %w", err)
			}
			if cred != nil {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO registry_credentials (app_id, registry, username, token_encrypted, updated_at)
					 VALUES (?, ?, ?, ?, ?)`,
					appID, cred.Registry, cred.Username, cred.TokenEncrypted, toMillis(now)); err != nil {
					return fmt.Errorf("create review app: registry credential: %w", err)
				}
			}
		}
		r := in.Review
		if r.PRState == "" {
			r.PRState = PRStateOpen
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO review_apps (app_id, parent_app_id, pr_number, pr_title, head_branch, pr_state,
			   pr_checked_at, data_copy, data_copy_detail, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			appID, r.ParentAppID, r.PRNumber, r.PRTitle, r.HeadBranch, r.PRState,
			toMillis(now), r.DataCopy, r.DataCopyDetail, toMillis(now)); err != nil {
			if isUniqueViolation(err) {
				return ErrReviewAppExists
			}
			return fmt.Errorf("create review app: %w", err)
		}
		return nil
	})
}

const reviewColumns = `app_id, parent_app_id, pr_number, pr_title, head_branch, pr_state,
	pr_checked_at, data_copy, data_copy_detail, created_at`

func scanReviewApp(row rowScanner) (ReviewApp, error) {
	var (
		r       ReviewApp
		checked sql.NullInt64
		created int64
	)
	if err := row.Scan(&r.AppID, &r.ParentAppID, &r.PRNumber, &r.PRTitle, &r.HeadBranch, &r.PRState,
		&checked, &r.DataCopy, &r.DataCopyDetail, &created); err != nil {
		return ReviewApp{}, err
	}
	if checked.Valid {
		t := fromMillis(checked.Int64)
		r.PRCheckedAt = &t
	}
	r.CreatedAt = fromMillis(created)
	return r, nil
}

// GetReviewApp returns the review row of an app, or ErrNotFound when the
// app is not a review app.
func (s *Store) GetReviewApp(ctx context.Context, appID int64) (ReviewApp, error) {
	r, err := scanReviewApp(s.db.QueryRowContext(ctx,
		`SELECT `+reviewColumns+` FROM review_apps WHERE app_id = ?`, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewApp{}, fmt.Errorf("review app %d: %w", appID, ErrNotFound)
	}
	if err != nil {
		return ReviewApp{}, fmt.Errorf("get review app: %w", err)
	}
	return r, nil
}

// ListReviewApps returns the review apps of a parent, newest pull request
// first. parentID 0 lists every review app.
func (s *Store) ListReviewApps(ctx context.Context, parentID int64) ([]ReviewApp, error) {
	query := `SELECT ` + reviewColumns + ` FROM review_apps`
	var args []any
	if parentID != 0 {
		query += ` WHERE parent_app_id = ?`
		args = append(args, parentID)
	}
	rows, err := s.db.QueryContext(ctx, query+` ORDER BY parent_app_id, pr_number DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("list review apps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ReviewApp
	for rows.Next() {
		r, err := scanReviewApp(rows)
		if err != nil {
			return nil, fmt.Errorf("list review apps: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list review apps: %w", err)
	}
	return out, nil
}

// SetReviewDataCopy records the state of the copy of the parent's data.
func (s *Store) SetReviewDataCopy(ctx context.Context, appID int64, state, detail string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE review_apps SET data_copy = ?, data_copy_detail = ? WHERE app_id = ?`, state, detail, appID)
	if err != nil {
		return fmt.Errorf("set review data copy: %w", err)
	}
	return requireOneRow(res, "set review data copy")
}

// UpdateReviewPullRequest records what GitHub said about the pull request.
func (s *Store) UpdateReviewPullRequest(ctx context.Context, appID int64, title, headBranch, state string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE review_apps SET pr_title = ?, head_branch = ?, pr_state = ?, pr_checked_at = ? WHERE app_id = ?`,
		title, headBranch, state, toMillis(now), appID)
	if err != nil {
		return fmt.Errorf("update review pull request: %w", err)
	}
	return requireOneRow(res, "update review pull request")
}

// isReviewApp reports whether an app is a review app.
func isReviewApp(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, appID int64) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM review_apps WHERE app_id = ?`, appID).Scan(&n); err != nil {
		return false, err
	}
	return n > 0, nil
}

// ReviewSummary counts the review apps of a parent (ARCHITECTURE 9.9).
type ReviewSummary struct {
	Count int
	// Failed counts the review apps whose latest deployment failed or
	// whose copy of the parent's data failed.
	Failed int
	// Deploying counts the review apps with a deployment queued or
	// running.
	Deploying int
}

// SummarizeReviewApps counts the review apps of a parent from the
// database alone: the containers are not asked about.
func (s *Store) SummarizeReviewApps(ctx context.Context, parentID int64) (ReviewSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
			COALESCE((SELECT status FROM deployments d WHERE d.app_id = ra.app_id ORDER BY d.id DESC LIMIT 1), ''),
			ra.data_copy,
			COALESCE((SELECT status FROM db_copies c WHERE c.target_app_id = ra.app_id
				AND c.source_app_id = ra.parent_app_id ORDER BY c.id DESC LIMIT 1), '')
		FROM review_apps ra WHERE ra.parent_app_id = ?`, parentID)
	if err != nil {
		return ReviewSummary{}, fmt.Errorf("summarize review apps: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out ReviewSummary
	for rows.Next() {
		var deployment, dataCopy, copyStatus string
		if err := rows.Scan(&deployment, &dataCopy, &copyStatus); err != nil {
			return ReviewSummary{}, fmt.Errorf("summarize review apps: %w", err)
		}
		out.Count++
		if deployment == StatusFailed || dataCopy == DataCopyFailed || copyStatus == CopyFailed {
			out.Failed++
		}
		if deployment == StatusQueued || deployment == StatusRunning {
			out.Deploying++
		}
	}
	if err := rows.Err(); err != nil {
		return ReviewSummary{}, fmt.Errorf("summarize review apps: %w", err)
	}
	return out, nil
}

// LatestCopyStatus returns the status of the newest copy of source's
// database into target, or "" when there is none.
func (s *Store) LatestCopyStatus(ctx context.Context, targetID, sourceID int64) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, `SELECT status FROM db_copies
		WHERE target_app_id = ? AND source_app_id = ? ORDER BY id DESC LIMIT 1`, targetID, sourceID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("latest copy status: %w", err)
	}
	return status, nil
}
