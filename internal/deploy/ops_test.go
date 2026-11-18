package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// deployedApp returns a harness with one successfully deployed app.
func deployedApp(t *testing.T) (*harness, store.App) {
	t.Helper()
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	if d := h.deploy("blog", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	app, err := h.store.GetApp(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	h.runner.Reset()
	h.router.mu.Lock()
	h.router.calls = 0
	h.router.mu.Unlock()
	return h, app
}

func (h *harness) reload(slug string) store.App {
	h.t.Helper()
	app, err := h.store.GetApp(context.Background(), slug)
	if err != nil {
		h.t.Fatal(err)
	}
	return app
}

func TestStopAndStart(t *testing.T) {
	h, app := deployedApp(t)
	ctx := context.Background()

	if err := h.engine.Stop(ctx, app); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	stop := h.lastCall("stop")
	if !slices.Equal(stop.Args, []string{"compose", "-p", "paas-blog", "stop"}) {
		t.Errorf("stop args = %v", stop.Args)
	}
	app = h.reload("blog")
	if app.DesiredState != store.DesiredStopped {
		t.Errorf("DesiredState = %q", app.DesiredState)
	}
	if h.router.calls != 1 {
		t.Errorf("routes must be synced so the 503 page appears: %d calls", h.router.calls)
	}

	if err := h.engine.Start(ctx, app); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Start uses the files of the current release, like a deploy would.
	up := h.lastCall("up")
	release := h.engine.ReleaseDir("blog", *app.CurrentDeploymentID)
	want := []string{"compose", "-p", "paas-blog",
		"-f", filepath.Join(release, "compose.yaml"), "-f", filepath.Join(release, compose.OverrideFileName),
		"--env-file", h.engine.EnvFile("blog"), "up", "-d", "--remove-orphans", "--wait", "--wait-timeout", "300"}
	if !slices.Equal(up.Args, want) {
		t.Errorf("up args = %v", up.Args)
	}
	if got := h.reload("blog"); got.DesiredState != store.DesiredRunning {
		t.Errorf("DesiredState = %q", got.DesiredState)
	}
	if h.router.calls != 2 {
		t.Errorf("router calls = %d", h.router.calls)
	}
}

func TestStartFallsBackWhenReleaseFilesAreGone(t *testing.T) {
	h, app := deployedApp(t)
	if err := os.RemoveAll(h.engine.ReleaseDir("blog", *app.CurrentDeploymentID)); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Start(context.Background(), app); err != nil {
		t.Fatalf("Start: %v", err)
	}
	start := h.lastCall("start")
	if !slices.Equal(start.Args, []string{"compose", "-p", "paas-blog", "start"}) {
		t.Errorf("start args = %v", start.Args)
	}
}

func TestOperationsOnUndeployedApp(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	ctx := context.Background()
	if err := h.engine.Start(ctx, app); !errors.Is(err, ErrNotDeployed) {
		t.Errorf("Start err = %v", err)
	}
	if err := h.engine.Restart(ctx, app); !errors.Is(err, ErrNotDeployed) {
		t.Errorf("Restart err = %v", err)
	}
	if len(h.runner.Calls()) != 0 {
		t.Errorf("nothing may run: %v", h.runner.Calls())
	}
}

func TestRestart(t *testing.T) {
	h, app := deployedApp(t)
	ctx := context.Background()
	if err := h.engine.Restart(ctx, app); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if got := h.dockerCalls(); !slices.Equal(got, []string{"restart"}) {
		t.Errorf("docker calls = %v", got)
	}
	// Restarting a stopped app starts it.
	_ = h.engine.Stop(ctx, app)
	h.runner.Reset()
	if err := h.engine.Restart(ctx, h.reload("blog")); err != nil {
		t.Fatalf("Restart of a stopped app: %v", err)
	}
	if got := h.dockerCalls(); !slices.Equal(got, []string{"up"}) {
		t.Errorf("docker calls = %v", got)
	}
	if got := h.reload("blog"); got.DesiredState != store.DesiredRunning {
		t.Errorf("DesiredState = %q", got.DesiredState)
	}
}

func TestOperationFailureKeepsState(t *testing.T) {
	h, app := deployedApp(t)
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub == "stop" {
			_, _ = cmd.Stderr.Write([]byte("Cannot connect to the Docker daemon\n"))
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.mu.Unlock()
	err := h.engine.Stop(context.Background(), app)
	if err == nil || !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Fatalf("err = %v", err)
	}
	if got := h.reload("blog"); got.DesiredState != store.DesiredRunning {
		t.Errorf("a failed stop must not mark the app stopped: %q", got.DesiredState)
	}
}

func TestOperationsAreRejectedWhileDeploying(t *testing.T) {
	h, app := deployedApp(t)
	ctx := context.Background()

	// A queued deployment already blocks manual operations.
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "build" {
			once.Do(func() { close(started) })
			<-release
		}
		return nil
	}
	h.mu.Unlock()
	d := h.enqueue("blog", shaB)
	<-started

	for name, op := range map[string]func() error{
		"stop":    func() error { return h.engine.Stop(ctx, app) },
		"start":   func() error { return h.engine.Start(ctx, app) },
		"restart": func() error { return h.engine.Restart(ctx, app) },
		"delete":  func() error { return h.engine.Delete(ctx, app, true) },
	} {
		if err := op(); !errors.Is(err, ErrBusy) {
			t.Errorf("%s during a deployment: err = %v", name, err)
		}
	}
	close(release)
	if got := h.wait(d.ID); got.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", got)
	}
	if err := h.engine.Stop(ctx, h.reload("blog")); err != nil {
		t.Errorf("Stop after the deployment: %v", err)
	}
}

