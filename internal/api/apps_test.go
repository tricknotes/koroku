package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

const testSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// fakeDeployer records deployments in the store without running them.
type fakeDeployer struct {
	store  *store.Store
	logDir string

	mu         sync.Mutex
	enqueueErr error
	cancelErr  error
	canceled   []int64
	opErr      error
	ops        []string
	services   []deploy.ServiceStatus
}

func (f *fakeDeployer) Enqueue(ctx context.Context, app store.App, ref, sha, message string) (store.Deployment, error) {
	f.mu.Lock()
	err := f.enqueueErr
	f.mu.Unlock()
	if err != nil {
		return store.Deployment{}, err
	}
	d, _, err := f.store.EnqueueDeployment(ctx, app.ID, ref, sha, message, time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC))
	return d, err
}

func (f *fakeDeployer) Cancel(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.canceled = append(f.canceled, id)
	return nil
}

// The fake also plays the AppOperator.

func (f *fakeDeployer) record(op string, app store.App) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op+":"+app.Slug)
	return f.opErr
}

func (f *fakeDeployer) Start(ctx context.Context, app store.App) error {
	if err := f.record("start", app); err != nil {
		return err
	}
	return f.store.SetAppDesiredState(ctx, app.ID, store.DesiredRunning, time.Now())
}

func (f *fakeDeployer) Stop(ctx context.Context, app store.App) error {
	if err := f.record("stop", app); err != nil {
		return err
	}
	return f.store.SetAppDesiredState(ctx, app.ID, store.DesiredStopped, time.Now())
}

func (f *fakeDeployer) Restart(_ context.Context, app store.App) error {
	return f.record("restart", app)
}

func (f *fakeDeployer) Delete(ctx context.Context, app store.App, removeVolumes bool) error {
	if err := f.record(fmt.Sprintf("delete(volumes=%v)", removeVolumes), app); err != nil {
		return err
	}
	return f.store.DeleteApp(ctx, app.ID)
}

func (f *fakeDeployer) Services(_ context.Context, app store.App) ([]deploy.ServiceStatus, error) {
	if err := f.record("services", app); err != nil {
		return nil, err
	}
	return f.services, nil
}

func (f *fakeDeployer) Prune(_ context.Context, images, builder bool) (deploy.PruneResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, fmt.Sprintf("prune(images=%v,builder=%v)", images, builder))
	return deploy.PruneResult{Output: "Total reclaimed space: 12MB"}, f.opErr
}

func (f *fakeDeployer) LogPath(slug string, id int64) string {
	return filepath.Join(f.logDir, fmt.Sprintf("%s-%d.log", slug, id))
}

type fakeRefs map[string]string

func (f fakeRefs) ResolveRef(_ context.Context, _ store.App, ref string) (string, string, error) {
	sha, ok := f[ref]
	if !ok {
		return "", "", errors.New("ref not found: secret-remote-detail")
	}
	return sha, "Resolved commit", nil
}

func request(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return v
}

func createApp(t *testing.T, h http.Handler, slug string) appJSON {
	t.Helper()
	rec := request(t, h, http.MethodPost, "/api/apps",
		fmt.Sprintf(`{"slug": %q, "repo_full_name": "octo/%s", "web_port": 3000}`, slug, slug))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create app: status = %d, body = %s", rec.Code, rec.Body)
	}
	return decode[appJSON](t, rec)
}

func TestCreateAppAppliesDefaults(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	app := createApp(t, h, "blog")

	if app.Slug != "blog" || app.HostPort != 20000 || app.URL != "https://blog.dev.example.com" {
		t.Errorf("app = %+v", app)
	}
	if app.DefaultBranch != "main" || app.WebService != "web" || app.HealthcheckPath != "/" ||
		app.Environment != "staging" || app.DesiredState != "running" || app.RelaxPolicy {
		t.Errorf("defaults = %+v", app)
	}
	if app.CurrentDeployment != nil || app.LatestDeployment != nil || app.ServiceMemoryLimits == nil {
		t.Errorf("unexpected deployment fields: %+v", app)
	}

	second := createApp(t, h, "shop")
	if second.HostPort != 20001 {
		t.Errorf("second host port = %d", second.HostPort)
	}

	list := decode[struct{ Apps []appJSON }](t, request(t, h, http.MethodGet, "/api/apps", ""))
	if len(list.Apps) != 2 || list.Apps[0].Slug != "blog" {
		t.Errorf("list = %+v", list)
	}
	got := request(t, h, http.MethodGet, "/api/apps/blog", "")
	if got.Code != http.StatusOK || decode[appJSON](t, got).Slug != "blog" {
		t.Errorf("get: %d %s", got.Code, got.Body)
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/missing", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing app status = %d", rec.Code)
	}
}

