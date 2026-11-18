package deploy

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// tamper makes the simulated Compose project of the harness violate the
// policy from now on, as if the files of a stored release had been changed
// after the deploy that checked them.
func (h *harness) tamper() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.repoServices = `{"web": {"image": "example/app", "privileged": true,
		"ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}]}}`
}

// TestStartRevalidatesTheStoredRelease: Start reuses the files of the current
// release. If they no longer pass the policy, nothing is started.
func TestStartRevalidatesTheStoredRelease(t *testing.T) {
	h, app := deployedApp(t)
	ctx := context.Background()
	if err := h.engine.Stop(ctx, app); err != nil {
		t.Fatal(err)
	}
	app = h.reload("blog")
	h.runner.Reset()

	// Untouched files: validated, then started.
	if err := h.engine.Start(ctx, app); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config", "up"}) {
		t.Errorf("docker calls = %v, want a config check before up", got)
	}

	_ = h.engine.Stop(ctx, h.reload("blog"))
	h.tamper()
	h.runner.Reset()
	err := h.engine.Start(ctx, h.reload("blog"))
	var policyErr *compose.PolicyError
	if !errors.As(err, &policyErr) || policyErr.Violations[0].Rule != compose.RulePrivileged {
		t.Fatalf("Start of a tampered release: err = %v", err)
	}
	if slices.Contains(h.dockerCalls(), "up") {
		t.Errorf("a tampered release was started: %v", h.dockerCalls())
	}
	if got := h.reload("blog"); got.DesiredState != store.DesiredStopped {
		t.Errorf("DesiredState = %q", got.DesiredState)
	}
}

// TestRollbackRevalidatesThePreviousRelease: a rollback must not bring up a
// previous release whose files were changed after it was deployed.
func TestRollbackRevalidatesThePreviousRelease(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	h.deploy("blog", shaA)

	// The next deploy passes its own checks (the hook flips the simulated
	// project only once its `up` has failed), then needs a rollback.
	h.runner.Reset()
	upCalls := 0
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "up" {
			upCalls++
			go h.tamper()
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.mu.Unlock()
	second := h.deploy("blog", shaB)
	if second.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", second)
	}
	log := h.log("blog", second.ID)
	if !strings.Contains(log, "no longer passes validation") {
		t.Errorf("log:\n%s", log)
	}
	if upCalls != 1 {
		t.Errorf("up ran %d times; the tampered previous release must not be started", upCalls)
	}
}

func TestDeployLogIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deploy.log")
	log, err := openDeployLog(path, secrets.NewMasker(), nil)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 1<<20)
	line = append(line, '\n')
	for i := 0; i < 60; i++ { // 60 MiB of output
		if _, err := log.Writer().Write(line); err != nil {
			t.Fatalf("writing to the log must never fail: %v", err)
		}
	}
	log.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > maxDeployLogBytes+1024 {
		t.Errorf("log size = %d, want at most about %d", info.Size(), maxDeployLogBytes)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data[len(data)-200:], []byte("[log truncated")) {
		t.Error("the truncation is not noted at the end of the log")
	}
	if bytes.Count(data, []byte("[log truncated")) != 1 {
		t.Error("the truncation note must appear once")
	}
}
