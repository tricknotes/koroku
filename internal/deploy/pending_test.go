package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// autoDeployApp is a deployed app that deploys pushes to main, with a
// command running on it until the returned function is called.
func autoDeployApp(t *testing.T) (h *harness, app store.App, run store.TaskRun, finish func()) {
	t.Helper()
	h, app = deployedApp(t)
	app.AutoDeploy = true
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	app = h.reload("blog")
	release := make(chan struct{})
	h.onRun(func(ctx context.Context, _ runner.Cmd) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	run, err := h.engine.RunTask(context.Background(), app, TaskSpec{TaskName: "report", Service: "web", Command: "bin/report"})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	var once bool
	finish = func() {
		if !once {
			once = true
			close(release)
			h.waitTask(run.ID)
		}
	}
	t.Cleanup(finish)
	return h, app, run, finish
}

func (h *harness) pending(appID int64) (store.PendingAutoDeploy, bool) {
	h.t.Helper()
	p, err := h.store.GetPendingAutoDeploy(context.Background(), appID)
	if errors.Is(err, store.ErrNotFound) {
		return store.PendingAutoDeploy{}, false
	}
	if err != nil {
		h.t.Fatalf("GetPendingAutoDeploy: %v", err)
	}
	return p, true
}

// deploymentsAfter returns the deployments of the app newer than id.
func (h *harness) deploymentsAfter(appID, id int64) []store.Deployment {
	h.t.Helper()
	list, err := h.store.ListDeployments(context.Background(), appID, 50)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []store.Deployment
	for _, d := range list {
		if d.ID > id {
			out = append(out, d)
		}
	}
	return out
}

// waitDeploymentsAfter waits until the app has n deployments newer than id.
// A run ends by unregistering itself and then queueing the deferred push,
// so the deployment can appear just after waitTask returns.
func (h *harness) waitDeploymentsAfter(appID, id int64, n int) []store.Deployment {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := h.deploymentsAfter(appID, id)
		if len(got) >= n || time.Now().After(deadline) {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) activity(appID int64) []store.Activity {
	h.t.Helper()
	list, err := h.store.ListActivity(context.Background(), appID, 0, 50)
	if err != nil {
		h.t.Fatal(err)
	}
	return list
}

func TestPushDuringACommandIsDeferredUntilItEnds(t *testing.T) {
	h, app, _, finish := autoDeployApp(t)
	before := *app.CurrentDeploymentID

	_, err := h.engine.EnqueuePush(context.Background(), app, "main", shaB, "Fix the footer", "octocat")
	if !errors.Is(err, ErrDeferred) {
		t.Fatalf("EnqueuePush = %v, want ErrDeferred", err)
	}
	p, ok := h.pending(app.ID)
	if !ok || p.CommitSHA != shaB || p.Ref != "main" || p.Pusher != "octocat" || p.CommitMessage != "Fix the footer" {
		t.Fatalf("pending = %+v, %v", p, ok)
	}
	if got := h.deploymentsAfter(app.ID, before); len(got) != 0 {
		t.Fatalf("nothing may be queued while the command runs: %+v", got)
	}

	finish()
	got := h.waitDeploymentsAfter(app.ID, before, 1)
	if len(got) != 1 {
		t.Fatalf("deployments after the command = %+v", got)
	}
	d := h.wait(got[0].ID)
	if d.Status != store.StatusSucceeded || d.CommitSHA != shaB || d.Trigger != store.TriggerPush || d.Pusher != "octocat" {
		t.Fatalf("deployment = %+v", d)
	}
	log := h.log("blog", d.ID)
	for _, want := range []string{
		"Queued deployment of " + shaB + ", pushed to main by octocat.\n",
		"Deferred while a command was running on the app; queued when it ended.\n",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if _, ok := h.pending(app.ID); ok {
		t.Error("the pending auto-deploy must be gone once queued")
	}
}

func TestWorkflowRunDuringACommandIsDeferred(t *testing.T) {
	h, app, _, finish := autoDeployApp(t)
	app.AutoDeployWorkflow = ".github/workflows/image.yml"
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	app = h.reload("blog")
	before := *app.CurrentDeploymentID
	if _, err := h.engine.EnqueueWorkflowRun(context.Background(), app, "main", shaB, "m", "octocat", app.AutoDeployWorkflow); !errors.Is(err, ErrDeferred) {
		t.Fatalf("EnqueueWorkflowRun = %v", err)
	}
	finish()
	got := h.waitDeploymentsAfter(app.ID, before, 1)
	if len(got) != 1 {
		t.Fatalf("deployments = %+v", got)
	}
	d := h.wait(got[0].ID)
	if !strings.Contains(h.log("blog", d.ID), "after workflow .github/workflows/image.yml succeeded.") {
		t.Errorf("log:\n%s", h.log("blog", d.ID))
	}
}

func TestLaterPushReplacesTheDeferredOne(t *testing.T) {
	h, app, _, finish := autoDeployApp(t)
	before := *app.CurrentDeploymentID
	for _, sha := range []string{shaB, shaC} {
		h.clock.Advance(time.Minute)
		if _, err := h.engine.EnqueuePush(context.Background(), app, "main", sha, "", "octocat"); !errors.Is(err, ErrDeferred) {
			t.Fatalf("EnqueuePush(%s) = %v", sha, err)
		}
	}
	if p, _ := h.pending(app.ID); p.CommitSHA != shaC {
		t.Fatalf("pending = %+v, want the later push", p)
	}
	finish()
	got := h.waitDeploymentsAfter(app.ID, before, 1)
	if len(got) != 1 || got[0].CommitSHA != shaC {
		t.Fatalf("deployments = %+v, want only the later push", got)
	}
	h.wait(got[0].ID)
}

func TestManualDeployDuringACommandIsStillBusy(t *testing.T) {
	h, app, _, finish := autoDeployApp(t)
	before := *app.CurrentDeploymentID
	if _, err := h.engine.Enqueue(context.Background(), app, "main", shaB, ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("Enqueue = %v, want ErrBusy", err)
	}
	if _, err := h.engine.EnqueueRollback(context.Background(), app, before); !errors.Is(err, ErrBusy) {
		t.Fatalf("EnqueueRollback = %v, want ErrBusy", err)
	}
	if _, ok := h.pending(app.ID); ok {
		t.Fatal("a manual deployment must not be kept")
	}
	finish()
	if got := h.deploymentsAfter(app.ID, before); len(got) != 0 {
		t.Fatalf("deployments = %+v", got)
	}
}

func TestDeferredPushIsDroppedWhenSettingsChanged(t *testing.T) {
	cases := []struct {
		name   string
		change func(*store.App)
		reason string
	}{
		{"auto-deploy off", func(a *store.App) { a.AutoDeploy = false }, DropAutoDeployDisabled},
		{"stopped", func(a *store.App) { a.DesiredState = store.DesiredStopped }, DropStopped},
		{"other branch", func(a *store.App) { a.AutoDeployBranch = "release" }, DropBranchChanged},
		{"now waits for a workflow", func(a *store.App) { a.AutoDeployWorkflow = ".github/workflows/image.yml" }, DropWorkflowChanged},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, app, _, finish := autoDeployApp(t)
			before := *app.CurrentDeploymentID
			if _, err := h.engine.EnqueuePush(context.Background(), app, "main", shaB, "", "octocat"); !errors.Is(err, ErrDeferred) {
				t.Fatalf("EnqueuePush = %v", err)
			}
			changed := h.reload("blog")
			tc.change(&changed)
			if err := h.store.UpdateApp(context.Background(), changed, h.clock.Now()); err != nil {
				t.Fatal(err)
			}
			if changed.DesiredState == store.DesiredStopped {
				if err := h.store.SetAppDesiredState(context.Background(), app.ID, store.DesiredStopped, h.clock.Now()); err != nil {
					t.Fatal(err)
				}
			}
			finish()
			if got := h.deploymentsAfter(app.ID, before); len(got) != 0 {
				t.Fatalf("deployments = %+v", got)
			}
			if _, ok := h.pending(app.ID); ok {
				t.Error("a dropped auto-deploy must be forgotten")
			}
			assertDropped(t, h.activity(app.ID), tc.reason)
		})
	}
}

func assertDropped(t *testing.T, list []store.Activity, reason string) {
	t.Helper()
	for _, a := range list {
		if a.Action != ActAutoDeployDropped {
			continue
		}
		var details map[string]string
		if err := json.Unmarshal(a.Details, &details); err != nil {
			t.Fatal(err)
		}
		if a.Actor != nil || details["reason"] != reason || details["commit_sha"] != shaB ||
			details["branch"] != "main" || details["pusher"] != "octocat" {
			t.Errorf("activity = %+v %v", a, details)
		}
		return
	}
	t.Errorf("no %s activity in %+v", ActAutoDeployDropped, list)
}

func TestDeferredPushIsDroppedWhenTheDiskIsFull(t *testing.T) {
	h, app, _, finish := autoDeployApp(t)
	before := *app.CurrentDeploymentID
	if _, err := h.engine.EnqueuePush(context.Background(), app, "main", shaB, "", "octocat"); !errors.Is(err, ErrDeferred) {
		t.Fatalf("EnqueuePush = %v", err)
	}
	h.prober.mu.Lock()
	h.prober.percent = 95
	h.prober.mu.Unlock()
	finish()
	if got := h.deploymentsAfter(app.ID, before); len(got) != 0 {
		t.Fatalf("deployments = %+v", got)
	}
	if _, ok := h.pending(app.ID); ok {
		t.Error("a dropped auto-deploy must be forgotten")
	}
	assertDropped(t, h.activity(app.ID), DropDiskFull)
}

func TestNewerDeploymentDiscardsTheDeferredPush(t *testing.T) {
	h, app := deployedApp(t)
	before := *app.CurrentDeploymentID
	// Kept before a restart; a deployment was queued since.
	if err := h.store.PutPendingAutoDeploy(context.Background(), store.PendingAutoDeploy{
		AppID: app.ID, Ref: "main", CommitSHA: shaB, ReceivedAt: h.clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	d := h.deploy("blog", shaC)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	if _, ok := h.pending(app.ID); ok {
		t.Error("a newer deployment must discard the deferred push")
	}
	if got := h.deploymentsAfter(app.ID, before); len(got) != 1 {
		t.Errorf("deployments = %+v", got)
	}
}

func TestDeferredPushIsQueuedOnStart(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	app.AutoDeploy = true
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	// Kept by the previous process, whose command was then interrupted.
	if err := h.store.PutPendingAutoDeploy(context.Background(), store.PendingAutoDeploy{
		AppID: app.ID, Ref: "main", CommitSHA: shaA, Pusher: "octocat", ReceivedAt: h.clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	h.start()
	deadline := time.Now().Add(5 * time.Second)
	var got []store.Deployment
	for time.Now().Before(deadline) {
		if got = h.deploymentsAfter(app.ID, 0); len(got) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(got) != 1 || got[0].CommitSHA != shaA || got[0].Trigger != store.TriggerPush {
		t.Fatalf("deployments = %+v", got)
	}
	if d := h.wait(got[0].ID); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	if _, ok := h.pending(app.ID); ok {
		t.Error("the pending auto-deploy must be gone once queued")
	}
}