func TestCreateAppWithAllFields(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	rec := request(t, h, http.MethodPost, "/api/apps", `{
		"slug": "shop", "repo_url": "https://example.com/git/shop.git", "default_branch": "release/1.x",
		"compose_path": "deploy/compose.yaml", "web_service": "app", "web_port": 8080,
		"healthcheck_path": "/up", "environment": "production", "relax_policy": true,
		"memory_limit": "1G", "service_memory_limits": {"db": "2g"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	app := decode[appJSON](t, rec)
	if app.RepoURL != "https://example.com/git/shop.git" || app.DefaultBranch != "release/1.x" ||
		app.ComposePath != "deploy/compose.yaml" || app.WebService != "app" || app.WebPort != 8080 ||
		app.HealthcheckPath != "/up" || app.Environment != "production" || !app.RelaxPolicy ||
		app.MemoryLimit != "1g" || app.ServiceMemoryLimits["db"] != "2g" {
		t.Errorf("app = %+v", app)
	}
}

func TestCreateAppValidation(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	cases := map[string]struct {
		body  string
		field string
	}{
		"missing slug":       {`{"repo_full_name": "octo/app", "web_port": 3000}`, "slug"},
		"uppercase slug":     {`{"slug": "Blog", "repo_full_name": "octo/app", "web_port": 3000}`, "slug"},
		"slug with dot":      {`{"slug": "a.b", "repo_full_name": "octo/app", "web_port": 3000}`, "slug"},
		"leading hyphen":     {`{"slug": "-a", "repo_full_name": "octo/app", "web_port": 3000}`, "slug"},
		"trailing hyphen":    {`{"slug": "a-", "repo_full_name": "octo/app", "web_port": 3000}`, "slug"},
		"reserved slug":      {`{"slug": "console", "repo_full_name": "octo/app", "web_port": 3000}`, "slug"},
		"too long slug":      {`{"slug": "` + strings.Repeat("a", 41) + `", "repo_full_name": "octo/app", "web_port": 3000}`, "slug"},
		"no repository":      {`{"slug": "blog", "web_port": 3000}`, "repo_full_name"},
		"bad repo name":      {`{"slug": "blog", "repo_full_name": "octo/app/../x", "web_port": 3000}`, "repo_full_name"},
		"ssh repo url":       {`{"slug": "blog", "repo_url": "git@github.com:octo/app.git", "web_port": 3000}`, "repo_url"},
		"file repo url":      {`{"slug": "blog", "repo_url": "file:///srv/repo", "web_port": 3000}`, "repo_url"},
		"ext repo url":       {`{"slug": "blog", "repo_url": "ext::sh -c id", "web_port": 3000}`, "repo_url"},
		"missing port":       {`{"slug": "blog", "repo_full_name": "octo/app"}`, "web_port"},
		"port out of range":  {`{"slug": "blog", "repo_full_name": "octo/app", "web_port": 70000}`, "web_port"},
		"bad branch":         {`{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000, "default_branch": "--upload-pack=x"}`, "default_branch"},
		"compose escapes":    {`{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000, "compose_path": "../compose.yaml"}`, "compose_path"},
		"absolute compose":   {`{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000, "compose_path": "/etc/compose.yaml"}`, "compose_path"},
		"bad service":        {`{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000, "web_service": "we b"}`, "web_service"},
		"bad health path":    {`{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000, "healthcheck_path": "up"}`, "healthcheck_path"},
		"bad environment":    {`{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000, "environment": "prod"}`, "environment"},
		"bad memory":         {`{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000, "memory_limit": "lots"}`, "memory_limit"},
		"bad service memory": {`{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000, "service_memory_limits": {"db": "0"}}`, "service_memory_limits"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := request(t, h, http.MethodPost, "/api/apps", tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
			}
			body := decode[struct {
				Error struct {
					Code    string
					Details map[string]string
				}
			}](t, rec)
			if body.Error.Code != "validation_failed" || body.Error.Details[tc.field] == "" {
				t.Errorf("expected an error on %q, got %s", tc.field, rec.Body)
			}
		})
	}
	list := decode[struct{ Apps []appJSON }](t, request(t, h, http.MethodGet, "/api/apps", ""))
	if len(list.Apps) != 0 {
		t.Errorf("invalid requests must not create apps: %+v", list.Apps)
	}
}

func TestCreateAppRejectsMalformedBodies(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	cases := map[string]string{
		"not json":      `slug=blog`,
		"unknown field": `{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000, "host_port": 80}`,
		"wrong type":    `{"slug": "blog", "repo_full_name": "octo/app", "web_port": "3000"}`,
		"two values":    `{"slug": "blog", "repo_full_name": "octo/app", "web_port": 3000} {}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := request(t, h, http.MethodPost, "/api/apps", body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, body = %s", rec.Code, rec.Body)
			}
		})
	}
	huge := `{"slug": "` + strings.Repeat("a", maxBodyBytes) + `"}`
	if rec := request(t, h, http.MethodPost, "/api/apps", huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d", rec.Code)
	}
}

