package review

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

var now = time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC)

type fakeCopier struct {
	mu   sync.Mutex
	err  error
	reqs []deploy.CopyRequest
}

func (f *fakeCopier) StartCopy(_ context.Context, req deploy.CopyRequest) (store.DBCopy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return store.DBCopy{}, f.err
	}
	return store.DBCopy{ID: 9, SourceKind: req.Kind}, nil
}

type fakePulls struct{ prs map[int]github.PullRequest }

func (f fakePulls) GetPullRequest(_ context.Context, owner, repo string, n int) (github.PullRequest, error) {
	if owner != "octo" || repo != "shop" {
		return github.PullRequest{}, github.ErrNotFound
	}
	pr, ok := f.prs[n]
	if !ok {
		return github.PullRequest{}, github.ErrNotFound
	}
	return pr, nil
}

type fixture struct {
	st     *store.Store
	parent store.App
	review store.App
	copier *fakeCopier
	w      *Watcher
}

func setup(t *testing.T, parentHooks bool) fixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	base := store.App{RepoFullName: "octo/shop", DefaultBranch: "main", WebService: "web", WebPort: 3000,
		HealthcheckPath: "/", Environment: store.EnvironmentStaging}
	parentIn := base
	parentIn.Slug = "shop-staging"
	if parentHooks {
		parentIn.DumpHookService, parentIn.DumpHookCommand = "db", "pg_dump"
		parentIn.LoadHookService, parentIn.LoadHookCommand = "db", "pg_restore"
	}
	parent, err := st.CreateApp(ctx, parentIn, 20000, 20999, now)
	if err != nil {
		t.Fatal(err)
	}
	reviewIn := parentIn
	reviewIn.Slug = "shop-staging-pr-12"
	review, err := st.CreateReviewApp(ctx, store.NewReviewApp{App: reviewIn, Review: store.ReviewApp{
		ParentAppID: parent.ID, PRNumber: 12, PRTitle: "Add search", HeadBranch: "search", DataCopy: store.DataCopyWaiting,
	}}, 20000, 20999, now)
	if err != nil {
		t.Fatal(err)
	}
	copier := &fakeCopier{}
	w := New(Watcher{Store: st, Copier: copier, Clock: clock.NewFake(now),
		Pulls: fakePulls{prs: map[int]github.PullRequest{12: {Number: 12, Title: "Add search (v2)", HeadBranch: "search", State: "merged"}}}})
	return fixture{st: st, parent: parent, review: review, copier: copier, w: w}
}

// deploy marks an app as having a successful deployment.
func (f fixture) deploy(t *testing.T, app store.App) {
	t.Helper()
	ctx := context.Background()
	d, _, err := f.st.EnqueueDeployment(ctx, app.ID, "search", "0123456789abcdef0123456789abcdef01234567", "msg", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.StartDeployment(ctx, d.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := f.st.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", now); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) review_(t *testing.T) store.ReviewApp {
	t.Helper()
	r, err := f.st.GetReviewApp(context.Background(), f.review.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCopyWaitsForTheFirstSuccessfulDeployment(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	f.w.StartCopies(ctx)
	if len(f.copier.reqs) != 0 {
		t.Fatalf("copied before a deployment: %+v", f.copier.reqs)
	}
	f.deploy(t, f.parent)
	f.deploy(t, f.review)
	f.w.StartCopies(ctx)
	if len(f.copier.reqs) != 1 {
		t.Fatalf("copies = %d", len(f.copier.reqs))
	}
	req := f.copier.reqs[0]
	if req.Source.ID != f.parent.ID || req.Target.ID != f.review.ID || req.Kind != store.CopySourceLive || !req.SkipBackup {
		t.Errorf("copy request = %+v", req)
	}
	if r := f.review_(t); r.DataCopy != store.DataCopyStarted {
		t.Errorf("data copy = %q", r.DataCopy)
	}
	// Started once only.
	f.w.StartCopies(ctx)
	if len(f.copier.reqs) != 1 {
		t.Errorf("copied again: %d", len(f.copier.reqs))
	}
	acts, err := f.st.ListActivity(ctx, f.review.ID, 0, 10)
	if err != nil || len(acts) != 1 || acts[0].Action != "db_copy.started" || acts[0].Actor != nil {
		t.Errorf("activity = %+v, %v", acts, err)
	}
}

func TestCopyRetriesWhileBusyAndRecordsRefusals(t *testing.T) {
	f := setup(t, true)
	ctx := context.Background()
	f.deploy(t, f.review)
	f.copier.err = deploy.ErrBusy
	f.w.StartCopies(ctx)
	if r := f.review_(t); r.DataCopy != store.DataCopyWaiting {
		t.Fatalf("busy: data copy = %q", r.DataCopy)
	}
	f.copier.err = deploy.ErrSourceNotRunning
	f.w.StartCopies(ctx)
	if r := f.review_(t); r.DataCopy != store.DataCopyFailed || r.DataCopyDetail != DetailSourceNotRunning {
		t.Errorf("refused: %+v", r)
	}
	f.copier.err = errors.New("boom")
	if err := f.st.SetReviewDataCopy(ctx, f.review.ID, store.DataCopyWaiting, ""); err != nil {
		t.Fatal(err)
	}
	f.w.StartCopies(ctx)
	if r := f.review_(t); r.DataCopy != store.DataCopyFailed || r.DataCopyDetail != DetailError {
		t.Errorf("unexpected error: %+v", r)
	}
}

func TestCopySkippedWithoutHooks(t *testing.T) {
	f := setup(t, false)
	f.deploy(t, f.review)
	f.w.StartCopies(context.Background())
	if len(f.copier.reqs) != 0 {
		t.Errorf("copied without hooks")
	}
	if r := f.review_(t); r.DataCopy != store.DataCopySkipped || r.DataCopyDetail != DetailNoDumpHook {
		t.Errorf("review = %+v", r)
	}
}

func TestRefreshPullRequests(t *testing.T) {
	f := setup(t, true)
	f.w.RefreshPullRequests(context.Background())
	r := f.review_(t)
	if r.PRState != store.PRStateMerged || r.PRTitle != "Add search (v2)" {
		t.Errorf("review = %+v", r)
	}
}