func TestDeploymentWaitsForRunningOperation(t *testing.T) {
	h, app := deployedApp(t)
	ctx := context.Background()

	stopping := make(chan struct{})
	finish := make(chan struct{})
	var order []string
	var mu sync.Mutex
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		mu.Lock()
		order = append(order, sub)
		mu.Unlock()
		if sub == "stop" {
			close(stopping)
			<-finish
		}
		return nil
	}
	h.mu.Unlock()

	done := make(chan error, 1)
	go func() { done <- h.engine.Stop(ctx, app) }()
	<-stopping
	d := h.enqueue("blog", shaB)
	time.Sleep(50 * time.Millisecond) // give the worker time to (wrongly) start
	mu.Lock()
	early := slices.Clone(order)
	mu.Unlock()
	if !slices.Equal(early, []string{"stop"}) {
		t.Errorf("the deployment ran docker while stop was in progress: %v", early)
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := h.wait(d.ID); got.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", got)
	}
}

func TestDelete(t *testing.T) {
	for _, removeVolumes := range []bool{false, true} {
		name := "keep volumes"
		if removeVolumes {
			name = "remove volumes"
		}
		t.Run(name, func(t *testing.T) {
			h, app := deployedApp(t)
			ctx := context.Background()
			handle := h.runner.Handler
			h.runner.Handler = func(ctx context.Context, cmd runner.Cmd) (string, error) {
				if cmd.Name == "docker" && len(cmd.Args) > 1 && cmd.Args[0] == "volume" && cmd.Args[1] == "ls" {
					return "paas-blog_pgdata\npaas-blog_storage\n", nil
				}
				return handle(ctx, cmd)
			}
			release := h.engine.ReleaseDir("blog", *app.CurrentDeploymentID)

			if err := h.engine.Delete(ctx, app, removeVolumes); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			down := h.lastCall("down")
			wantDown := []string{"compose", "-p", "paas-blog",
				"-f", filepath.Join(release, "compose.yaml"), "-f", filepath.Join(release, compose.OverrideFileName),
				"--env-file", h.engine.EnvFile("blog"), "down", "--remove-orphans"}
			if removeVolumes {
				wantDown = append(wantDown, "--volumes")
			}
			if !slices.Equal(down.Args, wantDown) {
				t.Errorf("down args = %v", down.Args)
			}
			calls := h.dockerCalls()
			removedVolumes := slices.Contains(calls, "volume rm paas-blog_pgdata paas-blog_storage")
			if removedVolumes != removeVolumes {
				t.Errorf("volume removal = %v, want %v (calls: %v)", removedVolumes, removeVolumes, calls)
			}
			if !slices.Contains(calls, "image rm paas-blog-web:r1") {
				t.Errorf("release images were not removed: %v", calls)
			}
			if _, err := h.store.GetApp(ctx, "blog"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("the app row still exists: %v", err)
			}
			if _, err := os.Stat(filepath.Join(h.dir, "apps", "blog")); !os.IsNotExist(err) {
				t.Errorf("the app directory still exists: %v", err)
			}
			if h.router.calls != 1 {
				t.Errorf("router calls = %d", h.router.calls)
			}
		})
	}
}

