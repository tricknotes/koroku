package deploy

import (
	"context"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// newIdentityHarness is a harness whose engine fetches through a fake
// GitHub App: the tokens and the repository lookups both come from api.
func newIdentityHarness(t *testing.T, api *fakeGitHubAPI) *harness {
	t.Helper()
	if api.token == "" {
		api.token = "ghs_exampleInstallationToken123"
	}
	return newHarness(t, func(o *Options) {
		o.Tokens = GitHubTokens{Client: api}
		o.Repos = GitHubRepos{Client: api}
	})
}

// createAppWithRepo creates an app fetched as octo/<slug> whose settings
// carry the repository's ID (0 for none).
func (h *harness) createAppWithRepo(slug string, repoID int64) store.App {
	h.t.Helper()
	app := h.createApp(slug)
	if repoID == 0 {
		return app
	}
	app.RepoID = &repoID
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		h.t.Fatalf("UpdateApp: %v", err)
	}
	return app
}

func (h *harness) repoID(slug string) int64 {
	h.t.Helper()
	if id := h.reload(slug).RepoID; id != nil {
		return *id
	}
	return 0
}

// steps returns the step keys of a deploy log, in order.
func (h *harness) steps(slug string, id int64) string {
	h.t.Helper()
	var steps []string
	for _, line := range strings.Split(h.log(slug, id), "\n") {
		if fields := strings.Fields(line); len(fields) > 2 && fields[0] == "==>" {
			steps = append(steps, fields[2])
		}
	}
	return strings.Join(steps, " ")
}

// assertNothingFetched fails unless the deployment failed in the fetch
// step with want in its error, before git or docker ran.
func (h *harness) assertNothingFetched(name, slug string, d store.Deployment, want string) {
	h.t.Helper()
	if d.Status != store.StatusFailed || !strings.HasPrefix(d.Error, "fetch source: ") || !strings.Contains(d.Error, want) {
		h.t.Errorf("%s: deployment = %s %q, want it failed with %q", name, d.Status, d.Error, want)
	}
	if !strings.Contains(d.Error, "Nothing was fetched") {
		h.t.Errorf("%s: the error does not say that nothing was fetched: %q", name, d.Error)
	}
	if calls := h.gitCalls(); len(calls) > 0 {
		h.t.Errorf("%s: git ran: %v", name, calls)
	}
	if calls := h.dockerCalls(); len(calls) > 0 {
		h.t.Errorf("%s: docker ran: %v", name, calls)
	}
	if steps := h.steps(slug, d.ID); steps != "fetch failed" {
		h.t.Errorf("%s: steps = %q, want the deployment to end in the fetch step", name, steps)
	}
}

