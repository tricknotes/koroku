package deploy

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// TestFailedCheckCopiesContainerOutputIntoTheLog: when the new release does
// not answer, what its containers printed is the reason. It has to be in the
// deploy log, inside the failed step and with secrets masked, because the
// rollback replaces those containers right afterwards.
func TestFailedCheckCopiesContainerOutputIntoTheLog(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	h.setEnv(app, map[string]string{"DATABASE_PASSWORD": "s3cr3t-pa55word"})
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub == "logs" {
			_, _ = io.WriteString(cmd.Stdout, "web-1  | connecting as app:s3cr3t-pa55word\nweb-1  | fatal: could not connect to the database\n")
		}
		return nil
	}
	h.start()
	h.deploy("blog", shaA)

	h.runner.Reset()
	h.health.mu.Lock()
	h.health.err = errors.New("health check did not pass within 1m0s (last result: connection failed)")
	h.health.mu.Unlock()

	d := h.deploy("blog", shaB)
	if d.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", d)
	}

	logs := h.lastCall("logs")
	if want := []string{"compose", "-p", "koroku-blog", "logs", "--no-color", "--tail", "30"}; !slices.Equal(logs.Args, want) {
		t.Errorf("logs args = %q, want %q", logs.Args, want)
	}

	log := h.log("blog", d.ID)
	if strings.Contains(log, "s3cr3t-pa55word") {
		t.Errorf("the deploy log holds an environment value:\n%s", log)
	}
	check := strings.Index(log, " check ")
	output := strings.Index(log, "fatal: could not connect to the database")
	failed := strings.Index(log, " failed ")
	if check < 0 || output < 0 || failed < 0 || !(check < output && output < failed) {
		t.Errorf("want the container output between the check and the failed headline (check %d, output %d, failed %d):\n%s",
			check, output, failed, log)
	}
	if !strings.Contains(log, "Last output of the containers:") {
		t.Errorf("log lacks the introduction of the container output:\n%s", log)
	}
}

// TestUnreadableContainerOutputDoesNotChangeTheOutcome: the copy is a
// convenience. When it fails, the deployment fails for its own reason and
// the rollback still runs.
func TestUnreadableContainerOutputDoesNotChangeTheOutcome(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "logs" {
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.start()
	first := h.deploy("blog", shaA)

	h.runner.Reset()
	h.health.mu.Lock()
	h.health.err = errors.New("health check did not pass within 1m0s (last result: HTTP 502)")
	h.health.mu.Unlock()

	d := h.deploy("blog", shaB)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "HTTP 502") {
		t.Fatalf("deployment = %+v", d)
	}
	log := h.log("blog", d.ID)
	for _, want := range []string{"Reading the container output failed", "is running again"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	got, err := h.store.GetApp(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentDeploymentID == nil || *got.CurrentDeploymentID != first.ID {
		t.Errorf("current deployment must stay #%d, got %v", first.ID, got.CurrentDeploymentID)
	}
}
