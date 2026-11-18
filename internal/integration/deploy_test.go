//go:build integration

// Package integration drives the deploy engine against a real Docker daemon
// through the HTTP API. Run with: go test -tags integration ./...
package integration

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/api"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
	"github.com/tricknotes/koroku/internal/system"
)

const samples = "../../testdata/apps"

type env struct {
	t      *testing.T
	server *httptest.Server
	engine *deploy.Engine
	suffix string
}

// newEnv starts a paasd-like stack (store, engine, worker, API) that talks
// to the local Docker daemon. Everything it creates is removed afterwards.
func newEnv(t *testing.T) *env {
	t.Helper()
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose is not available: %v: %s", err, out)
	}
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()

	st, err := store.Open(ctx, filepath.Join(dir, "paas.db"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, secrets.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := secrets.NewBox(key)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.BaseDomain = "dev.example.com"
	cfg.DataDir = dir
	cfg.Deploy.AllowFileRepos = true
	cfg.Deploy.PortRangeStart = freePort(t)
	cfg.Deploy.PortRangeEnd = cfg.Deploy.PortRangeStart + 50

	run := runner.Exec{}
	prober := system.Host{DataDir: dir, Runner: run}
	fetcher := gitrepo.Fetcher{Runner: run, AllowFileURLs: true}
	engine := deploy.New(deploy.Options{
		Store:   st,
		Runner:  run,
		Clock:   clock.System{},
		Box:     box,
		System:  prober,
		Fetcher: fetcher,
		AppsDir: cfg.AppsDir(),
		// Never fail on this machine's disk usage and never prune images
		// that belong to someone else's work.
		DiskLimitPercent: 100,
		PruneImages:      false,
		WaitTimeout:      90 * time.Second,
		HealthTimeout:    20 * time.Second,
	})
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		_ = engine.Run(ctx)
	}()

	srv := api.New(api.Options{
		Config:   cfg,
		Store:    st,
		System:   prober,
		Clock:    clock.System{},
		Box:      box,
		Deployer: engine,
		Ops:      engine,
		Refs:     deploy.GitRefResolver{Fetcher: fetcher},
		// Authentication is covered by the API unit tests.
		Auth: api.AuthOptions{InsecureNoAuth: true},
	})
	server := httptest.NewServer(srv.Handler())

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	e := &env{t: t, server: server, engine: engine, suffix: hex.EncodeToString(suffix)}
	t.Cleanup(func() {
		server.CloseClientConnections()
		server.Close()
		cancel()
		<-workerDone
		_ = st.Close()
	})
	return e
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port
	if port > 60000 {
		port -= 5000
	}
	return port
}

// slug returns a unique app slug and registers removal of its Compose project.
func (e *env) slug(name string) string {
	e.t.Helper()
	slug := fmt.Sprintf("it-%s-%s", name, e.suffix)
	e.t.Cleanup(func() {
		out, err := exec.Command("docker", "compose", "-p", "paas-"+slug,
			"down", "--volumes", "--remove-orphans", "--rmi", "local", "--timeout", "1").CombinedOutput()
		if err != nil {
			e.t.Logf("cleanup of paas-%s failed: %v\n%s", slug, err, out)
		}
	})
	return slug
}

func (e *env) call(method, path string, body any, out any) int {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.server.URL+path, reader)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %q: %v", method, path, data, err)
		}
	}
	if resp.StatusCode >= 400 {
		e.t.Logf("%s %s -> %d %s", method, path, resp.StatusCode, data)
	}
	return resp.StatusCode
}

type appInfo struct {
	Slug     string `json:"slug"`
	HostPort int    `json:"host_port"`
}

