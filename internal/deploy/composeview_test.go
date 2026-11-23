package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// Values that must never reach any output of the view.
const (
	savedSecret   = "saved-variable-s3cret"
	envFileSecret = "only-in-the-env-file-s3cret"
)

// viewConfigJSON is a no-interpolate config output in which the repository
// repeats both secrets literally, in places the structural masking does
// and does not cover.
const viewConfigJSON = `{
  "name": "koroku-blog",
  "services": {
    "web": {
      "environment": {"API_KEY": "` + savedSecret + `", "PORT": "3000", "EMPTY": null},
      "build": {"context": ".", "args": {"TOKEN": "${TOKEN}"}},
      "labels": {"note": "` + envFileSecret + `"},
      "image": "koroku-blog-web:r1",
      "ports": [{"host_ip": "127.0.0.1", "target": 3000, "published": "20000"}]
    }
  }
}`

// viewHarness is a harness with two successful releases of blog and the
// view's commands scripted on top of the deploy simulation.
type viewHarness struct {
	*harness
	app           store.App
	first, second store.Deployment
	// script answers the view's commands; nil falls through to the deploy
	// simulation.
	script func(cmd runner.Cmd) (string, error, bool)
}

func newViewHarness(t *testing.T) *viewHarness {
	t.Helper()
	h := newHarness(t, nil)
	app := h.createApp("blog")
	h.start()
	h.setEnv(app, map[string]string{"API_KEY": savedSecret, "PORT": "3000"})
	first := h.deploy("blog", shaA)
	second := h.deploy("blog", shaB)
	if first.Status != store.StatusSucceeded || second.Status != store.StatusSucceeded {
		t.Fatalf("deploys: %s %s", first.Status, second.Status)
	}
	// A value that was deleted from the database but is still in the env
	// file of the running release.
	envFile := h.engine.EnvFile("blog")
	content, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, append(content, []byte("OLD_TOKEN="+envFileSecret+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	app, err = h.store.GetApp(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	v := &viewHarness{harness: h, app: app, first: first, second: second}
	h.runner.Handler = func(ctx context.Context, cmd runner.Cmd) (string, error) {
		if v.script != nil {
			if out, err, ok := v.script(cmd); ok {
				return out, err
			}
		}
		return h.handle(ctx, cmd)
	}
	h.runner.Reset()
	return v
}

func assertNoSecrets(t *testing.T, what string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{savedSecret, envFileSecret} {
		if strings.Contains(string(data), secret) {
			t.Errorf("%s contains %q: %s", what, secret, data)
		}
	}
}

func hasArgs(cmd runner.Cmd, want ...string) bool {
	for _, w := range want {
		if !slices.Contains(cmd.Args, w) {
			return false
		}
	}
	return true
}

func TestComposeViewOfTheCurrentRelease(t *testing.T) {
	h := newViewHarness(t)
	h.script = func(cmd runner.Cmd) (string, error, bool) {
		if hasArgs(cmd, "--no-interpolate") {
			return viewConfigJSON, nil, true
		}
		return "", nil, false
	}
	view, err := h.engine.ComposeView(context.Background(), h.app, 0)
	if err != nil {
		t.Fatalf("ComposeView: %v", err)
	}
	if view.DeploymentID != h.second.ID || view.CurrentDeploymentID == nil || *view.CurrentDeploymentID != h.second.ID {
		t.Errorf("deployment = %d, current = %v", view.DeploymentID, view.CurrentDeploymentID)
	}
	if len(view.Releases) != 2 || view.Releases[0].DeploymentID != h.second.ID || !view.Releases[0].Current ||
		view.Releases[1].DeploymentID != h.first.ID || view.Releases[1].Current || view.Releases[1].CommitSHA != shaA {
		t.Errorf("releases = %+v", view.Releases)
	}
	if !slices.Equal(view.Files, []string{"compose.yaml", compose.OverrideFileName}) {
		t.Errorf("files = %v", view.Files)
	}
	written, _ := os.ReadFile(filepath.Join(h.engine.ReleaseDir("blog", h.second.ID), compose.OverrideFileName))
	if view.Override != string(written) {
		t.Errorf("override differs from the file:\n%s", view.Override)
	}
	if view.Config == nil || view.ConfigError != "" {
		t.Fatalf("config = %v, error = %q", view.Config, view.ConfigError)
	}
	for _, want := range []string{`API_KEY: "[MASKED]"`, `TOKEN: "[MASKED]"`, "EMPTY: null", `note: "[MASKED]"`, `published: "20000"`} {
		if !strings.Contains(*view.Config, want) {
			t.Errorf("config lacks %q:\n%s", want, *view.Config)
		}
	}
	assertNoSecrets(t, "compose view", view)

	// The call is the deployment's: same project, files in order, env file
	// and directory, plus the flags that keep values out.
	dir := h.engine.ReleaseDir("blog", h.second.ID)
	want := []string{"compose", "-p", "koroku-blog",
		"-f", filepath.Join(dir, "compose.yaml"), "-f", filepath.Join(dir, compose.OverrideFileName),
		"--env-file", h.engine.EnvFile("blog"), "--env-file", h.engine.ReleaseEnvFile("blog"),
		"config", "--format", "json", "--no-interpolate", "--no-env-resolution"}
	calls := h.runner.Calls()
	if len(calls) != 1 || !slices.Equal(calls[0].Args, want) || calls[0].Dir != dir {
		t.Errorf("calls = %+v\nwant args %v in %s", calls, want, dir)
	}
}

func TestComposeViewOfAKeptRelease(t *testing.T) {
	h := newViewHarness(t)
	h.script = func(cmd runner.Cmd) (string, error, bool) {
		if hasArgs(cmd, "--no-interpolate") {
			return viewConfigJSON, nil, true
		}
		return "", nil, false
	}
	view, err := h.engine.ComposeView(context.Background(), h.app, h.first.ID)
	if err != nil {
		t.Fatalf("ComposeView: %v", err)
	}
	if view.DeploymentID != h.first.ID || *view.CurrentDeploymentID != h.second.ID {
		t.Errorf("deployment = %d, current = %d", view.DeploymentID, *view.CurrentDeploymentID)
	}
	if call := h.runner.Calls()[0]; call.Dir != h.engine.ReleaseDir("blog", h.first.ID) {
		t.Errorf("config ran in %s", call.Dir)
	}
	assertNoSecrets(t, "compose view", view)

	if _, err := h.engine.ComposeView(context.Background(), h.app, 999); !errors.Is(err, ErrReleaseUnavailable) {
		t.Errorf("unknown release: err = %v", err)
	}
	if err := os.RemoveAll(h.engine.ReleaseDir("blog", h.first.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.engine.ComposeView(context.Background(), h.app, h.first.ID); !errors.Is(err, ErrReleaseUnavailable) {
		t.Errorf("removed release: err = %v", err)
	}
}

func TestComposeViewNotDeployed(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	if _, err := h.engine.ComposeView(context.Background(), app, 0); !errors.Is(err, ErrNotDeployed) {
		t.Errorf("never deployed: err = %v", err)
	}
	v := newViewHarness(t)
	if err := os.RemoveAll(v.engine.ReleaseDir("blog", v.second.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := v.engine.ComposeView(context.Background(), v.app, 0); !errors.Is(err, ErrNotDeployed) {
		t.Errorf("files gone: err = %v", err)
	}
}

func TestComposeViewConfigFailure(t *testing.T) {
	h := newViewHarness(t)
	h.script = func(cmd runner.Cmd) (string, error, bool) {
		if hasArgs(cmd, "--no-interpolate") {
			if cmd.Stderr != nil {
				_, _ = cmd.Stderr.Write([]byte("service web: invalid value " + savedSecret + "\n"))
			}
			return "", &runner.ExitError{Cmd: "docker", Code: 1}, true
		}
		return "", nil, false
	}
	view, err := h.engine.ComposeView(context.Background(), h.app, 0)
	if err != nil {
		t.Fatalf("ComposeView: %v", err)
	}
	if view.Config != nil || !strings.Contains(view.ConfigError, "invalid value") || view.Override == "" {
		t.Errorf("config = %v, error = %q, override = %q", view.Config, view.ConfigError, view.Override)
	}
	assertNoSecrets(t, "compose view", view)
}

// inspectJSON describes the containers the fake Docker reports.
const inspectJSON = `[
  {"Id": "c-web", "Name": "/koroku-blog-web-1", "Created": "2026-11-18T03:00:30Z", "Image": "sha256:webimage",
   "State": {"Status": "running", "OOMKilled": true, "ExitCode": 0, "StartedAt": "2026-11-18T03:10:00Z",
             "FinishedAt": "2026-11-18T03:09:59Z", "Health": {"Status": "unhealthy"}},
   "RestartCount": 2,
   "HostConfig": {"Memory": 536870912, "MemoryReservation": 0, "OomScoreAdj": 500,
                  "PortBindings": {"3000/tcp": [{"HostIp": "127.0.0.1", "HostPort": "20000"}]}},
   "Config": {"Image": "koroku-blog-web:r2", "Env": ["API_KEY=` + savedSecret + `", "OLD_TOKEN=` + envFileSecret + `"],
              "Labels": {"com.docker.compose.service": "web", "com.docker.compose.config-hash": "hash-web"}},
   "NetworkSettings": {"Ports": {"3000/tcp": [{"HostIp": "127.0.0.1", "HostPort": "20000"}]}}},
  {"Id": "c-db", "Name": "/koroku-blog-db-1", "Created": "2026-11-18T03:00:10Z", "Image": "sha256:dbimage",
   "State": {"Status": "exited", "ExitCode": 137, "StartedAt": "2026-11-18T03:00:11Z", "FinishedAt": "0001-01-01T00:00:00Z"},
   "HostConfig": {"Memory": 268435456, "MemoryReservation": 0, "OomScoreAdj": 500},
   "Config": {"Image": "postgres:17", "Labels": {"com.docker.compose.service": "db", "com.docker.compose.config-hash": "stale"}},
   "NetworkSettings": {"Ports": {}}},
  {"Id": "c-old", "Name": "/koroku-blog-worker-1", "Created": "2026-11-18T03:00:05Z", "Image": "sha256:old",
   "State": {"Status": "running"}, "HostConfig": {},
   "Config": {"Image": "worker:1", "Labels": {"com.docker.compose.service": "worker", "com.docker.compose.config-hash": "x"}},
   "NetworkSettings": {}},
  {"Id": "c-run", "Name": "/koroku-blog-web-run-1", "Created": "2026-11-18T03:20:00Z", "Image": "sha256:webimage",
   "State": {"Status": "running"}, "HostConfig": {},
   "Config": {"Image": "koroku-blog-web:r2", "Labels": {"com.docker.compose.service": "web", "com.docker.compose.oneoff": "True"}},
   "NetworkSettings": {}}
]`

// expectedConfigJSON is the interpolated configuration of the current
// release: what it asks of each service.
const expectedConfigJSON = `{"name": "koroku-blog", "services": {
  "web": {"image": "koroku-blog-web:r2", "mem_limit": "536870912", "oom_score_adj": 500,
          "environment": {"API_KEY": "` + savedSecret + `"}},
  "db": {"image": "postgres:17", "deploy": {"resources": {"limits": {"memory": "268435456"}}}, "oom_score_adj": 500}
}}`

func containerScript(h *viewHarness) func(cmd runner.Cmd) (string, error, bool) {
	return func(cmd runner.Cmd) (string, error, bool) {
		switch {
		case len(cmd.Args) > 0 && cmd.Args[0] == "ps":
			return "c-web\nc-db\nc-old\nc-run\n", nil, true
		case len(cmd.Args) > 1 && cmd.Args[0] == "container" && cmd.Args[1] == "inspect":
			return inspectJSON, nil, true
		case len(cmd.Args) > 1 && cmd.Args[0] == "image" && cmd.Args[1] == "inspect":
			return `[{"Id": "sha256:dbimage", "RepoDigests": ["postgres@sha256:abc"]}, {"Id": "sha256:webimage", "RepoDigests": []}]`, nil, true
		case hasArgs(cmd, "--hash"):
			return "web hash-web\ndb hash-db\n", nil, true
		case hasArgs(cmd, "config", "--no-env-resolution"):
			return expectedConfigJSON, nil, true
		}
		return "", nil, false
	}
}

func TestContainersMarkMismatches(t *testing.T) {
	h := newViewHarness(t)
	h.script = containerScript(h)
	view, err := h.engine.Containers(context.Background(), h.app)
	if err != nil {
		t.Fatalf("Containers: %v", err)
	}
	if !view.Compared || view.ComparisonError != "" || *view.CurrentDeploymentID != h.second.ID {
		t.Fatalf("compared = %v (%q), current = %v", view.Compared, view.ComparisonError, view.CurrentDeploymentID)
	}
	names := []string{}
	for _, c := range view.Containers {
		names = append(names, c.Name)
	}
	// Sorted by service; the one-off command container is left out.
	if !slices.Equal(names, []string{"koroku-blog-db-1", "koroku-blog-web-1", "koroku-blog-worker-1"}) {
		t.Fatalf("containers = %v", names)
	}
	db, web, worker := view.Containers[0], view.Containers[1], view.Containers[2]

	if web.Current == nil || !*web.Current {
		t.Errorf("web should be from the current release: %+v", web.Current)
	}
	if !slices.Equal(web.Mismatches, []string{MismatchOOMKilled, MismatchRestarted, MismatchUnhealthy}) {
		t.Errorf("web mismatches = %v", web.Mismatches)
	}
	if web.DeploymentID == nil || *web.DeploymentID != 2 || web.MatchedBy != MatchedByImage {
		t.Errorf("web deployment = %v by %q", web.DeploymentID, web.MatchedBy)
	}
	if web.ImageDigest != nil || len(web.Ports) != 1 || web.Ports[0].HostPort != "20000" || web.Ports[0].ContainerPort != "3000/tcp" {
		t.Errorf("web image digest = %v, ports = %+v", web.ImageDigest, web.Ports)
	}
	if web.Expected == nil || web.Expected.MemoryLimitBytes != 512<<20 || web.Expected.OomScoreAdj != 500 {
		t.Errorf("web expected = %+v", web.Expected)
	}

	if db.Current == nil || *db.Current {
		t.Errorf("db should be from an older configuration")
	}
	if !slices.Equal(db.Mismatches, []string{MismatchOutdated, MismatchExited}) {
		t.Errorf("db mismatches = %v", db.Mismatches)
	}
	if db.ImageDigest == nil || *db.ImageDigest != "postgres@sha256:abc" || db.FinishedAt != nil || db.ExitCode != 137 {
		t.Errorf("db = %+v", db)
	}
	if db.MatchedBy != MatchedByCreatedAt || db.DeploymentID == nil {
		t.Errorf("db deployment = %v by %q", db.DeploymentID, db.MatchedBy)
	}

	if !slices.Contains(worker.Mismatches, MismatchOrphan) || worker.Expected != nil {
		t.Errorf("worker = %+v", worker)
	}
	assertNoSecrets(t, "container view", view)

	for _, c := range h.runner.Calls() {
		if c.Args[0] == "ps" && !hasArgs(c, "--all", "label=com.docker.compose.project=koroku-blog") {
			t.Errorf("ps args = %v", c.Args)
		}
	}
}

func TestContainersMarkLimitDifferences(t *testing.T) {
	h := newViewHarness(t)
	script := containerScript(h)
	h.script = func(cmd runner.Cmd) (string, error, bool) {
		if hasArgs(cmd, "config", "--no-env-resolution") && !hasArgs(cmd, "--hash") {
			return `{"name": "koroku-blog", "services": {
			  "web": {"mem_limit": "1073741824", "mem_reservation": "268435456", "oom_score_adj": -400},
			  "db": {"oom_score_adj": 500}}}`, nil, true
		}
		return script(cmd)
	}
	view, err := h.engine.Containers(context.Background(), h.app)
	if err != nil {
		t.Fatal(err)
	}
	web := view.Containers[1]
	for _, m := range []string{MismatchMemoryLimit, MismatchMemoryReservation, MismatchOomScoreAdj} {
		if !slices.Contains(web.Mismatches, m) {
			t.Errorf("web mismatches %v lack %s", web.Mismatches, m)
		}
	}
	// db has no limit in the release, and 0 is what Docker reports then;
	// the container's 256 MiB differs.
	if db := view.Containers[0]; !slices.Contains(db.Mismatches, MismatchMemoryLimit) {
		t.Errorf("db mismatches = %v", db.Mismatches)
	}
}

func TestContainersWithoutComparison(t *testing.T) {
	h := newViewHarness(t)
	script := containerScript(h)
	h.script = func(cmd runner.Cmd) (string, error, bool) {
		if hasArgs(cmd, "--hash") {
			if cmd.Stderr != nil {
				_, _ = cmd.Stderr.Write([]byte("broken: " + savedSecret + "\n"))
			}
			return "", &runner.ExitError{Cmd: "docker", Code: 1}, true
		}
		return script(cmd)
	}
	view, err := h.engine.Containers(context.Background(), h.app)
	if err != nil {
		t.Fatal(err)
	}
	if view.Compared || view.ComparisonError == "" {
		t.Errorf("compared = %v, error = %q", view.Compared, view.ComparisonError)
	}
	for _, c := range view.Containers {
		if c.Current != nil || c.Expected != nil || slices.Contains(c.Mismatches, MismatchOutdated) {
			t.Errorf("%s was compared: %+v", c.Name, c)
		}
	}
	// State markers do not need the release.
	if !slices.Contains(view.Containers[1].Mismatches, MismatchOOMKilled) {
		t.Errorf("web mismatches = %v", view.Containers[1].Mismatches)
	}
	assertNoSecrets(t, "container view", view)
}

func TestContainersOfAnAppWithoutContainers(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	view, err := h.engine.Containers(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if view.Compared || view.CurrentDeploymentID != nil || view.Containers == nil || len(view.Containers) != 0 {
		t.Errorf("view = %+v", view)
	}
}
