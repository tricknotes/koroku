package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// fakePromoter plays the engine's promotion side.
type fakePromoter struct {
	store      *store.Store
	decisions  []deploy.ImageDecision
	previewErr error
	previewed  []string
}

func (f *fakePromoter) EnqueuePromotion(ctx context.Context, app store.App, source store.Deployment) (store.Deployment, error) {
	d, _, err := f.store.EnqueuePromotion(ctx, app.ID, "", source.CommitSHA, source.CommitMessage, &source.ID,
		time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC))
	return d, err
}

func (f *fakePromoter) PreviewPromotionImages(_ context.Context, target, src store.App, source store.Deployment) ([]deploy.ImageDecision, error) {
	f.previewed = append(f.previewed, fmt.Sprintf("%s<-%s#%d", target.Slug, src.Slug, source.ID))
	return f.decisions, f.previewErr
}

const (
	stagingSHA = "5555555555555555555555555555555555555555"
	middleSHA  = "4444444444444444444444444444444444444444"
	prodSHA    = "3333333333333333333333333333333333333333"
)

type promotionFixture struct {
	t        *testing.T
	srv      *Server
	h        http.Handler
	promoter *fakePromoter
	gh       *fakeGitHub
}

// newPromotionFixture creates shop-staging and shop (production) from one
// repository, with a succeeded release on each, and pairs them.
func newPromotionFixture(t *testing.T) *promotionFixture {
	t.Helper()
	f := &promotionFixture{t: t, gh: &fakeGitHub{}}
	f.srv = newTestServer(t, func(o *Options) {
		f.promoter = &fakePromoter{store: o.Store}
		o.Promoter = f.promoter
		o.Auth.GitHub = f.gh
	})
	f.h = f.srv.Handler()
	for _, body := range []string{
		`{"slug": "shop-staging", "repo_full_name": "octo/shop", "web_port": 3000}`,
		`{"slug": "shop", "repo_full_name": "octo/shop", "web_port": 3000, "environment": "production"}`,
	} {
		if rec := request(t, f.h, http.MethodPost, "/api/apps", body); rec.Code != http.StatusCreated {
			t.Fatalf("create app: %d %s", rec.Code, rec.Body)
		}
	}
	f.release("shop-staging", stagingSHA)
	f.release("shop", prodSHA)
	if rec := request(t, f.h, http.MethodPatch, "/api/apps/shop", `{"promote_from": "shop-staging"}`); rec.Code != http.StatusOK {
		t.Fatalf("pair: %d %s", rec.Code, rec.Body)
	}
	return f
}

// release records a succeeded deployment as the app's current release.
func (f *promotionFixture) release(slug, sha string) store.Deployment {
	f.t.Helper()
	ctx := context.Background()
	st := f.srv.store
	app, err := st.GetApp(ctx, slug)
	if err != nil {
		f.t.Fatal(err)
	}
	d, _, err := st.EnqueueDeployment(ctx, app.ID, "main", sha, "Commit "+sha[:4], time.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	if err := st.StartDeployment(ctx, d.ID, time.Now()); err != nil {
		f.t.Fatal(err)
	}
	if err := st.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", time.Now()); err != nil {
		f.t.Fatal(err)
	}
	d, _ = st.GetDeployment(ctx, d.ID)
	return d
}

func (f *promotionFixture) preview() promotionJSON {
	f.t.Helper()
	rec := request(f.t, f.h, http.MethodGet, "/api/apps/shop/promotion", "")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	return decode[promotionJSON](f.t, rec)
}