type deployment struct {
	ID     int64  `json:"id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

func (e *env) createApp(slug, repoURL string) appInfo {
	e.t.Helper()
	var app appInfo
	status := e.call(http.MethodPost, "/api/apps", map[string]any{
		"slug": slug, "repo_url": repoURL, "web_port": 8080,
	}, &app)
	if status != http.StatusCreated {
		e.t.Fatalf("create app: status %d", status)
	}
	return app
}

func (e *env) setEnv(slug string, vars map[string]string) {
	e.t.Helper()
	var list []map[string]string
	for k, v := range vars {
		list = append(list, map[string]string{"key": k, "value": v})
	}
	if status := e.call(http.MethodPut, "/api/apps/"+slug+"/env", map[string]any{"vars": list}, nil); status != http.StatusOK {
		e.t.Fatalf("set env: status %d", status)
	}
}

// deploy starts a deployment and waits for its final status.
func (e *env) deploy(slug string, body map[string]string) deployment {
	e.t.Helper()
	var d deployment
	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/deployments", body, &d); status != http.StatusAccepted {
		e.t.Fatalf("create deployment: status %d", status)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		e.call(http.MethodGet, fmt.Sprintf("/api/deployments/%d", d.ID), nil, &d)
		if d.Status == "succeeded" || d.Status == "failed" || d.Status == "canceled" {
			return d
		}
		time.Sleep(300 * time.Millisecond)
	}
	e.t.Fatalf("deployment %d did not finish:\n%s", d.ID, e.logText(d.ID))
	return d
}

func (e *env) logText(id int64) string {
	e.t.Helper()
	resp, err := http.Get(fmt.Sprintf("%s/api/deployments/%d/logs?format=text", e.server.URL, id))
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	return string(data)
}

// page fetches the app's status page straight from its loopback port.
func page(t *testing.T, port int) map[string]string {
	t.Helper()
	var lastErr error
	for range 20 {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
		if err != nil {
			lastErr = err
			time.Sleep(250 * time.Millisecond)
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		out := map[string]string{"": strings.TrimSpace(string(data))}
		for _, line := range strings.Split(string(data), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				out[k] = v
			}
		}
		return out
	}
	t.Fatalf("app on port %d is not answering: %v", port, lastErr)
	return nil
}

// repo is a local git repository that plays the role of GitHub.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T, sample string) *repo {
	t.Helper()
	r := &repo{t: t, dir: t.TempDir()}
	r.git("init", "--quiet", "--initial-branch", "main")
	// Let clients fetch any commit by SHA, as GitHub does.
	r.git("config", "uploadpack.allowAnySHA1InWant", "true")
	if sample != "" {
		if err := os.CopyFS(r.dir, os.DirFS(filepath.Join(samples, sample))); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *repo) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) commit(message string) string {
	r.t.Helper()
	r.git("add", "--all")
	r.git("commit", "--quiet", "-m", message)
	return r.git("rev-parse", "HEAD")
}

func (r *repo) url() string { return "file://" + r.dir }

// publishedPorts lists "service ports" for the containers of a project.
func publishedPorts(t *testing.T, project string) map[string]string {
	t.Helper()
	out, err := exec.Command("docker", "ps",
		"--filter", "label=com.docker.compose.project="+project,
		"--format", `{{.Label "com.docker.compose.service"}}|{{.Ports}}`).Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	ports := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if service, p, ok := strings.Cut(line, "|"); ok {
			ports[service] = p
		}
	}
	return ports
}

// TestBuildAppLifecycle covers the M1 completion criteria for an app with a
// build section: deploy through the API, redeploy with changed environment
// while the volume survives, and keep the old version when a commit fails
// to start.
func TestBuildAppLifecycle(t *testing.T) {
	e := newEnv(t)
	source := newRepo(t, "build-app")
	v1 := source.commit("Version 1")
	slug := e.slug("build")
	app := e.createApp(slug, source.url())

	const tricky = `pa$$w0rd ${HOME} 'single' "double" #hash \back`
	e.setEnv(slug, map[string]string{"GREETING": "hello-one", "TRICKY": tricky})

	first := e.deploy(slug, map[string]string{"sha": v1})
	if first.Status != "succeeded" {
		t.Fatalf("first deployment %s: %s\n%s", first.Status, first.Error, e.logText(first.ID))
	}
	got := page(t, app.HostPort)
	if got["version"] != "v1" || got["greeting"] != "hello-one" || got["boots"] != "1" {
		t.Errorf("page after first deploy = %v", got)
	}
	// Values reach the container byte for byte, whatever characters they hold.
	if got["tricky"] != tricky {
		t.Errorf("TRICKY = %q, want %q", got["tricky"], tricky)
	}
	// With --env-file, Compose does not read the repository's .env for
	// interpolation (recorded in ARCHITECTURE chapter 12).
	if got["dotenv"] != "unset" {
		t.Errorf("FROM_REPO_DOTENV = %q, want \"unset\": the repository's .env was used for interpolation", got["dotenv"])
	}
	log := e.logText(first.ID)
	if strings.Contains(log, "pa$$w0rd") || strings.Contains(log, "hello-one") {
		t.Errorf("the deploy log leaks environment values:\n%s", log)
	}

	// Only the web service is published, and only on loopback.
	ports := publishedPorts(t, "paas-"+slug)
	if want := fmt.Sprintf("127.0.0.1:%d->8080/tcp", app.HostPort); ports["web"] != want {
		t.Errorf("web ports = %q, want %q", ports["web"], want)
	}
	if p, ok := ports["sidecar"]; !ok || p != "" {
		t.Errorf("sidecar ports = %q (present: %v), want none published", p, ok)
	}

	// Redeploy by branch name with a changed variable: the new value is
	// live and the named volume kept its data.
	source.write("VERSION", "v2\n")
	source.commit("Version 2")
	e.setEnv(slug, map[string]string{"GREETING": "hello-two", "TRICKY": tricky})
	second := e.deploy(slug, map[string]string{"ref": "main"})
	if second.Status != "succeeded" {
		t.Fatalf("second deployment %s: %s\n%s", second.Status, second.Error, e.logText(second.ID))
	}
	got = page(t, app.HostPort)
	if got["version"] != "v2" || got["greeting"] != "hello-two" || got["boots"] != "2" {
		t.Errorf("page after second deploy = %v (boots=2 proves the volume survived)", got)
	}

	// A commit that cannot start fails, and the previous version keeps
	// answering with the environment it was deployed with.
	source.write("entrypoint.sh", "#!/bin/sh\necho 'boom: refusing to start' >&2\nexit 1\n")
	source.write("VERSION", "broken\n")
	broken := source.commit("Broken version")
	e.setEnv(slug, map[string]string{"GREETING": "hello-three", "TRICKY": tricky})
	third := e.deploy(slug, map[string]string{"sha": broken})
	if third.Status != "failed" {
		t.Fatalf("third deployment = %+v, want failed\n%s", third, e.logText(third.ID))
	}
	failedLog := e.logText(third.ID)
	t.Logf("log of the failed deployment:\n%s", failedLog)
	got = page(t, app.HostPort)
	if got["version"] != "v2" || got["greeting"] != "hello-two" {
		t.Errorf("page after the failed deploy = %v, want the previous release", got)
	}
	if !strings.Contains(failedLog, fmt.Sprintf("Rolling back to deployment #%d", second.ID)) ||
		!strings.Contains(failedLog, "is running again") {
		t.Errorf("the log does not show the rollback:\n%s", failedLog)
	}

	// The live log stream replays the log and ends with the final status.
	resp, err := http.Get(fmt.Sprintf("%s/api/deployments/%d/logs", e.server.URL, third.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sawEnd bool
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if scanner.Text() == "event: end" && scanner.Scan() && scanner.Text() == "data: failed" {
			sawEnd = true
		}
	}
	if !sawEnd {
		t.Error("the SSE stream did not end with the failed status")
	}

	var current struct {
		CurrentDeployment *deployment `json:"current_deployment"`
	}
	e.call(http.MethodGet, "/api/apps/"+slug, nil, &current)
	if current.CurrentDeployment == nil || current.CurrentDeployment.ID != second.ID {
		t.Errorf("current deployment = %+v, want #%d", current.CurrentDeployment, second.ID)
	}
}

// TestImageOnlyApp covers the second sample: no build, images are pulled.
func TestImageOnlyApp(t *testing.T) {
	e := newEnv(t)
	source := newRepo(t, "image-app")
	sha := source.commit("Image-only app")
	slug := e.slug("image")
	app := e.createApp(slug, source.url())
	e.setEnv(slug, map[string]string{"GREETING": "from-env-file"})

	d := e.deploy(slug, map[string]string{"sha": sha})
	if d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	if got := page(t, app.HostPort)[""]; got != "image-app greeting=from-env-file" {
		t.Errorf("page = %q", got)
	}
	log := e.logText(d.ID)
	if strings.Contains(log, " build Building images") || !strings.Contains(log, " pull Pulling images") {
		t.Errorf("an image-only app must pull and not build:\n%s", log)
	}
}

// containerStates maps service name to container state for a project.
func containerStates(t *testing.T, project string) map[string]string {
	t.Helper()
	out, err := exec.Command("docker", "ps", "--all",
		"--filter", "label=com.docker.compose.project="+project,
		"--format", `{{.Label "com.docker.compose.service"}}|{{.State}}`).Output()
	if err != nil {
		t.Fatalf("docker ps: %v", err)
	}
	states := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if service, state, ok := strings.Cut(line, "|"); ok {
			states[service] = state
		}
	}
	return states
}

