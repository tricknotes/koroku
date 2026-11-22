package deploy

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

func (h *harness) rollback(slug string, from int64) store.Deployment {
	h.t.Helper()
	app, err := h.store.GetApp(context.Background(), slug)
	if err != nil {
		h.t.Fatal(err)
	}
	d, err := h.engine.EnqueueRollback(context.Background(), app, from)
	if err != nil {
		h.t.Fatalf("EnqueueRollback: %v", err)
	}
	return h.wait(d.ID)
}

func TestRollbackStartsTheKeptRelease(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.KeepReleases = 2 })
	app := h.createApp("blog")
	h.start()
	h.setEnv(app, map[string]string{"GREETING": "first"})
	first := h.deploy("blog", shaA)
	h.setEnv(app, map[string]string{"GREETING": "second"})
	second := h.deploy("blog", shaB)
	if first.Status != store.StatusSucceeded || second.Status != store.StatusSucceeded {
		t.Fatalf("deploys: %s %s", first.Status, second.Status)
	}

	h.runner.Reset()
	rb := h.rollback("blog", first.ID)
	if rb.Status != store.StatusSucceeded {
		t.Fatalf("rollback %s: %s\n%s", rb.Status, rb.Error, h.log("blog", rb.ID))
	}
	if rb.Ref != store.RollbackRef || rb.CommitSHA != shaA || rb.SourceDeploymentID == nil || *rb.SourceDeploymentID != first.ID {
		t.Errorf("rollback record = %+v", rb)
	}
	// No fetch, build or pull: only config (validation) and up.
	for _, c := range h.runner.Calls() {
		if c.Name == "git" {
			t.Errorf("git ran during a rollback: %v", c.Args)
		}
	}
	for _, sub := range h.dockerCalls() {
		if sub == "build" || sub == "pull" {
			t.Errorf("%s ran during a rollback", sub)
		}
	}
	up := h.lastCall("up")
	if up.Dir != h.engine.ReleaseDir("blog", first.ID) {
		t.Errorf("up ran in %s, want the first release", up.Dir)
	}
	// A pull_policy in the Compose file must not make the rollback pull.
	if !slices.Contains(up.Args, "--pull") || up.Args[slices.Index(up.Args, "--pull")+1] != "missing" {
		t.Errorf("rollback up args = %v, want --pull missing", up.Args)
	}
	env, _ := os.ReadFile(h.engine.EnvFile("blog"))
	if !strings.Contains(string(env), "GREETING=first") {
		t.Errorf("env file after the rollback:\n%s", env)
	}
	app, _ = h.store.GetApp(context.Background(), "blog")
	if app.CurrentDeploymentID == nil || *app.CurrentDeploymentID != first.ID {
		t.Errorf("current deployment = %v, want %d", app.CurrentDeploymentID, first.ID)
	}
	// With two releases kept, the rollback (no files) and the second deploy
	// are the newest; the release that runs now must survive the cleanup.
	if !h.engine.ReleaseAvailable(app, first.ID) || !h.engine.ReleaseAvailable(app, second.ID) {
		t.Errorf("kept releases: first %v, second %v", h.engine.ReleaseAvailable(app, first.ID), h.engine.ReleaseAvailable(app, second.ID))
	}
	var steps []string
	for _, line := range strings.Split(h.log("blog", rb.ID), "\n") {
		if fields := strings.SplitN(line, " ", 4); len(fields) == 4 && fields[0] == "==>" {
			steps = append(steps, fields[2])
		}
	}
	if want := []string{StepPrepare, StepValidate, StepStart, StepRoute, StepCheck, StepDone}; !slices.Equal(steps, want) {
		t.Errorf("steps = %v, want %v", steps, want)
	}
	if !strings.HasPrefix(h.log("blog", rb.ID), "Queued rollback to deployment #1 ") {
		t.Errorf("log starts with %q", strings.SplitN(h.log("blog", rb.ID), "\n", 2)[0])
	}

	// Rolling back to the rollback record means its release.
	again := h.rollback("blog", rb.ID)
	if again.SourceDeploymentID == nil || *again.SourceDeploymentID != first.ID {
		t.Errorf("rollback to a rollback = %+v", again)
	}
	// And forward again to the second release.
	forward := h.rollback("blog", second.ID)
	app, _ = h.store.GetApp(context.Background(), "blog")
	if forward.Status != store.StatusSucceeded || *app.CurrentDeploymentID != second.ID {
		t.Errorf("forward: %s, current %v", forward.Status, *app.CurrentDeploymentID)
	}
}