// A deployment fetches only from the repository the app was set up with
// (ARCHITECTURE 5): when the name denotes a repository with another ID,
// the deployment fails before anything of it is fetched, built or started,
// and the release that runs is left alone.
func TestDeployRefusesAnotherRepositoryUnderTheSameName(t *testing.T) {
	api := &fakeGitHubAPI{repos: map[string]github.Repo{"octo/blog": {ID: 100, FullName: "octo/blog"}}}
	h := newIdentityHarness(t, api)
	h.createAppWithRepo("blog", 100)
	h.start()

	first := h.deploy("blog", shaA)
	if first.Status != store.StatusSucceeded {
		t.Fatalf("the repository the app was set up with: %+v\n%s", first, h.log("blog", first.ID))
	}
	if log := h.log("blog", first.ID); !strings.Contains(log, "Confirmed that octo/blog is repository 100") {
		t.Errorf("the log does not say that the repository was confirmed:\n%s", log)
	}
	if api.lookups() != 1 {
		t.Errorf("lookups = %d, want one per deployment", api.lookups())
	}

	// The repository was deleted and another one took its name.
	api.set(func(f *fakeGitHubAPI) { f.repos["octo/blog"] = github.Repo{ID: 200, FullName: "octo/blog"} })
	h.runner.Reset()
	d := h.deploy("blog", shaB)
	h.assertNothingFetched("another repository", "blog", d, "octo/blog is not the repository this app was set up with")
	for _, want := range []string{"GitHub has repository 200 under that name", "set up with repository 100", "the owner sets the app's repository again (repo_id 200)"} {
		if !strings.Contains(d.Error, want) {
			t.Errorf("the error lacks %q: %q", want, d.Error)
		}
	}
	app := h.reload("blog")
	if app.CurrentDeploymentID == nil || *app.CurrentDeploymentID != first.ID {
		t.Errorf("current deployment = %v, want %d", app.CurrentDeploymentID, first.ID)
	}
	if h.repoID("blog") != 100 {
		t.Errorf("repo_id = %d, want it left at 100", h.repoID("blog"))
	}

	// Automatic deployments take the same step.
	h.runner.Reset()
	pushed, err := h.engine.EnqueuePush(context.Background(), app, "main", shaB, "Fix", "octocat")
	if err != nil {
		t.Fatalf("EnqueuePush: %v", err)
	}
	h.assertNothingFetched("a push", "blog", h.wait(pushed.ID), "is not the repository this app was set up with")

	// A rollback starts a kept release and fetches nothing, so it is not
	// held to the check.
	before := api.lookups()
	if back := h.rollback("blog", first.ID); back.Status != store.StatusSucceeded {
		t.Errorf("rollback = %s %q", back.Status, back.Error)
	}
	if api.lookups() != before {
		t.Errorf("the rollback looked the repository up")
	}
}

// A promotion fetches the commit into the target app, so the target's
// repository is confirmed like any other deployment's.
func TestPromotionConfirmsTheTargetsRepository(t *testing.T) {
	api := &fakeGitHubAPI{repos: map[string]github.Repo{
		"octo/shop-staging": {ID: 100, FullName: "octo/shop-staging"},
		"octo/shop":         {ID: 200, FullName: "octo/shop"},
	}}
	h := newIdentityHarness(t, api)
	h.createAppWithRepo("shop-staging", 100)
	prod := h.createAppWithRepo("shop", 100)
	h.start()

	source := h.deploy("shop-staging", shaA)
	if source.Status != store.StatusSucceeded {
		t.Fatalf("staging: %+v\n%s", source, h.log("shop-staging", source.ID))
	}
	h.runner.Reset()
	d, err := h.engine.EnqueuePromotion(context.Background(), prod, source)
	if err != nil {
		t.Fatalf("EnqueuePromotion: %v", err)
	}
	h.assertNothingFetched("a promotion", "shop", h.wait(d.ID), "octo/shop is not the repository this app was set up with")
}

// A renamed or transferred repository keeps its ID and GitHub redirects
// its old name, so the deployment goes on. The log says what GitHub calls
// the repository; the app's settings are left as they are.
func TestDeployFollowsARenamedRepository(t *testing.T) {
	api := &fakeGitHubAPI{repos: map[string]github.Repo{"octo/blog": {ID: 100, FullName: "my-org/journal"}}}
	h := newIdentityHarness(t, api)
	h.createAppWithRepo("blog", 100)
	h.start()

	d := h.deploy("blog", shaA)
	log := h.log("blog", d.ID)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %s %q\n%s", d.Status, d.Error, log)
	}
	if !strings.Contains(log, "GitHub calls this repository my-org/journal now") {
		t.Errorf("the log does not name the repository's new name:\n%s", log)
	}
	if app := h.reload("blog"); app.RepoFullName != "octo/blog" || h.repoID("blog") != 100 {
		t.Errorf("the app's repository was rewritten: %q, %d", app.RepoFullName, h.repoID("blog"))
	}
	// A name that differs only in case is the same name.
	api.set(func(f *fakeGitHubAPI) { f.repos["octo/blog"] = github.Repo{ID: 100, FullName: "Octo/Blog"} })
	d = h.deploy("blog", shaB)
	if log := h.log("blog", d.ID); d.Status != store.StatusSucceeded || strings.Contains(log, "GitHub calls this repository") {
		t.Errorf("deployment = %s %q\n%s", d.Status, d.Error, log)
	}
}

