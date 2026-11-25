package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GitHubDeployment links a koroku deployment to the deployment recorded on
// GitHub (ARCHITECTURE 5.1).
type GitHubDeployment struct {
	DeploymentID int64
	// GitHubID is GitHub's deployment ID.
	GitHubID int64
	// State is the last status of the deployment's own progress that GitHub
	// accepted ("" before the first one). Retiring a replaced deployment on
	// GitHub (inactive) does not change it.
	State string
}

// GetGitHubDeployment returns the GitHub deployment of a koroku deployment,
// or ErrNotFound when none was created.
func (s *Store) GetGitHubDeployment(ctx context.Context, deploymentID int64) (GitHubDeployment, error) {
	g := GitHubDeployment{DeploymentID: deploymentID}
	err := s.db.QueryRowContext(ctx,
		`SELECT github_id, state FROM github_deployments WHERE deployment_id = ?`, deploymentID,
	).Scan(&g.GitHubID, &g.State)
	if errors.Is(err, sql.ErrNoRows) {
		return GitHubDeployment{}, ErrNotFound
	}
	if err != nil {
		return GitHubDeployment{}, fmt.Errorf("get github deployment: %w", err)
	}
	return g, nil
}

// SaveGitHubDeployment records the GitHub deployment created for a koroku
// deployment, before any status is sent.
func (s *Store) SaveGitHubDeployment(ctx context.Context, deploymentID, githubID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO github_deployments (deployment_id, github_id, state, updated_at) VALUES (?, ?, '', ?)
		 ON CONFLICT (deployment_id) DO UPDATE SET github_id = excluded.github_id, state = '',
		   updated_at = excluded.updated_at`,
		deploymentID, githubID, toMillis(now))
	if err != nil {
		return fmt.Errorf("save github deployment: %w", err)
	}
	return nil
}

// SetGitHubDeploymentState records the status GitHub last accepted.
func (s *Store) SetGitHubDeploymentState(ctx context.Context, deploymentID int64, state string, now time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE github_deployments SET state = ?, updated_at = ? WHERE deployment_id = ?`,
		state, toMillis(now), deploymentID)
	if err != nil {
		return fmt.Errorf("set github deployment state: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// NewestSucceededGitHubDeployment returns GitHub's ID of the app's newest
// deployment whose success reached GitHub, or ErrNotFound when there is none.
func (s *Store) NewestSucceededGitHubDeployment(ctx context.Context, appID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT g.github_id FROM github_deployments g JOIN deployments d ON d.id = g.deployment_id
		 WHERE d.app_id = ? AND g.state = 'success'
		 ORDER BY d.id DESC LIMIT 1`, appID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("get newest succeeded github deployment: %w", err)
	}
	return id, nil
}

// StaleGitHubDeployments lists koroku deployments that finished at or after
// since but whose GitHub deployment has no final status yet (koroku stopped,
// or GitHub failed, before it was sent), newest first.
func (s *Store) StaleGitHubDeployments(ctx context.Context, since time.Time, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT d.id FROM deployments d JOIN github_deployments g ON g.deployment_id = d.id
		 WHERE d.status IN (?, ?, ?) AND g.state NOT IN ('success', 'failure', 'error')
		   AND d.finished_at >= ?
		 ORDER BY d.id DESC LIMIT ?`,
		StatusSucceeded, StatusFailed, StatusCanceled, toMillis(since), limit)
	if err != nil {
		return nil, fmt.Errorf("list stale github deployments: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list stale github deployments: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list stale github deployments: %w", err)
	}
	return ids, nil
}
