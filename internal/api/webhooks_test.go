package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

const (
	testWebhookSecret = "0123456789abcdef0123456789abcdef"
	pushSHA           = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakePushDeployer records push deployments in the store.
type fakePushDeployer struct {
	store *store.Store

	mu    sync.Mutex
	err   error
	calls []string
}

func (f *fakePushDeployer) EnqueuePush(ctx context.Context, app store.App, branch, sha, message, pusher string) (store.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s %s %s %q", app.Slug, branch, sha, pusher, message))
	if f.err != nil {
		return store.Deployment{}, f.err
	}
	d, _, err := f.store.EnqueueTriggered(ctx, app.ID, branch, sha, message, nil,
		store.Origin{Trigger: store.TriggerPush, Pusher: pusher}, time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC))
	return d, err
}

func (f *fakePushDeployer) EnqueueWorkflowRun(ctx context.Context, app store.App, branch, sha, message, pusher, workflow string) (store.Deployment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("%s %s %s %s %q after %s", app.Slug, branch, sha, pusher, message, workflow))
	if f.err != nil {
		return store.Deployment{}, f.err
	}
	d, _, err := f.store.EnqueueTriggered(ctx, app.ID, branch, sha, message, nil,
		store.Origin{Trigger: store.TriggerPush, Pusher: pusher, Workflow: workflow}, time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC))
	return d, err
}

func (f *fakePushDeployer) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakePushDeployer) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := f.calls
	f.calls = nil
	return calls
}

type webhookFixture struct {
	t      *testing.T
	srv    *Server
	h      http.Handler
	pushes *fakePushDeployer
	// heads are the branch heads GitHub reports (ARCHITECTURE 5.2).
	heads fakeRefs
}

// newWebhookFixture starts a server with login configured (not the
// insecure mode), so that the webhook is shown to work without a session.
func newWebhookFixture(t *testing.T, secret string) *webhookFixture {
	t.Helper()
	pushes := &fakePushDeployer{}
	srv, _ := newAuthServer(t, defaultGitHub())
	srv.cfg.GitHub.WebhookSecret = secret
	pushes.store = srv.store
	srv.pushDeployer = pushes
	heads := fakeRefs{"main": pushSHA}
	srv.refs = heads
	return &webhookFixture{t: t, srv: srv, h: srv.Handler(), pushes: pushes, heads: heads}
}

// app stores an app directly; the console API is covered elsewhere.
func (f *webhookFixture) app(app store.App) store.App {
	f.t.Helper()
	if app.WebService == "" {
		app.WebService, app.WebPort, app.HealthcheckPath = "web", 3000, "/"
	}
	if app.DefaultBranch == "" {
		app.DefaultBranch = "main"
	}
	created, err := f.srv.store.CreateApp(context.Background(), app, 20000, 20999, time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return created
}

func pushBody(repoID int64, fullName, ref, after string) string {
	return fmt.Sprintf(`{"ref": %q, "before": "cccccccccccccccccccccccccccccccccccccccc", "after": %q, "deleted": false,
		"repository": {"id": %d, "full_name": %q},
		"pusher": {"name": "octo-git", "email": "octo@example.com"},
		"sender": {"login": "octo"},
		"head_commit": {"id": %q, "message": "Fix the thing"}}`, ref, after, repoID, fullName, after)
}

func (f *webhookFixture) deliver(event, delivery, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, WebhookPath, strings.NewReader(body))
	req.Host = "console.dev.example.com"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(github.EventHeader, event)
	req.Header.Set(github.DeliveryHeader, delivery)
	req.Header.Set(github.SignatureHeader, github.Sign([]byte(testWebhookSecret), []byte(body)))
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func int64p(v int64) *int64 { return &v }

func TestWebhookSignature(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", AutoDeploy: true})
	body := pushBody(0, "octo/blog", "refs/heads/main", pushSHA)

	cases := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"missing", func(r *http.Request) { r.Header.Del(github.SignatureHeader) }},
		{"other secret", func(r *http.Request) {
			r.Header.Set(github.SignatureHeader, github.Sign([]byte("another-secret-of-enough-length"), []byte(body)))
		}},
		{"sha1 form", func(r *http.Request) { r.Header.Set(github.SignatureHeader, "sha1=abc") }},
		{"not hex", func(r *http.Request) { r.Header.Set(github.SignatureHeader, "sha256=zz") }},
	}
	for _, c := range cases {
		rec := f.deliver("push", "d-"+c.name, body, c.mutate)
		if rec.Code != http.StatusUnauthorized || errorCode(t, rec) != "bad_signature" {
			t.Errorf("%s: %d %s", c.name, rec.Code, rec.Body)
		}
	}
	// A body changed after signing.
	rec := f.deliver("push", "d-tampered", body, func(r *http.Request) {
		tampered := strings.Replace(body, pushSHA, testSHA, 2)
		r.Body = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tampered)).Body
		r.ContentLength = int64(len(tampered))
	})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("tampered body: %d %s", rec.Code, rec.Body)
	}
	if calls := f.pushes.takeCalls(); len(calls) != 0 {
		t.Fatalf("unsigned deliveries deployed: %v", calls)
	}

	// The valid one needs no session, cookie or Origin.
	rec = f.deliver("push", "d-valid", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid: %d %s", rec.Code, rec.Body)
	}
	if calls := f.pushes.takeCalls(); len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestWebhookWithoutSecretRefusesEverything(t *testing.T) {
	f := newWebhookFixture(t, "")
	f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", AutoDeploy: true})
	body := pushBody(0, "octo/blog", "refs/heads/main", pushSHA)
	// Even a body "signed" with the empty secret.
	rec := f.deliver("push", "d1", body, func(r *http.Request) {
		r.Header.Set(github.SignatureHeader, github.Sign(nil, []byte(body)))
	})
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "webhook_disabled" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if calls := f.pushes.takeCalls(); len(calls) != 0 {
		t.Fatalf("deployed: %v", calls)
	}
}