// An app without a repository ID trusts the first reading: the ID is
// recorded once and every later deployment is held to it.
func TestDeployRecordsTheRepositoryOnFirstUse(t *testing.T) {
	api := &fakeGitHubAPI{repos: map[string]github.Repo{"octo/blog": {ID: 100, FullName: "octo/blog"}}}
	h := newIdentityHarness(t, api)
	h.createAppWithRepo("blog", 0)
	h.start()

	first := h.deploy("blog", shaA)
	if first.Status != store.StatusSucceeded {
		t.Fatalf("first deployment = %s %q\n%s", first.Status, first.Error, h.log("blog", first.ID))
	}
	if h.repoID("blog") != 100 {
		t.Fatalf("repo_id = %d, want the first reading recorded", h.repoID("blog"))
	}
	if log := h.log("blog", first.ID); !strings.Contains(log, "Recorded octo/blog as repository 100") {
		t.Errorf("the log does not say that the ID was recorded:\n%s", log)
	}

	second := h.deploy("blog", shaB)
	if log := h.log("blog", second.ID); second.Status != store.StatusSucceeded || strings.Contains(log, "Recorded") {
		t.Errorf("second deployment = %s %q\n%s", second.Status, second.Error, log)
	}

	api.set(func(f *fakeGitHubAPI) { f.repos["octo/blog"] = github.Repo{ID: 200, FullName: "octo/blog"} })
	h.runner.Reset()
	third := h.deploy("blog", shaC)
	h.assertNothingFetched("after the first use", "blog", third, "is not the repository this app was set up with")
	if h.repoID("blog") != 100 {
		t.Errorf("repo_id = %d, want the first reading kept", h.repoID("blog"))
	}
}

