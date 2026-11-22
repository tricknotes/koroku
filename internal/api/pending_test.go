package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// TestListDeploymentsShowsTheDeferredAutoDeploy covers pending_auto_deploy
// (ARCHITECTURE 5.2): null without one, the kept push otherwise.
func TestListDeploymentsShowsTheDeferredAutoDeploy(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.Handler()
	createApp(t, h, "blog")
	app, err := s.store.GetApp(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}

	rec := request(t, h, http.MethodGet, "/api/apps/blog/deployments", "")
	if !strings.Contains(rec.Body.String(), `"pending_auto_deploy":null`) {
		t.Errorf("without a kept push: %s", rec.Body)
	}

	received := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	if err := s.store.PutPendingAutoDeploy(context.Background(), store.PendingAutoDeploy{
		AppID: app.ID, Ref: "main", CommitSHA: testSHA, CommitMessage: "Fix the footer",
		Pusher: "octocat", ReceivedAt: received,
	}); err != nil {
		t.Fatal(err)
	}
	got := decode[struct {
		Pending *pendingAutoDeployJSON `json:"pending_auto_deploy"`
	}](t, request(t, h, http.MethodGet, "/api/apps/blog/deployments", ""))
	if p := got.Pending; p == nil || p.CommitSHA != testSHA || p.Ref != "main" || p.Pusher != "octocat" ||
		p.CommitMessage != "Fix the footer" || p.Workflow != "" || !p.ReceivedAt.Equal(received) {
		t.Errorf("pending = %+v", got.Pending)
	}
}
