package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

const headSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// fakePulls plays GitHub's pull requests of octo/blog.
type fakePulls struct {
	err error
	prs []github.PullRequest
}

func (f *fakePulls) ListPullRequests(_ context.Context, owner, repo string) ([]github.PullRequest, error) {
	if f.err != nil {
		return nil, f.err
	}
	if owner+"/"+repo != "octo/blog" {
		return nil, github.ErrNotFound
	}
	return f.prs, nil
}

func (f *fakePulls) GetPullRequest(_ context.Context, owner, repo string, n int) (github.PullRequest, error) {
	if f.err != nil {
		return github.PullRequest{}, f.err
	}
	for _, pr := range f.prs {
		if owner+"/"+repo == "octo/blog" && pr.Number == n {
			return pr, nil
		}
	}
	return github.PullRequest{}, github.ErrNotFound
}

type fakeVerifier struct{ ok bool }

func (f fakeVerifier) Verify(context.Context, string) error {
	if f.ok {
		return nil
	}
	return errors.New("does not resolve")
}

type fakeWatcher struct {
	mu    sync.Mutex
	kicks int
}

func (f *fakeWatcher) Kick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kicks++
}

type reviewFixture struct {
	*roleFixture
	pulls   *fakePulls
	watcher *fakeWatcher
}

