package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// finishDeployment runs a queued deployment to status at the given time,
// recording whether it replaced the containers.
func finishDeployment(t *testing.T, st *store.Store, appID int64, sha string, at time.Time, status string, replaced bool) store.Deployment {
	t.Helper()
	ctx := context.Background()
	d, _, err := st.EnqueueDeployment(ctx, appID, "main", sha, "", at.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.StartDeployment(ctx, d.ID, at.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentContainersReplaced(ctx, d.ID, replaced); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishDeployment(ctx, d.ID, status, "", at); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestListDeploymentsFinishedSince covers the filter the memory history
// reads its markers with (ARCHITECTURE 9.5), and containers_replaced.
func TestListDeploymentsFinishedSince(t *testing.T) {
	s := newTestServer(t, nil)
	h := s.Handler()
	createApp(t, h, "blog")
	app, err := s.store.GetApp(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	finishDeployment(t, s.store, app.ID, testSHA, base, store.StatusSucceeded, true)
	failed := finishDeployment(t, s.store, app.ID, testSHA, base.Add(3*time.Hour), store.StatusFailed, true)
	built := finishDeployment(t, s.store, app.ID, testSHA, base.Add(4*time.Hour), store.StatusFailed, false)
	// Queued only: not finished, so not listed.
	if _, _, err := s.store.EnqueueDeployment(context.Background(), app.ID, "main", testSHA, "", base.Add(5*time.Hour)); err != nil {
		t.Fatal(err)
	}

	got := decode[struct{ Deployments []deploymentJSON }](t,
		request(t, h, http.MethodGet, "/api/apps/blog/deployments?finished_since=2026-11-17T10:00:00%2B09:00&limit=200", ""))
	if len(got.Deployments) != 2 || got.Deployments[0].ID != built.ID || got.Deployments[1].ID != failed.ID {
		t.Fatalf("finished since 01:00Z (10:00 JST) = %+v", got.Deployments)
	}
	if r := got.Deployments[1].ContainersReplaced; r == nil || !*r {
		t.Errorf("a failed deployment that rolled back: containers_replaced = %v", r)
	}
	if r := got.Deployments[0].ContainersReplaced; r == nil || *r {
		t.Errorf("a failed build: containers_replaced = %v", r)
	}

	all := request(t, h, http.MethodGet, "/api/apps/blog/deployments", "")
	if !strings.Contains(all.Body.String(), `"containers_replaced":null`) {
		t.Errorf("a queued deployment has containers_replaced null: %s", all.Body)
	}

	for _, bad := range []string{"yesterday", "2026-11-17"} {
		rec := request(t, h, http.MethodGet, "/api/apps/blog/deployments?finished_since="+bad, "")
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_finished_since") {
			t.Errorf("finished_since=%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
}

// A member who may only look at the app reads the markers too.
func TestViewersListDeploymentsFinishedSince(t *testing.T) {
	f := newRoleFixture(t)
	blog := f.app(t, "blog")
	finishDeployment(t, f.srv.store, blog.ID, testSHA, f.clk.Now().Add(-time.Hour), store.StatusSucceeded, true)
	rec := f.as[viewerID].do(http.MethodGet, "/api/apps/blog/deployments?finished_since=2026-11-17T03:00:00Z&limit=200", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"containers_replaced":true`) {
		t.Errorf("viewer: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as[strangerID].do(http.MethodGet, "/api/apps/blog/deployments?finished_since=2026-11-17T03:00:00Z", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("a member without a role on blog: %d", rec.Code)
	}
}