func TestCreateAppConflicts(t *testing.T) {
	h := newTestServer(t, func(o *Options) {
		o.Config.Deploy.PortRangeStart, o.Config.Deploy.PortRangeEnd = 20000, 20000
	}).Handler()
	createApp(t, h, "blog")

	dup := request(t, h, http.MethodPost, "/api/apps", `{"slug": "blog", "repo_full_name": "octo/blog", "web_port": 3000}`)
	if dup.Code != http.StatusConflict || !strings.Contains(dup.Body.String(), "slug_taken") {
		t.Errorf("duplicate slug: %d %s", dup.Code, dup.Body)
	}
	full := request(t, h, http.MethodPost, "/api/apps", `{"slug": "shop", "repo_full_name": "octo/shop", "web_port": 3000}`)
	if full.Code != http.StatusConflict || !strings.Contains(full.Body.String(), "no_free_port") {
		t.Errorf("exhausted ports: %d %s", full.Code, full.Body)
	}
}

func TestFileRepoURLNeedsOptIn(t *testing.T) {
	h := newTestServer(t, func(o *Options) { o.Config.Deploy.AllowFileRepos = true }).Handler()
	rec := request(t, h, http.MethodPost, "/api/apps", `{"slug": "local", "repo_url": "file:///srv/repo", "web_port": 3000}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body)
	}
}

func TestUpdateApp(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")

	rec := request(t, h, http.MethodPatch, "/api/apps/blog",
		`{"web_port": 8080, "environment": "production", "service_memory_limits": {"db": "1g"}, "healthcheck_path": "/up"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	app := decode[appJSON](t, rec)
	if app.WebPort != 8080 || app.Environment != "production" || app.ServiceMemoryLimits["db"] != "1g" || app.HealthcheckPath != "/up" {
		t.Errorf("app = %+v", app)
	}
	// Untouched fields keep their values.
	if app.RepoFullName != "octo/blog" || app.WebService != "web" || app.HostPort != 20000 {
		t.Errorf("unrelated fields changed: %+v", app)
	}

	invalid := request(t, h, http.MethodPatch, "/api/apps/blog", `{"web_port": 0}`)
	if invalid.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid patch status = %d", invalid.Code)
	}
	rename := request(t, h, http.MethodPatch, "/api/apps/blog", `{"slug": "blog2"}`)
	if rename.Code != http.StatusUnprocessableEntity || !strings.Contains(rename.Body.String(), "cannot be changed") {
		t.Errorf("rename: %d %s", rename.Code, rename.Body)
	}
	if rec := request(t, h, http.MethodPatch, "/api/apps/missing", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing app status = %d", rec.Code)
	}
	after := decode[appJSON](t, request(t, h, http.MethodGet, "/api/apps/blog", ""))
	if after.WebPort != 8080 {
		t.Errorf("a rejected patch must not change the app: %+v", after)
	}
}

func TestEnvVars(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	createApp(t, h, "blog")
	createApp(t, h, "shop")

	empty := decode[envBody](t, request(t, h, http.MethodGet, "/api/apps/blog/env", ""))
	if empty.Vars == nil || len(empty.Vars) != 0 {
		t.Errorf("empty env = %+v", empty)
	}

	put := request(t, h, http.MethodPut, "/api/apps/blog/env",
		`{"vars": [{"key": "SECRET_KEY_BASE", "value": "s3cr3t value with $ and 'quotes'"}, {"key": "EMPTY", "value": ""}]}`)
	if put.Code != http.StatusOK {
		t.Fatalf("put: %d %s", put.Code, put.Body)
	}
	got := decode[envBody](t, request(t, h, http.MethodGet, "/api/apps/blog/env", ""))
	if len(got.Vars) != 2 || got.Vars[0].Key != "EMPTY" || got.Vars[1].Value != "s3cr3t value with $ and 'quotes'" {
		t.Errorf("env = %+v", got)
	}

	// Values are encrypted at rest.
	app, _ := srv.store.GetApp(context.Background(), "blog")
	rows, _ := srv.store.ListEnvVars(context.Background(), app.ID)
	for _, row := range rows {
		if strings.Contains(string(row.ValueEncrypted), "s3cr3t") {
			t.Error("the stored value is not encrypted")
		}
	}

	// PUT replaces the whole set and does not touch other apps.
	request(t, h, http.MethodPut, "/api/apps/blog/env", `{"vars": [{"key": "ONLY", "value": "1"}]}`)
	got = decode[envBody](t, request(t, h, http.MethodGet, "/api/apps/blog/env", ""))
	if len(got.Vars) != 1 || got.Vars[0].Key != "ONLY" {
		t.Errorf("env after replace = %+v", got)
	}
	other := decode[envBody](t, request(t, h, http.MethodGet, "/api/apps/shop/env", ""))
	if len(other.Vars) != 0 {
		t.Errorf("another app's env changed: %+v", other)
	}
}

func TestEnvVarsValidation(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")
	request(t, h, http.MethodPut, "/api/apps/blog/env", `{"vars": [{"key": "KEEP", "value": "1"}]}`)

	cases := map[string]string{
		"bad key":       `{"vars": [{"key": "NOT-VALID", "value": "topsecretvalue"}]}`,
		"empty key":     `{"vars": [{"key": "", "value": "topsecretvalue"}]}`,
		"duplicate key": `{"vars": [{"key": "A", "value": "topsecretvalue"}, {"key": "A", "value": "2"}]}`,
		"nul byte":      `{"vars": [{"key": "A", "value": "topsecretvalue\u0000"}]}`,
		"huge value":    `{"vars": [{"key": "A", "value": "topsecretvalue` + strings.Repeat("x", maxEnvValueLen) + `"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := request(t, h, http.MethodPut, "/api/apps/blog/env", body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, body = %.200s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), "topsecretvalue") {
				t.Errorf("the error echoes a value: %.200s", rec.Body)
			}
		})
	}
	got := decode[envBody](t, request(t, h, http.MethodGet, "/api/apps/blog/env", ""))
	if len(got.Vars) != 1 || got.Vars[0].Key != "KEEP" {
		t.Errorf("a rejected PUT must keep the previous set: %+v", got)
	}
}

func TestCreateDeployment(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")

	bySHA := request(t, h, http.MethodPost, "/api/apps/blog/deployments", `{"sha": "`+strings.ToUpper(testSHA)+`"}`)
	if bySHA.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", bySHA.Code, bySHA.Body)
	}
	first := decode[deploymentJSON](t, bySHA)
	if first.Status != "queued" || first.CommitSHA != testSHA || first.AppSlug != "blog" || first.StartedAt != nil {
		t.Errorf("deployment = %+v", first)
	}

	byRef := request(t, h, http.MethodPost, "/api/apps/blog/deployments", `{"ref": "main"}`)
	if byRef.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", byRef.Code, byRef.Body)
	}
	second := decode[deploymentJSON](t, byRef)
	if second.CommitSHA != testSHA || second.Ref != "main" || second.CommitMessage != "Resolved commit" {
		t.Errorf("deployment = %+v", second)
	}

	list := decode[struct{ Deployments []deploymentJSON }](t, request(t, h, http.MethodGet, "/api/apps/blog/deployments", ""))
	if len(list.Deployments) != 2 || list.Deployments[0].ID != second.ID {
		t.Errorf("list = %+v", list)
	}
	limited := decode[struct{ Deployments []deploymentJSON }](t, request(t, h, http.MethodGet, "/api/apps/blog/deployments?limit=1", ""))
	if len(limited.Deployments) != 1 {
		t.Errorf("limit ignored: %+v", limited)
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/blog/deployments?limit=0", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("bad limit status = %d", rec.Code)
	}

	one := request(t, h, http.MethodGet, fmt.Sprintf("/api/deployments/%d", second.ID), "")
	if one.Code != http.StatusOK || decode[deploymentJSON](t, one).ID != second.ID {
		t.Errorf("get deployment: %d %s", one.Code, one.Body)
	}
	app := decode[appJSON](t, request(t, h, http.MethodGet, "/api/apps/blog", ""))
	if app.LatestDeployment == nil || app.LatestDeployment.ID != second.ID || app.CurrentDeployment != nil {
		t.Errorf("app deployments = %+v / %+v", app.LatestDeployment, app.CurrentDeployment)
	}
}

func TestCreateDeploymentErrors(t *testing.T) {
	deployer := &fakeDeployer{}
	srv := newTestServer(t, func(o *Options) {
		deployer.store = o.Store
		deployer.logDir = t.TempDir()
		o.Deployer = deployer
	})
	h := srv.Handler()
	createApp(t, h, "blog")

	cases := map[string]string{
		"empty":      `{}`,
		"short sha":  `{"sha": "abc123"}`,
		"option ref": `{"ref": "--upload-pack=x"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := request(t, h, http.MethodPost, "/api/apps/blog/deployments", body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d, body = %s", rec.Code, rec.Body)
			}
		})
	}

	unknown := request(t, h, http.MethodPost, "/api/apps/blog/deployments", `{"ref": "no-such-branch"}`)
	if unknown.Code != http.StatusUnprocessableEntity || !strings.Contains(unknown.Body.String(), "ref_unresolvable") {
		t.Errorf("unknown ref: %d %s", unknown.Code, unknown.Body)
	}
	if strings.Contains(unknown.Body.String(), "secret-remote-detail") {
		t.Error("resolver internals leaked to the client")
	}

	deployer.enqueueErr = &deploy.DiskFullError{UsedPercent: 91.4, LimitPercent: 85}
	full := request(t, h, http.MethodPost, "/api/apps/blog/deployments", `{"sha": "`+testSHA+`"}`)
	if full.Code != http.StatusInsufficientStorage || !strings.Contains(full.Body.String(), "disk_full") {
		t.Errorf("disk full: %d %s", full.Code, full.Body)
	}
	if rec := request(t, h, http.MethodPost, "/api/apps/missing/deployments", `{"sha": "`+testSHA+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("missing app status = %d", rec.Code)
	}
	for _, id := range []string{"999", "abc", "0", "-1"} {
		if rec := request(t, h, http.MethodGet, "/api/deployments/"+id, ""); rec.Code != http.StatusNotFound {
			t.Errorf("deployment %s status = %d", id, rec.Code)
		}
	}
}

