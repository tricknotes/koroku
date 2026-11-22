package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// The fake plays the ComposeInspector: the current release and those in
// available are kept.
func (f *fakeDeployer) ComposeView(_ context.Context, app store.App, id int64) (deploy.ComposeView, error) {
	if err := f.record("compose", app); err != nil {
		return deploy.ComposeView{}, err
	}
	if app.CurrentDeploymentID == nil {
		return deploy.ComposeView{}, deploy.ErrNotDeployed
	}
	if id == 0 {
		id = *app.CurrentDeploymentID
	}
	f.mu.Lock()
	kept := f.available[id]
	f.mu.Unlock()
	if id != *app.CurrentDeploymentID && !kept {
		return deploy.ComposeView{}, deploy.ErrReleaseUnavailable
	}
	config := "services: {}\n"
	return deploy.ComposeView{
		DeploymentID: id, CurrentDeploymentID: app.CurrentDeploymentID,
		Override: "services: {}\n", Config: &config,
	}, nil
}

func (f *fakeDeployer) Containers(_ context.Context, app store.App) (deploy.ContainerView, error) {
	if err := f.record("containers", app); err != nil {
		return deploy.ContainerView{}, err
	}
	return deploy.ContainerView{}, nil
}

func TestComposeViewEndpoint(t *testing.T) {
	var fake *fakeDeployer
	srv := newTestServer(t, func(o *Options) { fake = o.Deployer.(*fakeDeployer) })
	h := srv.Handler()
	ctx := context.Background()
	createApp(t, h, "blog")

	rec := do(t, h, http.MethodGet, "/api/apps/blog/compose")
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "not_deployed" {
		t.Fatalf("before a deployment: %d %s", rec.Code, rec.Body)
	}

	app, err := srv.store.GetApp(ctx, "blog")
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := srv.store.EnqueueDeployment(ctx, app.ID, "main", testSHA, "first", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.StartDeployment(ctx, d.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.available = map[int64]bool{d.ID + 10: true}
	fake.mu.Unlock()
	rec = do(t, h, http.MethodGet, "/api/apps/blog/compose")
	if rec.Code != http.StatusOK {
		t.Fatalf("current: %d %s", rec.Code, rec.Body)
	}
	var view struct {
		DeploymentID int64   `json:"deployment_id"`
		Releases     []any   `json:"releases"`
		Config       *string `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.DeploymentID != d.ID || view.Releases == nil || view.Config == nil {
		t.Errorf("view = %s", rec.Body)
	}

	if rec := do(t, h, http.MethodGet, "/api/apps/blog/compose?deployment="+strconv.FormatInt(d.ID+10, 10)); rec.Code != http.StatusOK {
		t.Errorf("kept release: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodGet, "/api/apps/blog/compose?deployment="+strconv.FormatInt(d.ID+11, 10)); rec.Code != http.StatusConflict || errorCode(t, rec) != "release_unavailable" {
		t.Errorf("gone release: %d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"x", "0", "-1"} {
		if rec := do(t, h, http.MethodGet, "/api/apps/blog/compose?deployment="+bad); rec.Code != http.StatusBadRequest {
			t.Errorf("deployment=%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if rec := do(t, h, http.MethodGet, "/api/apps/nope/compose"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown app: %d", rec.Code)
	}
}

func TestContainersEndpoint(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")
	rec := do(t, h, http.MethodGet, "/api/apps/blog/containers")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var view struct {
		Containers []any `json:"containers"`
		Compared   bool  `json:"compared"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Containers == nil {
		t.Errorf("containers must be an array: %s", rec.Body)
	}
}

func TestComposeViewNotConfigured(t *testing.T) {
	h := newTestServer(t, func(o *Options) { o.Compose = nil }).Handler()
	createApp(t, h, "blog")
	for _, target := range []string{"/api/apps/blog/compose", "/api/apps/blog/containers"} {
		if rec := do(t, h, http.MethodGet, target); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: %d", target, rec.Code)
		}
	}
}