func newReviewFixture(t *testing.T) *reviewFixture {
	t.Helper()
	pulls := &fakePulls{prs: []github.PullRequest{
		{Number: 12, Title: "Add search", State: "open", HeadBranch: "search", HeadSHA: headSHA, Author: "mona"},
		{Number: 13, Title: "From a fork", State: "open", HeadBranch: "main", HeadSHA: headSHA, FromFork: true},
		{Number: 14, Title: "Old", State: "closed", HeadBranch: "old", HeadSHA: headSHA},
	}}
	watcher := &fakeWatcher{}
	f := newRoleFixture(t, func(o *Options) {
		o.Pulls = pulls
		o.HostVerifier = fakeVerifier{ok: false}
		o.ReviewWatcher = watcher
	})
	ctx := context.Background()
	blog := f.app(t, "blog")
	blog.DumpHookService, blog.DumpHookCommand = "db", "pg_dump"
	blog.LoadHookService, blog.LoadHookCommand = "db", "pg_restore"
	blog.PostLoadService, blog.PostLoadCommand = "web", "bin/rails db:anonymize"
	blog.ProtectionExemptPaths = []string{"/webhooks/"}
	if err := f.srv.store.UpdateApp(ctx, blog, f.clk.Now()); err != nil {
		t.Fatal(err)
	}
	sealed, err := f.srv.box.Seal([]byte("s3cret"), secrets.EnvVarContext(blog.ID, "SECRET_KEY_BASE"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.ReplaceEnvVars(ctx, blog.ID, []store.EnvVar{{Key: "SECRET_KEY_BASE", ValueEncrypted: sealed}}); err != nil {
		t.Fatal(err)
	}
	token, _ := f.srv.box.Seal([]byte("ghp_token"), secrets.RegistryTokenContext(blog.ID))
	if err := f.srv.store.PutRegistryCredential(ctx, store.RegistryCredential{AppID: blog.ID, Registry: "ghcr.io", Username: "bot", TokenEncrypted: token, UpdatedAt: f.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	return &reviewFixture{roleFixture: f, pulls: pulls, watcher: watcher}
}

func TestCreateReviewApp(t *testing.T) {
	f := newReviewFixture(t)
	ctx := context.Background()
	rec := f.owner.do(http.MethodPost, "/api/apps/blog/review-apps", `{"pr_number":12}`, originHeader)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	var out createdReviewJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Slug != "blog-pr-12" || !out.DNSWarning || out.Review == nil || out.Review.Parent != "blog" ||
		out.Review.PRNumber != 12 || out.Review.DataCopy != store.DataCopyWaiting ||
		out.Review.PRURL != "https://github.com/octo/blog/pull/12" {
		t.Fatalf("created = %+v (review %+v)", out, out.Review)
	}
	app := f.app(t, "blog-pr-12")
	if !app.AutoDeploy || app.AutoDeployBranch != "search" || app.DefaultBranch != "search" ||
		!app.AccessProtection || app.Environment != store.EnvironmentStaging || app.RepoFullName != "octo/blog" ||
		app.LoadHookCommand != "pg_restore" || app.PostLoadCommand != "bin/rails db:anonymize" ||
		len(app.ProtectionExemptPaths) != 1 {
		t.Errorf("settings = %+v", app)
	}
	// The variables and the registry login are the parent's, sealed anew.
	vars, _ := f.srv.store.ListEnvVars(ctx, app.ID)
	if len(vars) != 1 {
		t.Fatalf("vars = %+v", vars)
	}
	if plain, err := f.srv.box.Open(vars[0].ValueEncrypted, secrets.EnvVarContext(app.ID, "SECRET_KEY_BASE")); err != nil || string(plain) != "s3cret" {
		t.Errorf("copied variable = %q, %v", plain, err)
	}
	cred, err := f.srv.store.GetRegistryCredential(ctx, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plain, err := f.srv.box.Open(cred.TokenEncrypted, secrets.RegistryTokenContext(app.ID)); err != nil || string(plain) != "ghp_token" {
		t.Errorf("copied token = %q, %v", plain, err)
	}
	// The head was queued.
	ds, _ := f.srv.store.ListDeployments(ctx, app.ID, 5)
	if len(ds) != 1 || ds[0].CommitSHA != headSHA || ds[0].Ref != "search" {
		t.Errorf("deployments = %+v", ds)
	}
	if f.watcher.kicks != 1 {
		t.Errorf("kicks = %d", f.watcher.kicks)
	}
	acts, _ := f.srv.store.ListActivity(ctx, f.app(t, "blog").ID, 0, 10)
	if len(acts) == 0 || acts[0].Action != actReviewAppCreated {
		t.Errorf("parent activity = %+v", acts)
	}

	// The same pull request again.
	rec = f.owner.do(http.MethodPost, "/api/apps/blog/review-apps", `{"pr_number":12}`, originHeader)
	if rec.Code != http.StatusConflict || errorCode(t, rec) != "review_app_exists" {
		t.Errorf("again: %d %s", rec.Code, rec.Body)
	}
	// The list of pull requests marks it.
	rec = f.owner.do(http.MethodGet, "/api/apps/blog/pull-requests", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"review_app":"blog-pr-12"`) ||
		!strings.Contains(rec.Body.String(), `"from_fork":true`) {
		t.Errorf("pull requests: %d %s", rec.Code, rec.Body)
	}
	rec = f.owner.do(http.MethodGet, "/api/apps/blog/review-apps", "", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"slug":"blog-pr-12"`) {
		t.Errorf("review apps: %d %s", rec.Code, rec.Body)
	}
	// A review app cannot have review apps.
	rec = f.owner.do(http.MethodGet, "/api/apps/blog-pr-12/pull-requests", "", nil)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"reason":"review_app"`) {
		t.Errorf("nested: %d %s", rec.Code, rec.Body)
	}
}

func TestCreateReviewAppRefusals(t *testing.T) {
	f := newReviewFixture(t)
	cases := map[string]struct {
		path, body, code string
		status           int
	}{
		"fork":         {"/api/apps/blog/review-apps", `{"pr_number":13}`, "pull_request_from_fork", 422},
		"closed":       {"/api/apps/blog/review-apps", `{"pr_number":14}`, "pull_request_not_open", 422},
		"unknown":      {"/api/apps/blog/review-apps", `{"pr_number":99}`, "pull_request_not_found", 404},
		"no number":    {"/api/apps/blog/review-apps", `{}`, "validation_failed", 422},
		"production":   {"/api/apps/shop/review-apps", `{"pr_number":12}`, "not_review_parent", 422},
		"pulls of prd": {"/api/apps/shop/pull-requests", ``, "not_review_parent", 422},
	}
	for name, c := range cases {
		method := http.MethodPost
		if c.body == "" {
			method = http.MethodGet
		}
		rec := f.owner.do(method, c.path, c.body, originHeader)
		if rec.Code != c.status || errorCode(t, rec) != c.code {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
	if _, err := f.srv.store.GetApp(context.Background(), "blog-pr-13"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a refused review app exists: %v", err)
	}

	// Without the Pull requests permission.
	f.pulls.err = &github.APIError{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}
	rec := f.owner.do(http.MethodGet, "/api/apps/blog/pull-requests", "", nil)
	if rec.Code != http.StatusBadGateway || errorCode(t, rec) != "github_permission_missing" {
		t.Errorf("permission missing: %d %s", rec.Code, rec.Body)
	}
}

func TestCreateReviewAppNeedsARecentLogin(t *testing.T) {
	f := newReviewFixture(t)
	f.clk.Advance(11 * time.Minute)
	rec := f.owner.do(http.MethodPost, "/api/apps/blog/review-apps", `{"pr_number":12}`, originHeader)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "reauth_required" {
		t.Errorf("stale login: %d %s", rec.Code, rec.Body)
	}
}

func TestCreateReviewAppKeepsTheAppWhenTheDeployCannotBeQueued(t *testing.T) {
	f := newReviewFixture(t)
	f.srv.deployer.(*fakeDeployer).enqueueErr = &deploy.DiskFullError{UsedPercent: 90, LimitPercent: 85}
	rec := f.owner.do(http.MethodPost, "/api/apps/blog/review-apps", `{"pr_number":12,"copy_data":false}`, originHeader)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"deploy_error":"disk usage is 90%`) ||
		!strings.Contains(rec.Body.String(), `"data_copy":""`) {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	f.app(t, "blog-pr-12")
}

func TestReviewAppSkipsTheCopyWithoutHooks(t *testing.T) {
	f := newReviewFixture(t)
	blog := f.app(t, "blog")
	blog.DumpHookService, blog.DumpHookCommand = "", ""
	if err := f.srv.store.UpdateApp(context.Background(), blog, f.clk.Now()); err != nil {
		t.Fatal(err)
	}
	rec := f.owner.do(http.MethodPost, "/api/apps/blog/review-apps", `{"pr_number":12}`, originHeader)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"data_copy":"skipped","data_copy_detail":"no_dump_hook"`) {
		t.Errorf("create: %d %s", rec.Code, rec.Body)
	}
}

func TestReviewAppRoles(t *testing.T) {
	f := newReviewFixture(t)
	if rec := f.owner.do(http.MethodPost, "/api/apps/blog/review-apps", `{"pr_number":12}`, originHeader); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	// Deploy members of the parent cannot create one, managers can.
	rec := f.as[deployerID].do(http.MethodPost, "/api/apps/blog/review-apps", `{"pr_number":12}`, originHeader)
	if rec.Code != http.StatusForbidden || errorCode(t, rec) != "forbidden_role" {
		t.Errorf("deployer creates: %d %s", rec.Code, rec.Body)
	}
	// Roles follow the parent: the viewer sees the review app, the
	// stranger does not.
	if rec := f.as[viewerID].do(http.MethodGet, "/api/apps/blog-pr-12", "", nil); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"role":"view"`) {
		t.Errorf("viewer: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as[strangerID].do(http.MethodGet, "/api/apps/blog-pr-12", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("stranger: %d", rec.Code)
	}
	if rec := f.as[deployerID].do(http.MethodPost, "/api/apps/blog-pr-12/deployments", `{"ref":"main"}`, originHeader); rec.Code == http.StatusForbidden {
		t.Errorf("deployer deploys the review app: %d %s", rec.Code, rec.Body)
	}
	// No role can be set on a review app.
	rec = f.owner.do(http.MethodPut, "/api/members/101/roles/blog-pr-12", `{"role":"manage"}`, originHeader)
	if rec.Code != http.StatusUnprocessableEntity || errorCode(t, rec) != "role_follows_parent" {
		t.Errorf("set role: %d %s", rec.Code, rec.Body)
	}
}

func TestDeleteReviewApp(t *testing.T) {
	f := newReviewFixture(t)
	if rec := f.owner.do(http.MethodPost, "/api/apps/blog/review-apps", `{"pr_number":12}`, originHeader); rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	// A manager may delete review apps, not other apps.
	if rec := f.as[managerID].do(http.MethodDelete, "/api/apps/blog", "", originHeader); rec.Code != http.StatusForbidden ||
		!strings.Contains(rec.Body.String(), `"required":"owner"`) {
		t.Errorf("manager deletes blog: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as[deployerID].do(http.MethodDelete, "/api/apps/blog-pr-12", "", originHeader); rec.Code != http.StatusForbidden {
		t.Errorf("deployer deletes the review app: %d %s", rec.Code, rec.Body)
	}
	// The parent goes after its review apps.
	if rec := f.owner.do(http.MethodDelete, "/api/apps/blog", "", originHeader); rec.Code != http.StatusConflict ||
		errorCode(t, rec) != "has_review_apps" {
		t.Errorf("delete parent: %d %s", rec.Code, rec.Body)
	}
	if rec := f.as[managerID].do(http.MethodDelete, "/api/apps/blog-pr-12", "", originHeader); rec.Code != http.StatusNoContent {
		t.Fatalf("manager deletes the review app: %d %s", rec.Code, rec.Body)
	}
	ops := f.srv.ops.(*fakeDeployer).ops
	if !strings.Contains(strings.Join(ops, " "), "delete(volumes=true):blog-pr-12") {
		t.Errorf("ops = %v", ops)
	}
	acts, _ := f.srv.store.ListActivity(context.Background(), f.app(t, "blog").ID, 0, 10)
	if len(acts) == 0 || acts[0].Action != actReviewAppDeleted {
		t.Errorf("parent activity = %+v", acts)
	}
}

func TestReviewSlugs(t *testing.T) {
	got := reviewSlugs("shop-staging", 12)
	if got[0] != "shop-staging-pr-12" || got[1] != "shop-staging-pr-12-2" || len(got) != 9 {
		t.Errorf("slugs = %v", got)
	}
	long := strings.Repeat("a", 30) + "-staging"
	for _, s := range reviewSlugs(long, 12345) {
		if len(s) > maxSlugLength || !slugPattern.MatchString(s) {
			t.Errorf("slug %q does not fit", s)
		}
	}
	if got := reviewSlugs("abcdefghijklmnopqrstuvwxyzabcdefgh-x", 1)[0]; got != "abcdefghijklmnopqrstuvwxyzabcdefgh-pr-1" {
		t.Errorf("trailing hyphen kept: %q", got)
	}
}
