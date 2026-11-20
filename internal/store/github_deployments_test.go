package store

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestGitHubDeployments(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	d := enqueue(t, s, app.ID, "aaa")

	if _, err := s.GetGitHubDeployment(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetGitHubDeployment before saving = %v, want ErrNotFound", err)
	}
	if err := s.SetGitHubDeploymentState(ctx, d.ID, "queued", testNow); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetGitHubDeploymentState without a row = %v, want ErrNotFound", err)
	}
	if err := s.SaveGitHubDeployment(ctx, d.ID, 991, testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGitHubDeploymentState(ctx, d.ID, "in_progress", testNow); err != nil {
		t.Fatal(err)
	}
	g, err := s.GetGitHubDeployment(ctx, d.ID)
	if err != nil || g.GitHubID != 991 || g.State != "in_progress" {
		t.Fatalf("GetGitHubDeployment = %+v, %v", g, err)
	}

	// Running deployments are not stale; a finished one without a final
	// status is.
	stale, err := s.StaleGitHubDeployments(ctx, testNow.Add(-time.Hour), 10)
	if err != nil || len(stale) != 0 {
		t.Fatalf("StaleGitHubDeployments while queued = %v, %v", stale, err)
	}
	if err := s.StartDeployment(ctx, d.ID, testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeployment(ctx, d.ID, StatusFailed, "boom", testNow); err != nil {
		t.Fatal(err)
	}
	stale, err = s.StaleGitHubDeployments(ctx, testNow.Add(-time.Hour), 10)
	if err != nil || !slices.Equal(stale, []int64{d.ID}) {
		t.Fatalf("StaleGitHubDeployments = %v, %v", stale, err)
	}
	if stale, _ := s.StaleGitHubDeployments(ctx, testNow.Add(time.Hour), 10); len(stale) != 0 {
		t.Errorf("deployments finished before since are listed: %v", stale)
	}
	if err := s.SetGitHubDeploymentState(ctx, d.ID, "failure", testNow); err != nil {
		t.Fatal(err)
	}
	if stale, _ := s.StaleGitHubDeployments(ctx, testNow.Add(-time.Hour), 10); len(stale) != 0 {
		t.Errorf("deployments with a final status are listed: %v", stale)
	}

	// Deleting the app removes the link with the deployment.
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM apps WHERE id = ?`, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGitHubDeployment(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetGitHubDeployment after deleting the app = %v", err)
	}
}