func TestDeleteNeverDeployedApp(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	if err := h.engine.Delete(context.Background(), app, true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	down := h.lastCall("down")
	if !slices.Equal(down.Args, []string{"compose", "-p", "paas-blog", "down", "--remove-orphans", "--volumes"}) {
		t.Errorf("down args = %v", down.Args)
	}
	if _, err := h.store.GetApp(context.Background(), "blog"); !errors.Is(err, store.ErrNotFound) {
		t.Error("the app row still exists")
	}
}

func TestDeleteKeepsAppWhenDockerFails(t *testing.T) {
	h, app := deployedApp(t)
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "down" {
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.mu.Unlock()
	if err := h.engine.Delete(context.Background(), app, false); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := h.store.GetApp(context.Background(), "blog"); err != nil {
		t.Errorf("the app must survive a failed delete: %v", err)
	}
}

func TestParseComposePS(t *testing.T) {
	lines := `{"Service":"web","State":"running","Status":"Up 2 hours (healthy)","Health":"healthy","Name":"paas-blog-web-1"}
{"Service":"db","State":"exited","Status":"Exited (1) 3 minutes ago","Health":""}
`
	want := []ServiceStatus{
		{Name: "db", State: "exited", Status: "Exited (1) 3 minutes ago"},
		{Name: "web", State: "running", Status: "Up 2 hours (healthy)", Health: "healthy"},
	}
	got, err := parseComposePS([]byte(lines))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("JSON lines: %+v, %v", got, err)
	}
	array := `[{"Service":"web","State":"running","Status":"Up 2 hours (healthy)","Health":"healthy"},{"Service":"db","State":"exited","Status":"Exited (1) 3 minutes ago","Health":""}]`
	got, err = parseComposePS([]byte(array))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("JSON array: %+v, %v", got, err)
	}
	for _, empty := range []string{"", "\n", "[]"} {
		got, err := parseComposePS([]byte(empty))
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("empty output %q: %+v, %v", empty, got, err)
		}
	}
	if _, err := parseComposePS([]byte("no configuration file provided")); err == nil {
		t.Error("garbage must be an error")
	}
}

func TestServicesAndPrune(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	h.runner.Handler = func(_ context.Context, cmd runner.Cmd) (string, error) {
		joined := strings.Join(cmd.Args, " ")
		switch {
		case strings.HasSuffix(joined, "ps --all --format json"):
			return `{"Service":"web","State":"running","Status":"Up 5 seconds","Health":""}` + "\n", nil
		case joined == "image prune -f":
			return "Total reclaimed space: 12MB\n", nil
		case joined == "builder prune -f --filter until=168h":
			return "Total:\t0B\n", errors.New("buildx not available")
		}
		return "", errors.New("unexpected command: " + joined)
	}
	services, err := h.engine.Services(context.Background(), app)
	if err != nil || len(services) != 1 || services[0].Name != "web" || services[0].State != "running" {
		t.Errorf("Services = %+v, %v", services, err)
	}

	result, err := h.engine.Prune(context.Background(), true, true)
	if err == nil || !strings.Contains(err.Error(), "builder prune") {
		t.Errorf("err = %v", err)
	}
	for _, want := range []string{"$ docker image prune -f", "Total reclaimed space: 12MB", "$ docker builder prune -f --filter until=168h"} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, result.Output)
		}
	}
	h.runner.Reset()
	if _, err := h.engine.Prune(context.Background(), true, false); err != nil {
		t.Errorf("image-only prune: %v", err)
	}
	if calls := h.runner.Calls(); len(calls) != 1 || strings.Join(calls[0].Args, " ") != "image prune -f" {
		t.Errorf("calls = %v", calls)
	}
}
