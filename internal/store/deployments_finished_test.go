package store

import (
	"context"
	"testing"
	"time"
)

func TestListDeploymentsFinishedSince(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "shop")

	// run finishes a deployment at testNow + offset.
	run := func(appID int64, sha string, offset time.Duration, status string) Deployment {
		t.Helper()
		d := enqueue(t, s, appID, sha)
		if err := s.StartDeployment(ctx, d.ID, testNow); err != nil {
			t.Fatalf("StartDeployment: %v", err)
		}
		if err := s.FinishDeployment(ctx, d.ID, status, "", testNow.Add(offset)); err != nil {
			t.Fatalf("FinishDeployment: %v", err)
		}
		return d
	}
	run(app.ID, "old", time.Hour, StatusSucceeded)
	edge := run(app.ID, "edge", 2*time.Hour, StatusFailed)
	late := run(app.ID, "late", 3*time.Hour, StatusSucceeded)
	run(other.ID, "other", 3*time.Hour, StatusSucceeded)
	running := enqueue(t, s, app.ID, "running")
	if err := s.StartDeployment(ctx, running.ID, testNow); err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}

	got, err := s.ListDeploymentsFinishedSince(ctx, app.ID, testNow.Add(2*time.Hour), 10)
	if err != nil {
		t.Fatalf("ListDeploymentsFinishedSince: %v", err)
	}
	if len(got) != 2 || got[0].ID != late.ID || got[1].ID != edge.ID {
		t.Errorf("finished since = %+v, want #%d and #%d (newest first, the boundary included, unfinished left out)", got, late.ID, edge.ID)
	}
	limited, err := s.ListDeploymentsFinishedSince(ctx, app.ID, testNow, 1)
	if err != nil || len(limited) != 1 || limited[0].ID != late.ID {
		t.Errorf("limited = %+v, %v", limited, err)
	}
}

func TestDeploymentContainersReplaced(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	d := enqueue(t, s, app.ID, "aaa")
	if got, _ := s.GetDeployment(ctx, d.ID); got.ContainersReplaced != nil {
		t.Fatalf("a deployment that has not run = %v, want nil", *got.ContainersReplaced)
	}
	for _, want := range []bool{false, true} {
		if err := s.SetDeploymentContainersReplaced(ctx, d.ID, want); err != nil {
			t.Fatalf("SetDeploymentContainersReplaced: %v", err)
		}
		got, err := s.GetDeployment(ctx, d.ID)
		if err != nil || got.ContainersReplaced == nil || *got.ContainersReplaced != want {
			t.Errorf("containers replaced = %v, %v, want %v", got.ContainersReplaced, err, want)
		}
	}
}
