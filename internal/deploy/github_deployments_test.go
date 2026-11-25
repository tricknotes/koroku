package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// fakeDeploymentsAPI records the requests the reporter makes.
type fakeDeploymentsAPI struct {
	mu       sync.Mutex
	nextID   int64
	creates  []github.NewDeployment
	repos    []string
	statuses []github.DeploymentStatus
	ids      []int64
	// createErr and statusErr, when set, answer the next requests (one
	// element per request; the last one repeats).
	createErr []error
	statusErr []error
	// live is what LiveDeployments answers, per environment; liveErr fails it.
	live      map[string][]int64
	liveErr   []error
	liveCalls []string
}

func popErr(errs *[]error) error {
	if len(*errs) == 0 {
		return nil
	}
	err := (*errs)[0]
	if len(*errs) > 1 {
		*errs = (*errs)[1:]
	}
	return err
}

func (f *fakeDeploymentsAPI) CreateDeployment(_ context.Context, owner, repo string, in github.NewDeployment) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, owner+"/"+repo)
	f.creates = append(f.creates, in)
	if err := popErr(&f.createErr); err != nil {
		return 0, err
	}
	f.nextID++
	return 900 + f.nextID, nil
}

func (f *fakeDeploymentsAPI) CreateDeploymentStatus(_ context.Context, owner, repo string, id int64, in github.DeploymentStatus) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, owner+"/"+repo)
	f.statuses = append(f.statuses, in)
	f.ids = append(f.ids, id)
	return popErr(&f.statusErr)
}

func (f *fakeDeploymentsAPI) LiveDeployments(_ context.Context, owner, repo, environment string) ([]int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.liveCalls = append(f.liveCalls, owner+"/"+repo+" "+environment)
	if err := popErr(&f.liveErr); err != nil {
		return nil, err
	}
	return f.live[environment], nil
}

// retired lists the GitHub deployments that were sent "inactive", in order.
func (f *fakeDeploymentsAPI) retired() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int64
	for i, s := range f.statuses {
		if s.State == github.DeploymentInactive {
			out = append(out, f.ids[i])
		}
	}
	return out
}

func (f *fakeDeploymentsAPI) states() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, s := range f.statuses {
		out = append(out, s.State)
	}
	return out
}

func (f *fakeDeploymentsAPI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.creates) + len(f.statuses)
}

type reporterHarness struct {
	t     *testing.T
	store *store.Store
	api   *fakeDeploymentsAPI
	clock *clock.Fake
	logs  *bytes.Buffer
	g     *GitHubDeployments
}

func newReporterHarness(t *testing.T) *reporterHarness {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &reporterHarness{
		t: t, store: st, api: &fakeDeploymentsAPI{},
		clock: clock.NewFake(time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)),
		logs:  &bytes.Buffer{},
	}
	h.g = NewGitHubDeployments(GitHubDeploymentsOptions{
		Store:       st,
		Client:      h.api,
		Clock:       h.clock,
		Logger:      slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ConsoleURL:  "https://console.example.com",
		BaseDomain:  "dev.example.com",
		RetryDelays: []time.Duration{0, 0},
	})
	return h
}