func TestWebhookEventFiltering(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", AutoDeploy: true})

	if rec := f.deliver("ping", "d-ping", `{"zen": "Keep it simple."}`); rec.Code != http.StatusOK ||
		decode[webhookResultJSON](t, rec).Result != "pong" {
		t.Errorf("ping: %d %s", rec.Code, rec.Body)
	}
	if rec := f.deliver("pull_request", "d-pr", `{"action": "opened"}`); rec.Code != http.StatusOK ||
		decode[webhookResultJSON](t, rec).Result != "ignored" {
		t.Errorf("pull_request: %d %s", rec.Code, rec.Body)
	}
	ignored := map[string]string{
		"tag":          pushBody(0, "octo/blog", "refs/tags/v1.0.0", pushSHA),
		"other branch": pushBody(0, "octo/blog", "refs/heads/feature", pushSHA),
		"deleted":      strings.Replace(pushBody(0, "octo/blog", "refs/heads/main", strings.Repeat("0", 40)), `"deleted": false`, `"deleted": true`, 1),
		"zero sha":     pushBody(0, "octo/blog", "refs/heads/main", strings.Repeat("0", 40)),
		"other repo":   pushBody(0, "octo/other", "refs/heads/main", pushSHA),
	}
	for name, body := range ignored {
		rec := f.deliver("push", "d-"+strings.ReplaceAll(name, " ", "-"), body)
		if rec.Code != http.StatusOK || decode[webhookResultJSON](t, rec).Result != "ignored" {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if rec := f.deliver("push", "d-bad", `{"ref": "refs/heads/main", "after": "nope", "repository": {"full_name": "octo/blog"}}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad sha: %d %s", rec.Code, rec.Body)
	}
	if rec := f.deliver("push", "", pushBody(0, "octo/blog", "refs/heads/main", pushSHA)); rec.Code != http.StatusBadRequest {
		t.Errorf("no delivery ID: %d %s", rec.Code, rec.Body)
	}
	if calls := f.pushes.takeCalls(); len(calls) != 0 {
		t.Fatalf("deployed: %v", calls)
	}
}

func TestWebhookMatchesApps(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	// staging and production of one repository both follow main.
	f.app(store.App{Slug: "blog-staging", RepoFullName: "Octo/Blog", AutoDeploy: true})
	f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", RepoID: int64p(99), AutoDeploy: true, Environment: store.EnvironmentProduction})
	// Follows another branch.
	f.app(store.App{Slug: "blog-next", RepoFullName: "octo/blog", AutoDeploy: true, AutoDeployBranch: "next"})
	// Off, fetched by URL, or another repository with the same ID check.
	f.app(store.App{Slug: "blog-manual", RepoFullName: "octo/blog"})
	f.app(store.App{Slug: "blog-url", RepoFullName: "octo/blog", RepoURL: "https://example.com/octo/blog.git", AutoDeploy: true})
	f.app(store.App{Slug: "renamed", RepoFullName: "octo/old-name", RepoID: int64p(99), AutoDeploy: true, DefaultBranch: "trunk"})
	f.app(store.App{Slug: "same-name-other-id", RepoFullName: "octo/blog", RepoID: int64p(7), AutoDeploy: true})

	rec := f.deliver("push", "d1", pushBody(99, "octo/blog", "refs/heads/main", pushSHA))
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := decode[webhookResultJSON](t, rec)
	var slugs []string
	for _, a := range got.Apps {
		if a.Status != "queued" || a.DeploymentID == 0 {
			t.Errorf("%+v", a)
		}
		slugs = append(slugs, a.App)
	}
	if strings.Join(slugs, ",") != "blog,blog-staging" {
		t.Errorf("deployed %v", slugs)
	}
	calls := f.pushes.takeCalls()
	if len(calls) != 2 || calls[0] != `blog main `+pushSHA+` octo "Fix the thing"` {
		t.Errorf("calls = %v", calls)
	}

	// A renamed repository matches by ID; the branch is the app's default.
	rec = f.deliver("push", "d2", pushBody(99, "octo/new-name", "refs/heads/trunk", pushSHA))
	if got := decode[webhookResultJSON](t, rec); len(got.Apps) != 1 || got.Apps[0].App != "renamed" {
		t.Errorf("renamed: %s", rec.Body)
	}
	rec = f.deliver("push", "d3", pushBody(99, "octo/blog", "refs/heads/next", pushSHA))
	if got := decode[webhookResultJSON](t, rec); len(got.Apps) != 1 || got.Apps[0].App != "blog-next" {
		t.Errorf("next: %s", rec.Body)
	}
	f.pushes.takeCalls()

	// The deployment is recorded as a push.
	app, err := f.srv.store.GetApp(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	list, err := f.srv.store.ListDeployments(context.Background(), app.ID, 5)
	if err != nil || len(list) != 1 || list[0].Trigger != store.TriggerPush || list[0].Pusher != "octo" || list[0].Ref != "main" {
		t.Fatalf("deployments = %+v, %v", list, err)
	}
}

func TestWebhookSkipsStoppedApps(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	app := f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", AutoDeploy: true})
	if err := f.srv.store.SetAppDesiredState(context.Background(), app.ID, store.DesiredStopped, time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := f.deliver("push", "d1", pushBody(0, "octo/blog", "refs/heads/main", pushSHA))
	if got := decode[webhookResultJSON](t, rec); rec.Code != http.StatusOK || len(got.Apps) != 1 || got.Apps[0].Status != "stopped" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if calls := f.pushes.takeCalls(); len(calls) != 0 {
		t.Fatalf("deployed: %v", calls)
	}
}

func TestWebhookDropsDuplicates(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", AutoDeploy: true})
	body := pushBody(0, "octo/blog", "refs/heads/main", pushSHA)

	if rec := f.deliver("push", "d1", body); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// A redelivery, and the same signed body under another delivery ID.
	for _, delivery := range []string{"d1", "d2"} {
		rec := f.deliver("push", delivery, body)
		if got := decode[webhookResultJSON](t, rec); len(got.Apps) != 1 || got.Apps[0].Status != "duplicate" {
			t.Errorf("%s: %s", delivery, rec.Body)
		}
	}
	// A new push is not a duplicate.
	if rec := f.deliver("push", "d3", pushBody(0, "octo/blog", "refs/heads/main", testSHA)); decode[webhookResultJSON](t, rec).Apps[0].Status != "queued" {
		t.Errorf("new push: %s", rec.Body)
	}
	if calls := f.pushes.takeCalls(); len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestWebhookFailureCanBeRedelivered(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	f.app(store.App{Slug: "blog", RepoFullName: "octo/blog", AutoDeploy: true})
	body := pushBody(0, "octo/blog", "refs/heads/main", pushSHA)

	f.pushes.setErr(deploy.ErrBusy)
	rec := f.deliver("push", "d1", body)
	if got := decode[webhookResultJSON](t, rec); rec.Code != http.StatusServiceUnavailable || got.Apps[0].Error != "busy" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f.pushes.setErr(&deploy.DiskFullError{UsedPercent: 90, LimitPercent: 85})
	if rec := f.deliver("push", "d1", body); decode[webhookResultJSON](t, rec).Apps[0].Error != "disk_full" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	f.pushes.setErr(nil)
	rec = f.deliver("push", "d1", body)
	if got := decode[webhookResultJSON](t, rec); rec.Code != http.StatusOK || got.Apps[0].Status != "queued" {
		t.Fatalf("redelivery: %d %s", rec.Code, rec.Body)
	}
}

func TestWebhookLimits(t *testing.T) {
	f := newWebhookFixture(t, testWebhookSecret)
	huge := `{"pad": "` + strings.Repeat("x", maxWebhookBodyBytes) + `"}`
	if rec := f.deliver("push", "d-huge", huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("huge: %d", rec.Code)
	}
	if rec := f.deliver("push", "d-form", "payload=x", func(r *http.Request) {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form: %d", rec.Code)
	}
	limited := false
	for i := range 100 {
		if rec := f.deliver("ping", fmt.Sprintf("p%d", i), `{}`); rec.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("deliveries are not rate-limited")
	}
	// Other methods do not reach the handler or the console.
	req := httptest.NewRequest(http.MethodGet, WebhookPath, nil)
	req.Host = "console.dev.example.com"
	if rec := send(f.h, req); rec.Code != http.StatusNotFound {
		t.Errorf("GET: %d", rec.Code)
	}
}

func TestDeliverySetIsBounded(t *testing.T) {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	set := newDeliverySet(3, time.Hour)
	for i := range 5 {
		if !set.reserve(now, fmt.Sprint(i)) {
			t.Fatalf("%d refused", i)
		}
	}
	if len(set.seen) != 3 || set.reserve(now, "4") || !set.reserve(now, "0") {
		t.Errorf("seen = %v", set.seen)
	}
	// Expired keys are forgotten.
	if !set.reserve(now.Add(time.Hour), "4") {
		t.Error("an expired key was still remembered")
	}
	// A refused reservation records none of its keys.
	if set.reserve(now.Add(time.Hour), "new", "4") || !set.reserve(now.Add(time.Hour), "new") {
		t.Error("a refused reservation recorded keys")
	}
}

func TestAutoDeploySettings(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")
	rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"auto_deploy": true, "auto_deploy_branch": "release"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := decode[appJSON](t, rec); !got.AutoDeploy || got.AutoDeployBranch != "release" {
		t.Errorf("%+v", got)
	}
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"auto_deploy_branch": "bad..name"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad branch: %d %s", rec.Code, rec.Body)
	}
	// Only apps fetched through the GitHub App receive pushes.
	rec = request(t, h, http.MethodPost, "/api/apps",
		`{"slug": "oss", "repo_url": "https://github.com/octo/oss.git", "web_port": 3000, "auto_deploy": true}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "auto_deploy") {
		t.Errorf("repo_url: %d %s", rec.Code, rec.Body)
	}
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"repo_url": "https://github.com/octo/blog.git"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("switching an auto-deploy app to repo_url: %d %s", rec.Code, rec.Body)
	}
}

func TestProductionAutoDeployNeedsRecentLogin(t *testing.T) {
	srv, clk := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")
	origin := map[string]string{"Origin": consoleURL}
	for _, body := range []string{
		`{"slug":"blog","web_port":3000,"repo_full_name":"octo/blog","environment":"production"}`,
		`{"slug":"blog-staging","web_port":3000,"repo_full_name":"octo/blog"}`,
	} {
		if rec := b.do(http.MethodPost, "/api/apps", body, origin); rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
	}
	clk.Advance(ReauthWindow)

	if rec := b.do(http.MethodPatch, "/api/apps/blog-staging", `{"auto_deploy": true}`, origin); rec.Code != http.StatusOK {
		t.Errorf("staging: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy": true}`, origin); rec.Code != http.StatusForbidden || errorCode(t, rec) != "reauth_required" {
		t.Errorf("production on: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(http.MethodPost, "/api/apps", `{"slug":"shop","web_port":3000,"repo_full_name":"octo/shop","environment":"production","auto_deploy":true}`, origin); rec.Code != http.StatusForbidden {
		t.Errorf("create production with auto deploy: %d %s", rec.Code, rec.Body)
	}

	b.login("code-owner")
	if rec := b.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy": true}`, origin); rec.Code != http.StatusOK {
		t.Fatalf("after login: %d %s", rec.Code, rec.Body)
	}
	clk.Advance(ReauthWindow)
	// Another branch is another code path to production; turning it off is not.
	if rec := b.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy_branch": "hotfix"}`, origin); rec.Code != http.StatusForbidden {
		t.Errorf("production branch change: %d %s", rec.Code, rec.Body)
	}
	// So is waiting for another workflow, or for none.
	if rec := b.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy_workflow": ".github/workflows/image.yml"}`, origin); rec.Code != http.StatusForbidden {
		t.Errorf("production workflow change: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(http.MethodPatch, "/api/apps/blog", `{"auto_deploy": false}`, origin); rec.Code != http.StatusOK {
		t.Errorf("production off: %d %s", rec.Code, rec.Body)
	}
}
