package deploy

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/store"
)

// enqueueFrom queues sha to be deployed from ref ("" for a request that
// names only the commit).
func (h *harness) enqueueFrom(slug, ref, sha string) store.Deployment {
	h.t.Helper()
	app, err := h.store.GetApp(context.Background(), slug)
	if err != nil {
		h.t.Fatalf("GetApp: %v", err)
	}
	d, err := h.engine.Enqueue(context.Background(), app, ref, sha, "")
	if err != nil {
		h.t.Fatalf("Enqueue: %v", err)
	}
	return d
}

// gitCalls returns the git subcommands seen so far with their last
// argument, in order.
func (h *harness) gitCalls() []string {
	var calls []string
	for _, c := range h.runner.Calls() {
		if c.Name == "git" {
			calls = append(calls, c.Args[0]+" "+c.Args[len(c.Args)-1])
		}
	}
	return calls
}

// A deployment takes a commit only from a branch that reaches it
// (ARCHITECTURE 7): the SHA of a commit that some other ref of the
// repository holds, such as the head of a pull request from a fork, is
// refused before anything of it is fetched, built or started.
func TestDeployRefusesACommitTheRefDoesNotReach(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.branches = map[string][]string{
		"main":    {shaB, shaA},
		"release": {shaC, shaB, shaA},
	}
	h.start()

	first := h.wait(h.enqueueFrom("blog", "main", shaB).ID)
	if first.Status != store.StatusSucceeded {
		t.Fatalf("the tip of main: %+v\n%s", first, h.log("blog", first.ID))
	}

	foreign := strings.Repeat("f", 40)
	cases := []struct {
		name, ref, sha, want string
	}{
		{"a commit no branch reaches", "main", foreign, `commit ffffffff is not reachable from "main"`},
		{"the same commit without a ref", "", foreign, `commit ffffffff is not reachable from "main"`},
		{"a commit of another branch", "main", shaC, `commit cccccccc is not reachable from "main"`},
		{"a ref that is no branch", "pull/1/head", foreign, `no branch or tag named "pull/1/head"`},
	}
	for _, c := range cases {
		h.runner.Reset()
		d := h.wait(h.enqueueFrom("blog", c.ref, c.sha).ID)
		if d.Status != store.StatusFailed || !strings.HasPrefix(d.Error, "fetch source: ") || !strings.Contains(d.Error, c.want) {
			t.Errorf("%s: deployment = %s %q", c.name, d.Status, d.Error)
		}
		if !strings.Contains(d.Error, "deploy from a branch or tag that contains the commit") {
			t.Errorf("%s: the error does not say what to do: %q", c.name, d.Error)
		}
		if calls := h.dockerCalls(); len(calls) > 0 {
			t.Errorf("%s: docker ran: %v", c.name, calls)
		}
		for _, call := range h.gitCalls() {
			if strings.HasSuffix(call, " "+c.sha) || strings.HasPrefix(call, "checkout ") {
				t.Errorf("%s: the commit was fetched: %v", c.name, h.gitCalls())
			}
		}
		var steps []string
		for _, line := range strings.Split(h.log("blog", d.ID), "\n") {
			if fields := strings.Fields(line); len(fields) > 2 && fields[0] == "==>" {
				steps = append(steps, fields[2])
			}
		}
		if strings.Join(steps, " ") != "fetch failed" {
			t.Errorf("%s: steps = %v, want the deployment to end in the fetch step", c.name, steps)
		}
		// The release that runs is left alone.
		app, _ := h.store.GetApp(context.Background(), "blog")
		if app.CurrentDeploymentID == nil || *app.CurrentDeploymentID != first.ID {
			t.Errorf("%s: current deployment = %v, want %d", c.name, app.CurrentDeploymentID, first.ID)
		}
	}
}

func TestDeployTakesCommitsTheRefReaches(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.branches = map[string][]string{
		"main":    {shaB, shaA},
		"release": {shaC, shaB, shaA},
	}
	h.start()

	cases := []struct {
		name, ref, sha string
		history        bool
	}{
		{"the tip of the branch", "main", shaB, false},
		{"a commit behind the tip", "main", shaA, true},
		{"a commit behind the tip, without a ref", "", shaA, true},
		{"the tip of another branch, from that branch", "release", shaC, false},
		{"a commit behind the tip of another branch", "release", shaB, true},
	}
	for _, c := range cases {
		h.runner.Reset()
		d := h.wait(h.enqueueFrom("blog", c.ref, c.sha).ID)
		if d.Status != store.StatusSucceeded {
			t.Errorf("%s: deployment = %s %q\n%s", c.name, d.Status, d.Error, h.log("blog", d.ID))
			continue
		}
		// Only a commit behind the tip needs the history of the branch.
		if got := slices.ContainsFunc(h.gitCalls(), func(call string) bool { return strings.HasPrefix(call, "rev-list ") }); got != c.history {
			t.Errorf("%s: read the history = %v, want %v (%v)", c.name, got, c.history, h.gitCalls())
		}
		ref := c.ref
		if ref == "" {
			ref = "main"
		}
		if log := h.log("blog", d.ID); !strings.Contains(log, "Checking that "+c.sha[:7]+" is on "+ref) {
			t.Errorf("%s: the log does not name the ref:\n%s", c.name, log)
		}
	}
}