func (h *reporterHarness) app(slug, env string) store.App {
	h.t.Helper()
	app, err := h.store.CreateApp(context.Background(), store.App{
		Slug: slug, RepoFullName: "octo/blog", DefaultBranch: "main",
		WebService: "web", WebPort: 3000, Environment: env,
	}, 20000, 20999, h.clock.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return app
}

func (h *reporterHarness) enqueue(app store.App, sha string) store.Deployment {
	h.t.Helper()
	d, _, err := h.store.EnqueueDeployment(context.Background(), app.ID, "main", sha, "", h.clock.Now())
	if err != nil {
		h.t.Fatal(err)
	}
	return d
}

func (h *reporterHarness) sync(id int64, rolledBack bool) {
	h.g.sync(context.Background(), StatusChange{DeploymentID: id, RolledBack: rolledBack})
}

func (h *reporterHarness) link(id int64) store.GitHubDeployment {
	h.t.Helper()
	g, err := h.store.GetGitHubDeployment(context.Background(), id)
	if err != nil {
		h.t.Fatalf("GetGitHubDeployment(%d): %v", id, err)
	}
	return g
}

func TestGitHubDeploymentFollowsTheStateMachine(t *testing.T) {
	h := newReporterHarness(t)
	ctx := context.Background()
	app := h.app("blog", store.EnvironmentProduction)
	dom, err := h.store.AddDomain(ctx, app.ID, "blog.example.com", h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.RecordDomainCheck(ctx, dom.ID, true, "", h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetDomainPrimary(ctx, app.ID, dom.ID, true); err != nil {
		t.Fatal(err)
	}
	d := h.enqueue(app, shaA)

	h.sync(d.ID, false)
	if err := h.store.StartDeployment(ctx, d.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.sync(d.ID, false)
	if err := h.store.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.sync(d.ID, false)

	if len(h.api.creates) != 1 {
		t.Fatalf("creates = %+v", h.api.creates)
	}
	want := github.NewDeployment{SHA: shaA, Environment: "blog", Description: "Deployment by koroku", Production: true}
	if h.api.creates[0] != want {
		t.Errorf("create = %+v, want %+v", h.api.creates[0], want)
	}
	if got := h.api.states(); !slices.Equal(got, []string{"queued", "in_progress", "success"}) {
		t.Errorf("states = %v", got)
	}
	for _, id := range h.api.ids {
		if id != 901 {
			t.Errorf("status sent to GitHub deployment %d, want 901", id)
		}
	}
	for _, repo := range h.api.repos {
		if repo != "octo/blog" {
			t.Errorf("request for %s", repo)
		}
	}
	logURL := fmt.Sprintf("https://console.example.com/apps/blog/deploy?deployment=%d", d.ID)
	success := h.api.statuses[2]
	if success.EnvironmentURL != "https://blog.example.com" || success.LogURL != logURL {
		t.Errorf("success status = %+v", success)
	}
	if h.api.statuses[0].EnvironmentURL != "" || h.api.statuses[0].LogURL != logURL {
		t.Errorf("queued status = %+v", h.api.statuses[0])
	}
	if got := h.link(d.ID); got.GitHubID != 901 || got.State != "success" {
		t.Errorf("link = %+v", got)
	}

	// A late change is not sent again.
	h.sync(d.ID, false)
	if n := h.api.calls(); n != 4 {
		t.Errorf("%d requests after a repeated change, want 4", n)
	}
}

func TestGitHubDeploymentSkipsStatesItMissed(t *testing.T) {
	h := newReporterHarness(t)
	ctx := context.Background()
	app := h.app("blog-staging", store.EnvironmentStaging)
	d := h.enqueue(app, shaA)
	if err := h.store.StartDeployment(ctx, d.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	// The "queued" change is handled after the deployment started.
	h.sync(d.ID, false)
	h.sync(d.ID, false)
	if got := h.api.states(); !slices.Equal(got, []string{"in_progress"}) {
		t.Errorf("states = %v", got)
	}
	if h.api.creates[0].Production || h.api.creates[0].Environment != "blog-staging" {
		t.Errorf("create = %+v", h.api.creates[0])
	}
	if err := h.store.FinishDeployment(ctx, d.ID, store.StatusFailed, "start containers: exit 1 at /var/lib/x", h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.sync(d.ID, true)
	last := h.api.statuses[len(h.api.statuses)-1]
	if last.State != "failure" || last.Description != "Deployment failed; the previous release was restored" {
		t.Errorf("failure status = %+v", last)
	}
	if last.EnvironmentURL != "" {
		t.Errorf("a failure carries an environment URL: %+v", last)
	}
}

func TestDescribeStatus(t *testing.T) {
	source, promoted := int64(3), int64(9)
	cases := []struct {
		d          store.Deployment
		rolledBack bool
		state      string
		desc       string
	}{
		{store.Deployment{Status: store.StatusQueued}, false, "queued", "Deployment queued"},
		{store.Deployment{Status: store.StatusRunning, PromotedFromID: &promoted}, false, "in_progress", "Promotion from staging in progress"},
		{store.Deployment{Status: store.StatusSucceeded, SourceDeploymentID: &source}, false, "success", "Rollback to koroku deployment #3 succeeded"},
		{store.Deployment{Status: store.StatusFailed, Error: "secret path"}, false, "failure", "Deployment failed"},
		{store.Deployment{Status: store.StatusFailed}, true, "failure", "Deployment failed; the previous release was restored"},
		{store.Deployment{Status: store.StatusFailed, Error: store.InterruptedError}, false, "error", "Deployment interrupted: koroku restarted"},
		{store.Deployment{Status: store.StatusCanceled, Error: "canceled by user"}, false, "error", "Deployment canceled"},
		{store.Deployment{Status: store.StatusCanceled, Error: store.SupersededError}, false, "error", "Deployment superseded by a newer one"},
	}
	for _, tc := range cases {
		got := describeStatus(tc.d, tc.rolledBack)
		if got.State != tc.state || got.Description != tc.desc {
			t.Errorf("describeStatus(%s %q, %v) = %+v", tc.d.Status, tc.d.Error, tc.rolledBack, got)
		}
	}
}

func TestGitHubDeploymentNotCreatedForCanceledQueuedDeployments(t *testing.T) {
	h := newReporterHarness(t)
	app := h.app("blog", store.EnvironmentStaging)
	first := h.enqueue(app, shaA)
	second := h.enqueue(app, shaB) // supersedes first
	h.sync(first.ID, false)
	h.sync(second.ID, false)
	if len(h.api.creates) != 1 || h.api.creates[0].SHA != shaB {
		t.Errorf("creates = %+v", h.api.creates)
	}

	// Superseded after GitHub heard of it: the deployment ends with "error".
	third := h.enqueue(app, shaA)
	h.sync(second.ID, false)
	h.sync(third.ID, false)
	if got := h.api.states(); !slices.Equal(got, []string{"queued", "error", "queued"}) {
		t.Errorf("states = %v", got)
	}
	if desc := h.api.statuses[1].Description; desc != "Deployment superseded by a newer one" {
		t.Errorf("superseded description = %q", desc)
	}
}

func TestGitHubDeploymentSkipsAppsWithoutTheApp(t *testing.T) {
	h := newReporterHarness(t)
	ctx := context.Background()
	app, err := h.store.CreateApp(ctx, store.App{
		Slug: "pub", RepoURL: "https://example.com/pub.git", DefaultBranch: "main", WebService: "web", WebPort: 80,
	}, 20000, 20999, h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.sync(h.enqueue(app, shaA).ID, false)
	if n := h.api.calls(); n != 0 {
		t.Errorf("%d requests for an app given by URL", n)
	}

	// A repository the App is not installed on: skipped quietly.
	h.api.createErr = []error{fmt.Errorf("github: installation of octo/blog: %w", github.ErrNotFound)}
	other := h.app("blog", store.EnvironmentStaging)
	d := h.enqueue(other, shaA)
	h.sync(d.ID, false)
	if _, err := h.store.GetGitHubDeployment(ctx, d.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a link was recorded: %v", err)
	}
	if strings.Contains(h.logs.String(), "level=WARN") {
		t.Errorf("a repository without the App is warned about:\n%s", h.logs)
	}
}

func TestGitHubDeploymentWithoutPermissionIsSkippedForAWhile(t *testing.T) {
	h := newReporterHarness(t)
	app := h.app("blog", store.EnvironmentStaging)
	h.api.createErr = []error{&github.APIError{Status: http.StatusForbidden, Message: "Resource not accessible by integration"}}

	h.sync(h.enqueue(app, shaA).ID, false)
	h.sync(h.enqueue(app, shaB).ID, false)
	if n := len(h.api.creates); n != 1 {
		t.Errorf("%d create requests, want 1 (the app is skipped after a 403)", n)
	}
	if n := strings.Count(h.logs.String(), "Deployments: Read and write"); n != 1 {
		t.Errorf("the missing permission is logged %d times:\n%s", n, h.logs)
	}

	h.clock.Advance(time.Hour)
	h.api.createErr = nil
	d := h.enqueue(app, shaA)
	h.sync(d.ID, false)
	if got := h.link(d.ID); got.State != "queued" {
		t.Errorf("after the pause the app is reported again: %+v", got)
	}
}

func TestGitHubDeploymentRetriesTransientFailures(t *testing.T) {
	h := newReporterHarness(t)
	app := h.app("blog", store.EnvironmentStaging)
	h.api.createErr = []error{
		&github.APIError{Status: http.StatusBadGateway, Message: "Bad Gateway"},
		errors.New("dial tcp: i/o timeout"),
		nil,
	}
	d := h.enqueue(app, shaA)
	h.sync(d.ID, false)
	if n := len(h.api.creates); n != 3 {
		t.Errorf("%d create requests, want 3", n)
	}
	if got := h.link(d.ID); got.State != "queued" {
		t.Errorf("link = %+v", got)
	}

	// A rejected request is not repeated, and is logged.
	h.api.statusErr = []error{&github.APIError{Status: http.StatusUnprocessableEntity, Message: "Validation Failed"}}
	if err := h.store.StartDeployment(context.Background(), d.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.sync(d.ID, false)
	if got := h.api.states(); !slices.Equal(got, []string{"queued", "in_progress"}) {
		t.Errorf("states = %v", got)
	}
	if !strings.Contains(h.logs.String(), "updating the GitHub deployment failed") {
		t.Errorf("logs:\n%s", h.logs)
	}
	if got := h.link(d.ID); got.State != "queued" {
		t.Errorf("a failed update was recorded: %+v", got)
	}
}

func TestGitHubDeploymentDeletedOnGitHub(t *testing.T) {
	h := newReporterHarness(t)
	ctx := context.Background()
	app := h.app("blog", store.EnvironmentStaging)
	d := h.enqueue(app, shaA)
	h.sync(d.ID, false)
	h.api.statusErr = []error{fmt.Errorf("github: update deployment: %w", &github.APIError{Status: http.StatusNotFound, Message: "Not Found"})}
	if err := h.store.StartDeployment(ctx, d.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.sync(d.ID, false)
	if err := h.store.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.sync(d.ID, false)
	if got := h.api.states(); !slices.Equal(got, []string{"queued", "in_progress"}) {
		t.Errorf("states = %v (nothing is sent after GitHub lost the deployment)", got)
	}
	if got := h.link(d.ID); got.State != stateGone {
		t.Errorf("link = %+v", got)
	}
}

// succeed takes a deployment through the state machine, telling GitHub at
// the end only (the earlier states are skipped, as after a backlog).
func (h *reporterHarness) succeed(d store.Deployment) {
	h.t.Helper()
	ctx := context.Background()
	if err := h.store.StartDeployment(ctx, d.ID, h.clock.Now()); err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", h.clock.Now()); err != nil {
		h.t.Fatal(err)
	}
	h.sync(d.ID, false)
}

func TestGitHubDeploymentRetiresWhatASuccessReplaced(t *testing.T) {
	h := newReporterHarness(t)
	app := h.app("blog", store.EnvironmentProduction)
	h.succeed(h.enqueue(app, shaA)) // GitHub deployment 901
	if got := h.api.retired(); len(got) != 0 {
		t.Fatalf("retired %v after the first deployment", got)
	}

	// 700 was made by something else in the same environment; 950 is newer
	// than koroku's newest and is not koroku's to retire.
	h.api.live = map[string][]int64{"blog": {950, 902, 901, 700}}
	h.succeed(h.enqueue(app, shaB)) // 902
	if got := h.api.retired(); !slices.Equal(got, []int64{901, 700}) {
		t.Errorf("retired = %v, want [901 700]", got)
	}
	if !slices.Equal(h.api.liveCalls, []string{"octo/blog blog", "octo/blog blog"}) {
		t.Errorf("live deployments asked for %v", h.api.liveCalls)
	}
	last := h.api.statuses[len(h.api.statuses)-1]
	if last.Description != "Replaced by a newer deployment" || last.EnvironmentURL != "" || last.LogURL != "" {
		t.Errorf("inactive status = %+v", last)
	}
}

func TestGitHubDeploymentFailureRetiresNothing(t *testing.T) {
	h := newReporterHarness(t)
	ctx := context.Background()
	app := h.app("blog", store.EnvironmentStaging)
	h.succeed(h.enqueue(app, shaA))
	h.api.live = map[string][]int64{"blog": {901}}
	calls := len(h.api.liveCalls)

	d := h.enqueue(app, shaB)
	if err := h.store.StartDeployment(ctx, d.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.store.FinishDeployment(ctx, d.ID, store.StatusFailed, "boom", h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.sync(d.ID, true)
	if len(h.api.liveCalls) != calls || len(h.api.retired()) != 0 {
		t.Errorf("a failure retired the release that still runs: live calls %v, retired %v",
			h.api.liveCalls, h.api.retired())
	}
}

func TestGitHubDeploymentLateSuccessIsRetiredItself(t *testing.T) {
	h := newReporterHarness(t)
	ctx := context.Background()
	app := h.app("blog", store.EnvironmentStaging)
	first := h.enqueue(app, shaA)
	if err := h.store.StartDeployment(ctx, first.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.sync(first.ID, false) // 901, in_progress
	if err := h.store.FinishDeployment(ctx, first.ID, store.StatusSucceeded, "", h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	// The success of the first one has not reached GitHub when the second
	// one succeeds.
	h.succeed(h.enqueue(app, shaB)) // 902
	h.api.live = map[string][]int64{"blog": {902, 901}}
	h.sync(first.ID, false)
	if got := h.api.retired(); !slices.Equal(got, []int64{901}) {
		t.Errorf("retired = %v, want [901]", got)
	}
}

func TestGitHubDeploymentRetiringSurvivesFailures(t *testing.T) {
	h := newReporterHarness(t)
	app := h.app("blog", store.EnvironmentStaging)
	h.succeed(h.enqueue(app, shaA)) // 901
	h.succeed(h.enqueue(app, shaB)) // 902

	// One of the older ones was deleted on GitHub: the others are still retired.
	h.api.live = map[string][]int64{"blog": {903, 902, 901, 700}}
	second := h.enqueue(app, shaA) // 903
	gone := fmt.Errorf("github: update deployment: %w", &github.APIError{Status: http.StatusNotFound, Message: "Not Found"})
	h.api.statusErr = []error{nil, gone, nil} // success, inactive 902, inactive 901...
	h.succeed(second)
	if got := h.api.retired(); !slices.Equal(got, []int64{902, 901, 700}) {
		t.Errorf("retired = %v, want [902 901 700]", got)
	}
	if got := h.link(second.ID); got.State != "success" {
		t.Errorf("link = %+v", got)
	}

	// Listing fails: the deployment stays a success and nothing is retired.
	h.api.liveErr = []error{&github.APIError{Status: http.StatusUnprocessableEntity, Message: "no"}}
	before := len(h.api.retired())
	third := h.enqueue(app, shaB)
	h.succeed(third)
	if got := h.link(third.ID); got.State != "success" {
		t.Errorf("link = %+v", got)
	}
	if len(h.api.retired()) != before {
		t.Errorf("retired %v without a listing", h.api.retired())
	}
	if !strings.Contains(h.logs.String(), "listing the live GitHub deployments failed") {
		t.Errorf("logs:\n%s", h.logs)
	}
}

func TestGitHubDeploymentReviewAppsAreNotRetired(t *testing.T) {
	h := newReporterHarness(t)
	ctx := context.Background()
	parent := h.app("blog", store.EnvironmentStaging)
	child := parent
	child.Slug = "blog-pr-12"
	app, err := h.store.CreateReviewApp(ctx, store.NewReviewApp{
		App:    child,
		Review: store.ReviewApp{ParentAppID: parent.ID, PRNumber: 12, PRTitle: "Add search", HeadBranch: "feature"},
	}, 20000, 20999, h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.succeed(h.enqueue(app, shaA))
	h.api.live = map[string][]int64{"blog-pr-12": {902, 901}}
	h.succeed(h.enqueue(app, shaB))
	if len(h.api.liveCalls) != 0 || len(h.api.retired()) != 0 {
		t.Errorf("live calls %v, retired %v", h.api.liveCalls, h.api.retired())
	}
}

func TestGitHubDeploymentRunSendsStaleFinalStatuses(t *testing.T) {
	h := newReporterHarness(t)
	ctx := context.Background()
	app := h.app("blog", store.EnvironmentStaging)
	d := h.enqueue(app, shaA)
	if err := h.store.StartDeployment(ctx, d.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.sync(d.ID, false)
	// koroku stops before the final status reaches GitHub; the next start
	// records the deployment as interrupted.
	if _, err := h.store.FailInterruptedDeployments(ctx, h.clock.Now()); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.g.Run(runCtx)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for h.api.calls() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := h.api.states(); !slices.Equal(got, []string{"in_progress", "error"}) {
		t.Errorf("states = %v", got)
	}
	if desc := h.api.statuses[1].Description; desc != "Deployment interrupted: koroku restarted" {
		t.Errorf("description = %q", desc)
	}
}

func TestGitHubDeploymentReportNeverBlocks(t *testing.T) {
	h := newReporterHarness(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Nothing consumes the queue (Run is not started).
		for i := range githubQueueSize + 10 {
			h.g.Report(StatusChange{DeploymentID: int64(i)})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Report blocked on a full queue")
	}
	if !strings.Contains(h.logs.String(), "dropping one") {
		t.Errorf("a dropped change is not logged:\n%s", h.logs)
	}
}