func TestRollbackRefusals(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.KeepReleases = 1 })
	app := h.createApp("blog")
	other := h.createApp("other")
	h.start()
	ctx := context.Background()
	first := h.deploy("blog", shaA)
	h.health.mu.Lock()
	h.health.err = errors.New("not answering")
	h.health.mu.Unlock()
	failed := h.deploy("blog", shaB)
	h.health.mu.Lock()
	h.health.err = nil
	h.health.mu.Unlock()
	third := h.deploy("blog", shaC) // keeps one release: first is pruned

	if _, err := h.engine.EnqueueRollback(ctx, app, failed.ID); !errors.Is(err, ErrReleaseUnavailable) {
		t.Errorf("failed deployment: err = %v", err)
	}
	if _, err := h.engine.EnqueueRollback(ctx, app, first.ID); !errors.Is(err, ErrReleaseUnavailable) {
		t.Errorf("pruned release: err = %v", err)
	}
	if _, err := h.engine.EnqueueRollback(ctx, other, third.ID); !errors.Is(err, ErrOtherApp) {
		t.Errorf("other app: err = %v", err)
	}
	if !h.engine.ReleaseAvailable(app, third.ID) || h.engine.ReleaseAvailable(app, first.ID) {
		t.Errorf("availability: third %v first %v", h.engine.ReleaseAvailable(app, third.ID), h.engine.ReleaseAvailable(app, first.ID))
	}
}

func TestFailedRollbackRestoresTheRunningRelease(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	first := h.deploy("blog", shaA)
	second := h.deploy("blog", shaB)

	h.health.mu.Lock()
	h.health.err = errors.New("not answering")
	h.health.mu.Unlock()
	rb := h.rollback("blog", first.ID)
	if rb.Status != store.StatusFailed {
		t.Fatalf("status = %s", rb.Status)
	}
	log := h.log("blog", rb.ID)
	if !strings.Contains(log, "Rolling back to deployment #2") {
		t.Errorf("log lacks the automatic rollback:\n%s", log)
	}
	app, _ := h.store.GetApp(context.Background(), "blog")
	if *app.CurrentDeploymentID != second.ID {
		t.Errorf("current = %d, want %d", *app.CurrentDeploymentID, second.ID)
	}
}

// TestRollbackWorkerWaitsForTheQueuedLine is TestWorkerWaitsForTheQueuedLine
// for EnqueueRollback.
func TestRollbackWorkerWaitsForTheQueuedLine(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	first := h.deploy("blog", shaA)
	if first.Status != store.StatusSucceeded {
		t.Fatalf("deploy: %s", first.Status)
	}
	// Wake the worker right after the insert and give it time to run.
	h.engine.afterInsert = func() {
		h.engine.notify()
		time.Sleep(100 * time.Millisecond)
	}
	// The rollback gets the next ID in the database.
	stale := h.engine.LogPath("blog", first.ID+1)
	if err := os.WriteFile(stale, []byte("left over from an earlier database\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rb := h.rollback("blog", first.ID)
	if rb.ID != first.ID+1 {
		t.Fatalf("rollback ID = %d, want %d", rb.ID, first.ID+1)
	}
	got := h.log("blog", rb.ID)
	if !strings.HasPrefix(got, "Queued rollback to deployment #1 ") || strings.Contains(got, "left over") {
		t.Errorf("log:\n%s", got)
	}
	if !strings.Contains(got, "==> ") {
		t.Errorf("the worker's lines are missing:\n%s", got)
	}
}