// A review app deploys its parent's repository: without an ID of its own
// it is held to the parent's instead of trusting its own first reading.
func TestReviewAppIsHeldToItsParentsRepository(t *testing.T) {
	api := &fakeGitHubAPI{repos: map[string]github.Repo{"octo/blog-staging": {ID: 200, FullName: "octo/blog-staging"}}}
	h := newIdentityHarness(t, api)
	parent := h.createAppWithRepo("blog-staging", 0)
	child := parent
	child.Slug = "blog-staging-pr-12"
	if _, err := h.store.CreateReviewApp(context.Background(), store.NewReviewApp{
		App:    child,
		Review: store.ReviewApp{ParentAppID: parent.ID, PRNumber: 12, PRTitle: "Add search", HeadBranch: "main"},
	}, 20000, 20999, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	// The parent's repository becomes known after the review app was made.
	if _, err := h.store.RecordAppRepoID(context.Background(), parent.ID, 100); err != nil {
		t.Fatal(err)
	}
	h.start()

	h.runner.Reset()
	d := h.deploy("blog-staging-pr-12", shaA)
	h.assertNothingFetched("a review app", "blog-staging-pr-12", d, "GitHub has repository 200 under that name, the app was set up with repository 100")
	if id := h.repoID("blog-staging-pr-12"); id != 0 {
		t.Errorf("the review app recorded repository %d on its own", id)
	}

	api.set(func(f *fakeGitHubAPI) {
		f.repos["octo/blog-staging"] = github.Repo{ID: 100, FullName: "octo/blog-staging"}
	})
	d = h.deploy("blog-staging-pr-12", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %s %q\n%s", d.Status, d.Error, h.log("blog-staging-pr-12", d.ID))
	}
	if id := h.repoID("blog-staging-pr-12"); id != 100 {
		t.Errorf("the review app's repo_id = %d, want the parent's", id)
	}
}

// What cannot be confirmed is not waved through: a GitHub API failure
// fails the fetch step, as a failure to get the token does.
func TestDeployFailsWhenTheRepositoryCannotBeConfirmed(t *testing.T) {
	api := &fakeGitHubAPI{repos: map[string]github.Repo{}}
	h := newIdentityHarness(t, api)
	h.createAppWithRepo("blog", 100)
	h.createAppWithRepo("shop", 0)
	h.start()

	limited := &github.APIError{Status: 403, Message: "API rate limit exceeded", RateLimited: true}
	api.set(func(f *fakeGitHubAPI) { f.repoErr = limited })
	for _, slug := range []string{"blog", "shop"} {
		h.runner.Reset()
		d := h.deploy(slug, shaA)
		h.assertNothingFetched("rate limited, "+slug, slug, d, "cannot confirm that octo/"+slug+" is the repository this app was set up with")
		if !strings.Contains(d.Error, "API rate limit exceeded") || !strings.Contains(d.Error, "deploy again once GitHub answers") {
			t.Errorf("%s: the error does not say why and what to do: %q", slug, d.Error)
		}
	}
	if h.repoID("shop") != 0 {
		t.Errorf("an ID was recorded without a reading: %d", h.repoID("shop"))
	}

	// GitHub has no repository of the name the app's ID belongs to.
	api.set(func(f *fakeGitHubAPI) { f.repoErr = nil })
	h.runner.Reset()
	d := h.deploy("blog", shaA)
	h.assertNothingFetched("gone", "blog", d, "GitHub shows koroku no repository of that name")

	// Without an ID there is nothing to compare: git reports the trouble.
	d = h.deploy("shop", shaA)
	if d.Status != store.StatusSucceeded || h.repoID("shop") != 0 {
		t.Errorf("no ID and no repository: deployment = %s %q, repo_id %d", d.Status, d.Error, h.repoID("shop"))
	}

	// The token itself failing stops the deployment before the lookup.
	api.set(func(f *fakeGitHubAPI) { f.tokenErr = github.ErrRateLimited })
	before := api.lookups()
	d = h.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "get repository token") || api.lookups() != before {
		t.Errorf("token failure: deployment = %s %q", d.Status, d.Error)
	}
}

// Repositories the check does not cover are fetched as they are: a
// repository the GitHub App cannot see (git fetches it without
// credentials, as a public repository) and an explicit URL.
func TestDeployWithoutTheAppIsNotConfirmed(t *testing.T) {
	api := &fakeGitHubAPI{tokenErr: github.ErrNotFound, repos: map[string]github.Repo{"octo/blog": {ID: 200, FullName: "octo/blog"}}}
	h := newIdentityHarness(t, api)
	h.createAppWithRepo("blog", 0)
	byURL := h.createAppWithRepo("mirror", 100)
	byURL.RepoURL = "https://git.example.com/octo/mirror.git"
	if err := h.store.UpdateApp(context.Background(), byURL, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.start()

	d := h.deploy("blog", shaA)
	log := h.log("blog", d.ID)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("not installed: deployment = %s %q\n%s", d.Status, d.Error, log)
	}
	if !strings.Contains(log, "The GitHub App cannot see octo/blog") {
		t.Errorf("the log does not say that the repository was not confirmed:\n%s", log)
	}
	if api.lookups() != 0 || h.repoID("blog") != 0 {
		t.Errorf("lookups = %d, repo_id = %d; want neither a lookup nor a recorded ID", api.lookups(), h.repoID("blog"))
	}

	d = h.deploy("mirror", shaA)
	if d.Status != store.StatusSucceeded || api.lookups() != 0 {
		t.Errorf("explicit URL: deployment = %s %q, lookups %d", d.Status, d.Error, api.lookups())
	}
}