func projectVolumes(t *testing.T, project string) []string {
	t.Helper()
	out, err := exec.Command("docker", "volume", "ls", "--quiet",
		"--filter", "label=com.docker.compose.project="+project).Output()
	if err != nil {
		t.Fatalf("docker volume ls: %v", err)
	}
	return strings.Fields(string(out))
}

// TestStopStartDelete covers the manual operations of M5 on a real app:
// stop, start (data survives), the services listing, and deletion with and
// without volumes.
func TestStopStartDelete(t *testing.T) {
	e := newEnv(t)
	source := newRepo(t, "build-app")
	sha := source.commit("Version 1")
	slug := e.slug("ops")
	project := "paas-" + slug
	app := e.createApp(slug, source.url())

	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/start", nil, nil); status != http.StatusConflict {
		t.Errorf("starting an app that was never deployed: status %d, want 409", status)
	}
	if d := e.deploy(slug, map[string]string{"sha": sha}); d.Status != "succeeded" {
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}

	var listing struct {
		Services []struct {
			Name, State      string
			MemoryBytes      *int64 `json:"memory_bytes"`
			MemoryLimitBytes *int64 `json:"memory_limit_bytes"`
		} `json:"services"`
	}
	e.call(http.MethodGet, "/api/apps/"+slug+"/services", nil, &listing)
	if len(listing.Services) != 2 || listing.Services[0].Name != "sidecar" || listing.Services[1].Name != "web" ||
		listing.Services[1].State != "running" {
		t.Fatalf("services = %+v", listing.Services)
	}
	// Memory figures come from docker stats; the limit is the platform
	// default (512m) that the override applied.
	web := listing.Services[1]
	if web.MemoryBytes == nil || *web.MemoryBytes <= 0 || web.MemoryLimitBytes == nil || *web.MemoryLimitBytes != 512<<20 {
		t.Errorf("web memory = %v / %v, want usage > 0 and a 512MiB limit", web.MemoryBytes, web.MemoryLimitBytes)
	}

	var usage struct {
		Items []struct {
			Type       string `json:"type"`
			TotalBytes int64  `json:"total_bytes"`
		} `json:"items"`
	}
	e.call(http.MethodGet, "/api/system/docker", nil, &usage)
	if len(usage.Items) != 4 || usage.Items[0].Type != "images" || usage.Items[0].TotalBytes <= 0 {
		t.Errorf("docker usage = %+v", usage.Items)
	}

	var state struct {
		DesiredState string `json:"desired_state"`
	}
	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/stop", nil, &state); status != http.StatusOK || state.DesiredState != "stopped" {
		t.Fatalf("stop: status %d, state %q", status, state.DesiredState)
	}
	for service, s := range containerStates(t, project) {
		if s != "exited" {
			t.Errorf("after stop, %s is %s", service, s)
		}
	}
	if _, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", app.HostPort)); err == nil {
		t.Error("the app still answers after stop")
	}

	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/start", nil, &state); status != http.StatusOK || state.DesiredState != "running" {
		t.Fatalf("start: status %d, state %q", status, state.DesiredState)
	}
	if got := page(t, app.HostPort); got["version"] != "v1" || got["boots"] != "2" {
		t.Errorf("page after start = %v (boots=2: same volume, second boot)", got)
	}
	if status := e.call(http.MethodPost, "/api/apps/"+slug+"/restart", nil, nil); status != http.StatusOK {
		t.Errorf("restart: status %d", status)
	}
	if got := page(t, app.HostPort); got["boots"] != "3" {
		t.Errorf("page after restart = %v", got)
	}

	// Delete without volumes: containers and images go, data stays.
	if status := e.call(http.MethodDelete, "/api/apps/"+slug, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete: status %d", status)
	}
	if states := containerStates(t, project); len(states) != 0 {
		t.Errorf("containers left after delete: %v", states)
	}
	if volumes := projectVolumes(t, project); len(volumes) != 1 {
		t.Errorf("volumes after delete without volumes=true: %v, want the data volume kept", volumes)
	}
	if out, _ := exec.Command("docker", "images", "--quiet", "--filter", "reference="+project+"-*").Output(); strings.TrimSpace(string(out)) != "" {
		t.Errorf("release images left after delete: %s", out)
	}
	if status := e.call(http.MethodGet, "/api/apps/"+slug, nil, nil); status != http.StatusNotFound {
		t.Errorf("deleted app: status %d", status)
	}

	// The same slug can be created again and picks the kept volume up;
	// deleting it with volumes=true removes the data too.
	app = e.createApp(slug, source.url())
	if d := e.deploy(slug, map[string]string{"sha": sha}); d.Status != "succeeded" {
		t.Fatalf("redeploy after delete %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	if got := page(t, app.HostPort); got["boots"] != "4" {
		t.Errorf("page after recreating the app = %v (boots=4: the kept volume was reused)", got)
	}
	if status := e.call(http.MethodDelete, "/api/apps/"+slug+"?volumes=true", nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete with volumes: status %d", status)
	}
	if volumes := projectVolumes(t, project); len(volumes) != 0 {
		t.Errorf("volumes left after delete with volumes=true: %v", volumes)
	}
}