// A deployment queued with only a commit records the branch it is checked
// against: the branch the app follows at that moment.
func TestDeploymentWithoutARefRecordsTheBranchOfTheApp(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	ctx := context.Background()

	d := h.enqueueFrom("blog", "", shaA)
	if d.Ref != "main" {
		t.Errorf("ref = %q, want the default branch", d.Ref)
	}

	// With automatic deployments on, the app follows their branch.
	app.AutoDeploy, app.AutoDeployBranch = true, "release"
	if err := h.store.UpdateApp(ctx, app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if d := h.enqueueFrom("blog", "", shaA); d.Ref != "release" {
		t.Errorf("ref = %q, want the branch of automatic deployments", d.Ref)
	}
	// A branch that only waits for automatic deployments to be turned on
	// is not followed.
	app.AutoDeploy = false
	if err := h.store.UpdateApp(ctx, app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if d := h.enqueueFrom("blog", "", shaA); d.Ref != "main" {
		t.Errorf("ref = %q, want the default branch", d.Ref)
	}
	// A ref that was given is kept.
	if d := h.enqueueFrom("blog", "v1.2.0", shaA); d.Ref != "v1.2.0" {
		t.Errorf("ref = %q", d.Ref)
	}
}

// A queued row without a ref (written by a koroku that does not record
// one) is not exempt: the fetch step checks it against the branch the app
// follows.
func TestQueuedDeploymentWithoutARefIsCheckedAgainstTheBranchOfTheApp(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	h.branches = map[string][]string{"main": {shaB, shaA}}
	foreign := strings.Repeat("f", 40)
	d, _, err := h.store.EnqueueDeployment(context.Background(), app.ID, "", foreign, "", h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.start()

	d = h.wait(d.ID)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, `commit ffffffff is not reachable from "main"`) {
		t.Errorf("deployment = %s %q", d.Status, d.Error)
	}
	if calls := h.dockerCalls(); len(calls) > 0 {
		t.Errorf("docker ran: %v", calls)
	}
}

// A promotion fetches the commit again and is checked like any deployment,
// against the ref the promoted release was deployed from (ARCHITECTURE
// 9.7): staging may follow a branch that production never deploys itself.
func TestPromotionIsCheckedAgainstTheRefOfTheSource(t *testing.T) {
	p := newPromotionHarness(t, "https://assets.example.com", "https://assets.example.com")
	p.mu.Lock()
	p.branches = map[string][]string{
		"main":    {shaA},
		"develop": {shaB, shaA},
	}
	p.mu.Unlock()
	source := p.wait(p.enqueueFrom("shop-staging", "develop", shaB).ID)
	if source.Status != store.StatusSucceeded {
		t.Fatalf("staging deployment = %+v\n%s", source, p.log("shop-staging", source.ID))
	}

	d := p.promote(source)
	if d.Status != store.StatusSucceeded || d.Ref != "develop" {
		t.Fatalf("promotion = %s %q from %q\n%s", d.Status, d.Error, d.Ref, p.log("shop", d.ID))
	}

	// Once the branch does not reach the commit any more (a force-push),
	// the release cannot be promoted: production fetches from the repository.
	p.mu.Lock()
	p.branches["develop"] = []string{shaC, shaA}
	p.mu.Unlock()
	p.runner.Reset()
	d = p.promote(source)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, `commit bbbbbbbb is not reachable from "develop"`) {
		t.Errorf("promotion = %s %q", d.Status, d.Error)
	}
	if calls := p.dockerCalls(); len(calls) > 0 {
		t.Errorf("docker ran for a refused promotion: %v", calls)
	}

	// A source that records no branch is checked against the branch the
	// promoting app follows; the marker of a rollback is not a branch.
	prod, err := p.store.GetApp(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"", store.RollbackRef} {
		legacy := source
		legacy.Ref, legacy.CommitSHA = ref, shaA
		queued, err := p.engine.EnqueuePromotion(context.Background(), prod, legacy)
		if err != nil || queued.Ref != "main" {
			t.Errorf("promotion of a source with ref %q: ref = %q, %v", ref, queued.Ref, err)
		}
		p.wait(queued.ID)
	}
}
