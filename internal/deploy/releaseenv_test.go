package deploy

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// The commit variables live in env.release for every app, and only the
// services that every deployment recreates read it (ARCHITECTURE 6).

// overrideBlock returns the lines of one service in a release's override.
func overrideBlock(t *testing.T, h *harness, id int64, service string) string {
	t.Helper()
	out := readFile(t, filepath.Join(h.engine.ReleaseDir("blog", id), compose.OverrideFileName))
	start := strings.Index(out, "  \""+service+"\":\n")
	if start < 0 {
		t.Fatalf("override has no %s:\n%s", service, out)
	}
	rest := out[start+1:]
	if end := strings.Index(rest, "\n  \""); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

func TestPlainAppSplitsTheCommitVariables(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	h.setEnv(app, map[string]string{"GREETING": "hello"})
	h.start()
	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, h.log("blog", d.ID))
	}
	env := readFile(t, h.engine.EnvFile("blog"))
	release := readFile(t, h.engine.ReleaseEnvFile("blog"))
	if strings.Contains(env, "KOROKU_COMMIT") || !strings.Contains(env, "GREETING=hello") {
		t.Errorf("env:\n%s", env)
	}
	if release != "KOROKU_COMMIT_SHA="+shaA+"\nKOROKU_COMMIT_SHORT="+shaA[:7]+"\n" {
		t.Errorf("release env:\n%s", release)
	}
	// The snapshots are what a rollback restores.
	if got := readFile(t, h.engine.releaseEnvSnapshot("blog", d.ID)); got != release {
		t.Errorf("release env snapshot = %q", got)
	}
	// The built public service reads the commit; the database does not.
	if web := overrideBlock(t, h, d.ID, "web"); !strings.Contains(web, h.engine.ReleaseEnvFile("blog")) {
		t.Errorf("web:\n%s", web)
	}
	if db := overrideBlock(t, h, d.ID, "db"); strings.Contains(db, "env.release") {
		t.Errorf("db:\n%s", db)
	}
	meta, err := h.engine.readMeta("blog", d.ID)
	if err != nil || !slices.Equal(meta.ReleaseEnvServices, []string{"web"}) || meta.BlueGreen {
		t.Errorf("meta = %+v, %v", meta, err)
	}
	// Every call on the release interpolates with both files, and the
	// probe for services that follow the commit ran once.
	probes := 0
	for _, c := range h.runner.Calls() {
		if c.Name != "docker" {
			continue
		}
		if sub, merged := composeSubcommand(c.Args); sub == "up" && (!merged || !slices.Contains(c.Args, h.engine.ReleaseEnvFile("blog"))) {
			t.Errorf("up args = %v", c.Args)
		}
		for _, a := range c.Args {
			if strings.HasSuffix(a, "env.probe") {
				probes++
			}
		}
		if slices.Contains(c.Args, compose.DisabledProfile) {
			t.Errorf("a plain release activated the disabled profile: %v", c.Args)
		}
	}
	if probes != 1 {
		t.Errorf("probes = %d", probes)
	}
}

func TestPlainAppExplainsRecreationOnlyWhenTheEnvChanged(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	h.setEnv(app, map[string]string{"GREETING": "hello"})
	h.start()
	h.deploy("blog", shaA)

	// Code only: the database's env file is the same.
	second := h.deploy("blog", shaB)
	if log := h.log("blog", second.ID); second.Status != store.StatusSucceeded || strings.Contains(log, "Recreating") {
		t.Errorf("code-only deploy %s:\n%s", second.Status, log)
	}
	if db1, db2 := overrideBlock(t, h, 1, "db"), overrideBlock(t, h, second.ID, "db"); db1 != db2 {
		t.Errorf("db override changed:\n%s\n---\n%s", db1, db2)
	}

	// A changed variable recreates the database, and the log names the
	// variable, never its value.
	h.setEnv(app, map[string]string{"GREETING": "s3cret-greeting", "EXTRA": "1"})
	third := h.deploy("blog", shaC)
	log := h.log("blog", third.ID)
	want := "Recreating db because the app's env file changed (EXTRA, GREETING); requests may fail while it restarts."
	if !strings.Contains(log, want) || strings.Contains(log, "Recreating web") || strings.Contains(log, "s3cret-greeting") {
		t.Errorf("log:\n%s", log)
	}
}