// TestPolicyBlocksPrivilegedContainer checks that a rejected project never
// reaches Docker.
func TestPolicyBlocksPrivilegedContainer(t *testing.T) {
	e := newEnv(t)
	source := newRepo(t, "")
	source.write("compose.yaml", "services:\n  web:\n    image: busybox:1.37\n    privileged: true\n"+
		"    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n    command: [\"sleep\", \"infinity\"]\n")
	sha := source.commit("Privileged container")
	slug := e.slug("policy")
	e.createApp(slug, source.url())

	d := e.deploy(slug, map[string]string{"sha": sha})
	if d.Status != "failed" || !strings.Contains(d.Error, "privileged") || !strings.Contains(d.Error, "docker_socket") {
		t.Fatalf("deployment = %+v\n%s", d, e.logText(d.ID))
	}
	if ports := publishedPorts(t, "paas-"+slug); len(ports) != 0 {
		t.Errorf("containers were started despite the policy: %v", ports)
	}
}

// TestTwoAppsAreIsolated covers two acceptance scenarios with real
// containers: while one app is being deployed the other keeps answering, and
// two apps side by side cannot reach each other's services.
func TestTwoAppsAreIsolated(t *testing.T) {
	e := newEnv(t)
	repoA, repoB := newRepo(t, "build-app"), newRepo(t, "build-app")
	shaA := repoA.commit("App A")
	// A different VERSION forces a real build for the second app.
	repoB.write("VERSION", "b1\n")
	shaB := repoB.commit("App B")
	slugA, slugB := e.slug("iso-a"), e.slug("iso-b")
	appA := e.createApp(slugA, repoA.url())
	appB := e.createApp(slugB, repoB.url())

	if d := e.deploy(slugA, map[string]string{"sha": shaA}); d.Status != "succeeded" {
		t.Fatalf("deploy A %s: %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}

	// Keep asking app A while app B is built and started.
	stop := make(chan struct{})
	type tally struct{ ok, failed int }
	result := make(chan tally, 1)
	go func() {
		var counts tally
		client := &http.Client{Timeout: 2 * time.Second}
		for {
			select {
			case <-stop:
				result <- counts
				return
			default:
			}
			resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/", appA.HostPort))
			if err != nil || resp.StatusCode != http.StatusOK {
				counts.failed++
			} else {
				counts.ok++
			}
			if resp != nil {
				_ = resp.Body.Close()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	deployB := e.deploy(slugB, map[string]string{"sha": shaB})
	close(stop)
	counts := <-result
	if deployB.Status != "succeeded" {
		t.Fatalf("deploy B %s: %s\n%s", deployB.Status, deployB.Error, e.logText(deployB.ID))
	}
	if counts.failed != 0 || counts.ok < 5 {
		t.Errorf("app A during the deployment of app B: %d ok, %d failed requests", counts.ok, counts.failed)
	}
	if got := page(t, appB.HostPort); got["version"] != "b1" {
		t.Errorf("app B page = %v", got)
	}

	// Each app publishes only its web service, on loopback, on its own port.
	for slug, app := range map[string]appInfo{slugA: appA, slugB: appB} {
		ports := publishedPorts(t, "paas-"+slug)
		if want := fmt.Sprintf("127.0.0.1:%d->8080/tcp", app.HostPort); ports["web"] != want || ports["sidecar"] != "" {
			t.Errorf("%s ports = %v, want only web on %s", slug, ports, want)
		}
	}
	if appA.HostPort == appB.HostPort {
		t.Errorf("both apps got host port %d", appA.HostPort)
	}

	// Inside app A: its own sidecar resolves by name, app B's containers do
	// not, and app B's web container is unreachable by address.
	containerA := "paas-" + slugA + "-web-1"
	containerB := "paas-" + slugB + "-web-1"
	// (ping, because busybox nslookup exits 0 even for unknown names.)
	if out, err := exec.Command("docker", "exec", containerA, "ping", "-c", "1", "-W", "3", "sidecar").CombinedOutput(); err != nil {
		t.Errorf("app A cannot reach its own sidecar by name: %v\n%s", err, out)
	}
	if out, err := exec.Command("docker", "exec", containerA, "ping", "-c", "1", "-W", "3", containerB).CombinedOutput(); err == nil {
		t.Errorf("app A resolves and reaches a container of app B by name:\n%s", out)
	}
	ipB, err := exec.Command("docker", "inspect", "--format",
		"{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", containerB).Output()
	if err != nil || strings.TrimSpace(string(ipB)) == "" {
		t.Fatalf("address of app B's container: %v %q", err, ipB)
	}
	target := fmt.Sprintf("http://%s:8080/", strings.TrimSpace(string(ipB)))
	if out, err := exec.Command("docker", "exec", containerA, "wget", "-q", "-T", "3", "-O", "-", target).CombinedOutput(); err == nil {
		t.Errorf("app A reached app B's web service at %s:\n%s", target, out)
	}
	// Sanity check of the probe itself: app A reaches its own web service.
	if out, err := exec.Command("docker", "exec", containerA, "wget", "-q", "-T", "3", "-O", "-", "http://127.0.0.1:8080/").CombinedOutput(); err != nil {
		t.Errorf("the probe does not work inside app A: %v\n%s", err, out)
	}
}
