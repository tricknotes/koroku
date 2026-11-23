//go:build integration

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// containerEnv returns the environment of the container of service in
// project, one VAR=value per line.
func containerEnv(t *testing.T, project, service string) string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "--all", "--quiet",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service).Output()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		t.Fatalf("no container of %s: %v", service, err)
	}
	id := strings.Fields(string(out))[0]
	env, err := exec.Command("docker", "inspect", "--format", "{{range .Config.Env}}{{println .}}{{end}}", id).Output()
	if err != nil {
		t.Fatalf("docker inspect %s: %v", service, err)
	}
	return string(env)
}

// TestReleaseEnvKeepsTheDatabase: the commit variables reach only the
// services that every deployment recreates (ARCHITECTURE 6), so code-only
// deploys of a plain app keep the database container, and so do the
// deploys that turn blue/green on and off. A changed variable recreates it,
// and the log names the variable.
func TestReleaseEnvKeepsTheDatabase(t *testing.T) {
	t.Parallel()
	s := newBGStack(t)
	ctx := context.Background()
	repo := newRepo(t, "bluegreen-app")
	first := repo.commit("version 1")

	portStart, portEnd := portRange(t)
	if _, err := s.db.CreateApp(ctx, store.App{
		Slug: s.slug, RepoURL: repo.url(), DefaultBranch: "main",
		WebService: "web", WebPort: 8080, HealthcheckPath: "/",
	}, portStart, portEnd, time.Now()); err != nil {
		t.Fatal(err)
	}
	project := "koroku-" + s.slug

	d1 := s.wait(s.enqueue(first).ID)
	if d1.Status != store.StatusSucceeded || d1.WebColor != "" {
		t.Fatalf("first deployment: %+v\n%s", d1, s.log(d1.ID))
	}
	dbBefore := s.containerCreated("db")
	if env := containerEnv(t, project, "db"); strings.Contains(env, "KOROKU_COMMIT_SHA") {
		t.Errorf("the database received the commit:\n%s", env)
	}
	if env := containerEnv(t, project, "web"); !strings.Contains(env, "KOROKU_COMMIT_SHA="+first) {
		t.Errorf("the public service lacks the commit:\n%s", env)
	}

	// (1) Code only, plain: the database container stays.
	repo.write("VERSION", "2\n")
	second := repo.commit("version 2")
	d2 := s.wait(s.enqueue(second).ID)
	if d2.Status != store.StatusSucceeded {
		t.Fatalf("code-only deployment: %+v\n%s", d2, s.log(d2.ID))
	}
	if after := s.containerCreated("db"); after != dbBefore || after == "" {
		t.Errorf("a code-only deploy recreated the database: %q -> %q\n%s", dbBefore, after, s.log(d2.ID))
	}
	if env := containerEnv(t, project, "web"); !strings.Contains(env, "KOROKU_COMMIT_SHA="+second) {
		t.Errorf("the public service did not get the new commit:\n%s", env)
	}
	if log := s.log(d2.ID); strings.Contains(log, "Recreating db") {
		t.Errorf("log:\n%s", log)
	}

	// (2) Turning blue/green on: the switch leaves the database alone.
	app := s.reload()
	app.BlueGreen = true
	app.BlueGreenDrainSeconds = 1
	if err := s.db.UpdateApp(ctx, app, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.AllocateAltHostPort(ctx, app.ID, portStart, portEnd); err != nil {
		t.Fatal(err)
	}
	repo.write("VERSION", "3\n")
	d3 := s.wait(s.enqueue(repo.commit("version 3")).ID)
	if d3.Status != store.StatusSucceeded || d3.WebColor != store.ColorGreen || d3.SwitchedAt == nil {
		t.Fatalf("enabling deployment: %+v\n%s", d3, s.log(d3.ID))
	}
	if after := s.containerCreated("db"); after != dbBefore {
		t.Errorf("turning blue/green on recreated the database: %q -> %q\n%s", dbBefore, after, s.log(d3.ID))
	}

	// (3) And off again.
	app = s.reload()
	app.BlueGreen = false
	if err := s.db.UpdateApp(ctx, app, time.Now()); err != nil {
		t.Fatal(err)
	}
	repo.write("VERSION", "4\n")
	d4 := s.wait(s.enqueue(repo.commit("version 4")).ID)
	if d4.Status != store.StatusSucceeded || d4.WebColor != "" {
		t.Fatalf("disabling deployment: %+v\n%s", d4, s.log(d4.ID))
	}
	if after := s.containerCreated("db"); after != dbBefore {
		t.Errorf("turning blue/green off recreated the database: %q -> %q\n%s", dbBefore, after, s.log(d4.ID))
	}

	// (4) A changed variable recreates it, and says which one.
	sealed, err := s.opts.Box.Seal([]byte("hello"), secrets.EnvVarContext(app.ID, "GREETING"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.ReplaceEnvVars(ctx, app.ID, []store.EnvVar{{Key: "GREETING", ValueEncrypted: sealed}}); err != nil {
		t.Fatal(err)
	}
	repo.write("VERSION", "5\n")
	d5 := s.wait(s.enqueue(repo.commit("version 5")).ID)
	if d5.Status != store.StatusSucceeded {
		t.Fatalf("env deployment: %+v\n%s", d5, s.log(d5.ID))
	}
	if after := s.containerCreated("db"); after == dbBefore {
		t.Errorf("a changed variable did not recreate the database\n%s", s.log(d5.ID))
	}
	if log := s.log(d5.ID); !strings.Contains(log, "Recreating db because the app's env file changed (GREETING)") {
		t.Errorf("log:\n%s", log)
	}
}

// TestCommitVariablesOptIn: a service that does not build reads the commit
// only by interpolating it; koroku then gives it the release env file too.
func TestCommitVariablesOptIn(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	r := newRepo(t, "")
	r.write("compose.yaml", `services:
  web:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sh", "-c", "mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 8080 -h /www"]
  reporter:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sleep", "infinity"]
    environment:
      KOROKU_COMMIT_SHA: ${KOROKU_COMMIT_SHA}
  cache:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sleep", "infinity"]
`)
	sha := r.commit("Opt in")
	slug := e.slug("optin")
	e.createApp(slug, r.url())
	d := e.deploy(slug, map[string]string{"sha": sha})
	if d.Status != "succeeded" {
		t.Fatalf("deploy %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	project := "koroku-" + slug
	if env := containerEnv(t, project, "reporter"); !strings.Contains(env, "KOROKU_COMMIT_SHA="+sha) || !strings.Contains(env, "KOROKU_COMMIT_SHORT="+sha[:7]) {
		t.Errorf("reporter env:\n%s", env)
	}
	if env := containerEnv(t, project, "cache"); strings.Contains(env, "KOROKU_COMMIT") {
		t.Errorf("cache env:\n%s", env)
	}
	if log := e.logText(d.ID); !strings.Contains(log, "Services that follow the commit: reporter") {
		t.Errorf("log:\n%s", log)
	}
}