func TestPairingValidation(t *testing.T) {
	f := newPromotionFixture(t)
	if app := decode[appJSON](t, request(t, f.h, http.MethodGet, "/api/apps/shop", "")); app.PromoteFrom != "shop-staging" {
		t.Errorf("promote_from = %q", app.PromoteFrom)
	}
	for _, body := range []string{
		`{"slug": "other", "repo_full_name": "octo/other", "web_port": 3000}`,
		`{"slug": "shop-prod2", "repo_full_name": "octo/shop", "web_port": 3000, "environment": "production"}`,
		`{"slug": "shop-review", "repo_full_name": "octo/shop", "web_port": 3000}`,
	} {
		if rec := request(t, f.h, http.MethodPost, "/api/apps", body); rec.Code != http.StatusCreated {
			t.Fatalf("create app: %d %s", rec.Code, rec.Body)
		}
	}

	tests := []struct {
		slug, body, want string
	}{
		{"shop", `{"promote_from": "nope"}`, "no such app"},
		{"shop", `{"promote_from": "shop"}`, "cannot be the app itself"},
		{"shop", `{"promote_from": "other"}`, "must deploy the same repository"},
		{"shop", `{"promote_from": "shop-prod2"}`, "must be a staging app"},
		{"shop-review", `{"promote_from": "shop-staging"}`, "only a production app"},
	}
	for _, tt := range tests {
		rec := request(t, f.h, http.MethodPatch, "/api/apps/"+tt.slug, tt.body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tt.want) {
			t.Errorf("PATCH %s %s = %d %s, want %q", tt.slug, tt.body, rec.Code, rec.Body, tt.want)
		}
	}

	// Becoming production and pairing in one request works.
	rec := request(t, f.h, http.MethodPatch, "/api/apps/shop-prod2",
		`{"environment": "staging"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("make staging: %d %s", rec.Code, rec.Body)
	}
	rec = request(t, f.h, http.MethodPatch, "/api/apps/shop-review",
		`{"environment": "production", "promote_from": "shop-prod2"}`)
	if rec.Code != http.StatusOK || decode[appJSON](t, rec).PromoteFrom != "shop-prod2" {
		t.Errorf("pair while becoming production: %d %s", rec.Code, rec.Body)
	}
	// An empty value unpairs.
	rec = request(t, f.h, http.MethodPatch, "/api/apps/shop", `{"promote_from": ""}`)
	if rec.Code != http.StatusOK || decode[appJSON](t, rec).PromoteFrom != "" {
		t.Errorf("unpair: %d %s", rec.Code, rec.Body)
	}
}

func TestPromotionPreview(t *testing.T) {
	f := newPromotionFixture(t)
	f.gh.commits = []github.Commit{
		{SHA: stagingSHA, Message: "Add checkout\n\nLong body", Author: "octo"},
		{SHA: middleSHA, Message: "Fix typo", Author: "octo"},
		{SHA: prodSHA, Message: "Release 1", Author: "octo"},
	}
	f.promoter.decisions = []deploy.ImageDecision{
		{Service: "web", Reason: deploy.ReasonArgsDiffer, DifferingArgs: []string{"ASSET_HOST"}},
		{Service: "worker", Reuse: true, SourceImage: "koroku-shop-staging-worker:r1"},
	}
	ctx := context.Background()
	staging, _ := f.srv.store.GetApp(ctx, "shop-staging")
	prod, _ := f.srv.store.GetApp(ctx, "shop")
	if err := f.srv.store.ReplaceEnvVars(ctx, staging.ID, []store.EnvVar{
		{Key: "DEBUG_TOOLBAR", ValueEncrypted: []byte("x")}, {Key: "SHARED", ValueEncrypted: []byte("x")},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.ReplaceEnvVars(ctx, prod.ID, []store.EnvVar{
		{Key: "SENTRY_DSN", ValueEncrypted: []byte("x")}, {Key: "SHARED", ValueEncrypted: []byte("y")},
	}); err != nil {
		t.Fatal(err)
	}

	got := f.preview()
	if got.Status != "ready" || got.SourceApp != "shop-staging" || got.SourceDeployment == nil ||
		got.SourceDeployment.CommitSHA != stagingSHA || got.CurrentDeployment == nil || got.CurrentDeployment.CommitSHA != prodSHA {
		t.Fatalf("preview = %+v", got)
	}
	if f.gh.lastRef != stagingSHA {
		t.Errorf("commits listed from %q", f.gh.lastRef)
	}
	if got.Commits.Status != "ok" || len(got.Commits.List) != 2 || got.Commits.List[0].Message != "Add checkout" {
		t.Errorf("commits = %+v", got.Commits)
	}
	if !slices.Equal(got.Env.OnlyInSource, []string{"DEBUG_TOOLBAR"}) || !slices.Equal(got.Env.OnlyInTarget, []string{"SENTRY_DSN"}) {
		t.Errorf("env = %+v", got.Env)
	}
	want := []promotionImageJSON{
		{Service: "web", Reason: "args_differ", DifferingArgs: []string{"ASSET_HOST"}},
		{Service: "worker", Reuse: true, DifferingArgs: []string{}},
	}
	if got.Images.Status != "checked" || len(got.Images.Services) != 2 ||
		got.Images.Services[0].Reason != want[0].Reason || !slices.Equal(got.Images.Services[0].DifferingArgs, want[0].DifferingArgs) ||
		!got.Images.Services[1].Reuse {
		t.Errorf("images = %+v", got.Images)
	}

	// Production's commit beyond the limit, and a failing image check.
	f.gh.commits = f.gh.commits[:2]
	f.promoter.previewErr = errors.New("docker unavailable")
	got = f.preview()
	if got.Commits.Status != "unknown" || len(got.Commits.List) != 0 || got.Images.Status != "unavailable" {
		t.Errorf("degraded preview = %+v", got)
	}
}

func TestPromotionPreviewStates(t *testing.T) {
	f := newPromotionFixture(t)
	ctx := context.Background()

	// Unpaired.
	request(t, f.h, http.MethodPatch, "/api/apps/shop", `{"promote_from": ""}`)
	if got := f.preview(); got.Status != "not_paired" || got.SourceDeployment != nil {
		t.Errorf("unpaired preview = %+v", got)
	}
	request(t, f.h, http.MethodPatch, "/api/apps/shop", `{"promote_from": "shop-staging"}`)

	// The staging app was turned into production after pairing.
	staging, _ := f.srv.store.GetApp(ctx, "shop-staging")
	staging.Environment = store.EnvironmentProduction
	if err := f.srv.store.UpdateApp(ctx, staging, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := f.preview(); got.Status != "unavailable" || got.Reason != "source_not_staging" {
		t.Errorf("preview = %+v", got)
	}
	rec := request(t, f.h, http.MethodPost, "/api/apps/shop/deployments", `{"promote_from_deployment": 1}`)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"reason":"source_not_staging"`) {
		t.Errorf("promote = %d %s", rec.Code, rec.Body)
	}
}