// A release from before the split keeps the commit variables in env and
// has no env.release snapshot.
func makeReleaseOld(t *testing.T, h *harness, id int64, sha string) {
	t.Helper()
	state := h.engine.stateDir("blog", id)
	env := readFile(t, filepath.Join(state, "env"))
	env += "KOROKU_COMMIT_SHA=" + sha + "\nKOROKU_COMMIT_SHORT=" + sha[:7] + "\n"
	if err := os.WriteFile(filepath.Join(state, "env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(h.engine.releaseEnvSnapshot("blog", id)); err != nil {
		t.Fatal(err)
	}
}

func TestDeployAfterAReleaseWithoutTheReleaseEnv(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	first := h.deploy("blog", shaA)
	makeReleaseOld(t, h, first.ID, shaA)

	// The database loses the commit variables once, and the log says so.
	second := h.deploy("blog", shaB)
	want := "Recreating db because the app's env file changed (KOROKU_COMMIT_SHA, KOROKU_COMMIT_SHORT); requests may fail while it restarts."
	if log := h.log("blog", second.ID); second.Status != store.StatusSucceeded || !strings.Contains(log, want) {
		t.Errorf("deployment %s, log:\n%s", second.Status, log)
	}

	// Rolling back to it restores its env and leaves env.release alone,
	// which its override does not read.
	h.runner.Reset()
	back := h.rollback("blog", first.ID)
	if back.Status != store.StatusSucceeded {
		t.Fatalf("rollback = %+v\n%s", back, h.log("blog", back.ID))
	}
	up := h.lastCall("up")
	if slices.Contains(up.Args, h.engine.ReleaseEnvFile("blog")) {
		t.Errorf("up args = %v", up.Args)
	}
	if env := readFile(t, h.engine.EnvFile("blog")); !strings.Contains(env, "KOROKU_COMMIT_SHA="+shaA) {
		t.Errorf("env = %s", env)
	}
	if release := readFile(t, h.engine.ReleaseEnvFile("blog")); !strings.Contains(release, shaB) {
		t.Errorf("release env = %s", release)
	}
}

func TestPlainRollbacksRestoreTheReleaseEnv(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	first := h.deploy("blog", shaA)
	h.deploy("blog", shaB)
	if release := readFile(t, h.engine.ReleaseEnvFile("blog")); !strings.Contains(release, shaB) {
		t.Fatalf("release env = %s", release)
	}

	// A manual rollback brings back the kept release's commit variables.
	back := h.rollback("blog", first.ID)
	if back.Status != store.StatusSucceeded {
		t.Fatalf("rollback = %+v\n%s", back, h.log("blog", back.ID))
	}
	if release := readFile(t, h.engine.ReleaseEnvFile("blog")); !strings.Contains(release, shaA) {
		t.Errorf("after the rollback, release env = %s", release)
	}

	// The automatic rollback after a failed start does too.
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub == "up" && slices.Contains(cmd.Args, h.engine.ReleaseDir("blog", 4)+"/compose.yaml") {
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.mu.Unlock()
	failed := h.deploy("blog", shaC)
	if failed.Status != store.StatusFailed || failed.ID != 4 {
		t.Fatalf("deployment = %+v\n%s", failed, h.log("blog", failed.ID))
	}
	if release := readFile(t, h.engine.ReleaseEnvFile("blog")); !strings.Contains(release, shaA) {
		t.Errorf("after the automatic rollback, release env = %s", release)
	}
}

func TestServiceThatFollowsTheCommitReadsTheReleaseEnv(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.mu.Lock()
	h.servicesFor = func(cmd runner.Cmd) string {
		sha := shaA
		for _, a := range cmd.Args {
			if strings.HasSuffix(a, "env.probe") {
				sha = probeCommit
			}
		}
		return `{
	"web": {"build": {"context": "."}, "ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}]},
	"reporter": {"image": "example/reporter:1", "environment": {"KOROKU_COMMIT_SHA": "` + sha + `"}},
	"db": {"image": "postgres:17"}
}`
	}
	h.mu.Unlock()
	h.start()
	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, h.log("blog", d.ID))
	}
	if log := h.log("blog", d.ID); !strings.Contains(log, "Services that follow the commit: reporter") {
		t.Errorf("log:\n%s", log)
	}
	if reporter := overrideBlock(t, h, d.ID, "reporter"); !strings.Contains(reporter, "env.release") {
		t.Errorf("reporter:\n%s", reporter)
	}
	if db := overrideBlock(t, h, d.ID, "db"); strings.Contains(db, "env.release") {
		t.Errorf("db:\n%s", db)
	}
}

// Turning blue/green on leaves the services other than the public one with
// the same configuration: the deploy that moves to the colours does not
// recreate the database.
func TestBlueGreenTransitionKeepsTheOtherServices(t *testing.T) {
	b := newBGHarness(t, false)
	first := b.deploy("blog", shaA)
	if first.Status != store.StatusSucceeded {
		t.Fatalf("first = %+v", first)
	}
	b.setBlueGreen(true)
	second := b.deploy("blog", shaB)
	if second.Status != store.StatusSucceeded || second.WebColor != store.ColorGreen {
		t.Fatalf("second = %+v\n%s", second, b.log("blog", second.ID))
	}
	// The built worker and migration get a new image either way.
	if a, c := overrideBlock(t, b.harness, first.ID, "db"), overrideBlock(t, b.harness, second.ID, "db"); a != c {
		t.Errorf("db changed:\n%s\n---\n%s", a, c)
	}
	if log := b.log("blog", second.ID); strings.Contains(log, "Recreating db") {
		t.Errorf("log:\n%s", log)
	}
	// And back.
	b.setBlueGreen(false)
	third := b.deploy("blog", shaC)
	if third.Status != store.StatusSucceeded {
		t.Fatalf("third = %+v\n%s", third, b.log("blog", third.ID))
	}
	if a, c := overrideBlock(t, b.harness, first.ID, "db"), overrideBlock(t, b.harness, third.ID, "db"); a != c {
		t.Errorf("db changed:\n%s\n---\n%s", a, c)
	}
	if log := b.log("blog", third.ID); strings.Contains(log, "Recreating db") {
		t.Errorf("log:\n%s", log)
	}
}