func TestCancelDeployment(t *testing.T) {
	deployer := &fakeDeployer{}
	srv := newTestServer(t, func(o *Options) {
		deployer.store = o.Store
		deployer.logDir = t.TempDir()
		o.Deployer = deployer
	})
	h := srv.Handler()
	createApp(t, h, "blog")
	d := decode[deploymentJSON](t, request(t, h, http.MethodPost, "/api/apps/blog/deployments", `{"sha": "`+testSHA+`"}`))

	rec := request(t, h, http.MethodPost, fmt.Sprintf("/api/deployments/%d/cancel", d.ID), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if len(deployer.canceled) != 1 || deployer.canceled[0] != d.ID {
		t.Errorf("canceled = %v", deployer.canceled)
	}
	deployer.cancelErr = deploy.ErrNotCancelable
	again := request(t, h, http.MethodPost, fmt.Sprintf("/api/deployments/%d/cancel", d.ID), "")
	if again.Code != http.StatusConflict {
		t.Errorf("status = %d", again.Code)
	}
}

func TestDeploymentLogsAsText(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	createApp(t, h, "blog")
	d := decode[deploymentJSON](t, request(t, h, http.MethodPost, "/api/apps/blog/deployments", `{"sha": "`+testSHA+`"}`))

	target := fmt.Sprintf("/api/deployments/%d/logs?format=text", d.ID)
	if rec := request(t, h, http.MethodGet, target, ""); rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("no log yet: %d %q", rec.Code, rec.Body)
	}
	if err := os.WriteFile(srv.deployer.LogPath("blog", d.ID), []byte("==> Fetching\nline two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := request(t, h, http.MethodGet, target, "")
	if rec.Body.String() != "==> Fetching\nline two\n" || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("text log = %q (%s)", rec.Body, rec.Header().Get("Content-Type"))
	}
}

// sseEvent is one parsed Server-Sent Events message.
type sseEvent struct {
	ID, Event, Data string
}

func readEvent(t *testing.T, r *bufio.Reader) sseEvent {
	t.Helper()
	var ev sseEvent
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended early: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if ev != (sseEvent{}) {
				return ev
			}
		case strings.HasPrefix(line, "id: "):
			ev.ID = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "event: "):
			ev.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.Data = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestDeploymentLogStream(t *testing.T) {
	old := logPollInterval
	logPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { logPollInterval = old })

	srv := newTestServer(t, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	createApp(t, srv.Handler(), "blog")
	d := decode[deploymentJSON](t, request(t, srv.Handler(), http.MethodPost, "/api/apps/blog/deployments", `{"sha": "`+testSHA+`"}`))
	logPath := srv.deployer.LogPath("blog", d.ID)
	appendLog := func(text string) {
		t.Helper()
		f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
	}
	appendLog("==> Fetching\n")

	url := fmt.Sprintf("%s/api/deployments/%d/logs", ts.URL, d.ID)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	reader := bufio.NewReader(resp.Body)

	first := readEvent(t, reader)
	if first.Data != "==> Fetching" || first.ID != "13" {
		t.Errorf("first event = %+v", first)
	}

	// Lines written later arrive live; a partial line waits for its newline.
	appendLog("building")
	appendLog(" image\npartial")
	second := readEvent(t, reader)
	if second.Data != "building image" || second.ID != "28" {
		t.Errorf("second event = %+v", second)
	}
	appendLog(" done\n")
	third := readEvent(t, reader)
	if third.Data != "partial done" {
		t.Errorf("third event = %+v", third)
	}

	// When the deployment finishes, the stream ends with its status.
	ctx := context.Background()
	if err := srv.store.StartDeployment(ctx, d.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	appendLog("==> Deployment succeeded\n")
	if err := srv.store.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if ev := readEvent(t, reader); ev.Data != "==> Deployment succeeded" {
		t.Errorf("event = %+v", ev)
	}
	end := readEvent(t, reader)
	if end.Event != "end" || end.Data != "succeeded" {
		t.Errorf("end event = %+v", end)
	}
	if _, err := reader.ReadByte(); err != io.EOF {
		t.Errorf("the stream must close after the end event, err = %v", err)
	}

	// A reconnect with Last-Event-ID resumes after the lines already seen.
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Last-Event-ID", second.ID)
	resumed, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resumed.Body.Close() }()
	resumedReader := bufio.NewReader(resumed.Body)
	if ev := readEvent(t, resumedReader); ev.Data != "partial done" {
		t.Errorf("resumed stream started with %+v", ev)
	}
}

func TestDeploymentLogStreamStopsWhenClientLeaves(t *testing.T) {
	old := logPollInterval
	logPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { logPollInterval = old })

	srv := newTestServer(t, nil)
	h := srv.Handler()
	createApp(t, h, "blog")
	d := decode[deploymentJSON](t, request(t, h, http.MethodPost, "/api/apps/blog/deployments", `{"sha": "`+testSHA+`"}`))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/deployments/%d/logs", d.ID), nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler kept running after the client disconnected")
	}
}
