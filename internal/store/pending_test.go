package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPendingAutoDeployRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "shop")
	at := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	shaA, shaB := strings.Repeat("a", 40), strings.Repeat("b", 40)

	if _, err := s.GetPendingAutoDeploy(ctx, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("none yet: %v", err)
	}
	for i, sha := range []string{shaA, shaB} {
		if err := s.PutPendingAutoDeploy(ctx, PendingAutoDeploy{
			AppID: app.ID, Ref: "main", CommitSHA: sha, Pusher: "octocat",
			Workflow: ".github/workflows/image.yml", ReceivedAt: at.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutPendingAutoDeploy(ctx, PendingAutoDeploy{AppID: other.ID, Ref: "main", CommitSHA: shaA, ReceivedAt: at}); err != nil {
		t.Fatal(err)
	}
	// The later push replaced the earlier one.
	got, err := s.GetPendingAutoDeploy(ctx, app.ID)
	if err != nil || got.CommitSHA != shaB || got.Workflow != ".github/workflows/image.yml" || !got.ReceivedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("pending = %+v, %v", got, err)
	}
	list, err := s.ListPendingAutoDeploys(ctx)
	if err != nil || len(list) != 2 || list[0].AppID != other.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}

	// Queueing any deployment of the app discards what was kept for it.
	if _, _, err := s.EnqueueTriggered(ctx, app.ID, "main", shaB, "", nil, Origin{}, at); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetPendingAutoDeploy(ctx, app.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after a deployment: %v", err)
	}
	// It goes with the app.
	if err := s.DeleteApp(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if list, err := s.ListPendingAutoDeploys(ctx); err != nil || len(list) != 0 {
		t.Errorf("after deleting the app: %+v, %v", list, err)
	}
	if err := s.DeletePendingAutoDeploy(ctx, app.ID); err != nil {
		t.Errorf("deleting nothing: %v", err)
	}
}