func TestPromote(t *testing.T) {
	f := newPromotionFixture(t)
	source := f.preview().SourceDeployment

	// Combined with a sha.
	rec := request(t, f.h, http.MethodPost, "/api/apps/shop/deployments",
		fmt.Sprintf(`{"promote_from_deployment": %d, "sha": %q}`, source.ID, stagingSHA))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("combined = %d %s", rec.Code, rec.Body)
	}

	rec = request(t, f.h, http.MethodPost, "/api/apps/shop/deployments",
		fmt.Sprintf(`{"promote_from_deployment": %d}`, source.ID))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("promote = %d %s", rec.Code, rec.Body)
	}
	d := decode[deploymentJSON](t, rec)
	if d.CommitSHA != stagingSHA || d.PromotedFrom == nil || d.PromotedFrom.DeploymentID != source.ID ||
		d.PromotedFrom.AppSlug != "shop-staging" {
		t.Errorf("promotion = %+v", d)
	}
	// The history shows where it came from.
	list := decode[struct {
		Deployments []deploymentJSON `json:"deployments"`
	}](t, request(t, f.h, http.MethodGet, "/api/apps/shop/deployments", ""))
	if len(list.Deployments) == 0 || list.Deployments[0].PromotedFrom == nil {
		t.Errorf("history = %+v", list.Deployments)
	}

	// Staging moved on after the preview.
	newer := f.release("shop-staging", middleSHA)
	rec = request(t, f.h, http.MethodPost, "/api/apps/shop/deployments",
		fmt.Sprintf(`{"promote_from_deployment": %d}`, source.ID))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "promotion_stale") {
		t.Errorf("stale = %d %s", rec.Code, rec.Body)
	}
	rec = request(t, f.h, http.MethodPost, "/api/apps/shop/deployments",
		fmt.Sprintf(`{"promote_from_deployment": %d}`, newer.ID))
	if rec.Code != http.StatusAccepted {
		t.Errorf("promote newer = %d %s", rec.Code, rec.Body)
	}
}

func TestPromoteNeedsRecentLogin(t *testing.T) {
	srv, clk := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")
	origin := map[string]string{"Origin": consoleURL}
	create := b.do(http.MethodPost, "/api/apps",
		`{"slug":"shop","web_port":3000,"repo_full_name":"octo/shop","environment":"production"}`, origin)
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body)
	}
	clk.Advance(ReauthWindow)
	rec := b.do(http.MethodPost, "/api/apps/shop/deployments", `{"promote_from_deployment": 1}`, origin)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "reauth_required") {
		t.Errorf("promote after the window = %d %s", rec.Code, rec.Body)
	}
	// The preview reads no values and stays available.
	if rec := b.do(http.MethodGet, "/api/apps/shop/promotion", "", nil); rec.Code != http.StatusOK {
		t.Errorf("preview after the window = %d %s", rec.Code, rec.Body)
	}
}
