package deploy

import (
	"context"
	"encoding/json"
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
	if !slices.Equal(stop.Args, []string{"compose", "-p", "koroku-blog", "stop"}) {
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
	want := []string{"compose", "-p", "koroku-blog",
		"-f", filepath.Join(release, "compose.yaml"), "-f", filepath.Join(release, compose.OverrideFileName),
		"--env-file", h.engine.EnvFile("blog"), "--env-file", h.engine.ReleaseEnvFile("blog"), "up", "-d", "--pull", "missing", "--remove-orphans", "--wait", "--wait-timeout", "300"}
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
	if !slices.Equal(start.Args, []string{"compose", "-p", "koroku-blog", "start"}) {
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
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config", "up"}) {
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
					return "koroku-blog_pgdata\nkoroku-blog_storage\n", nil
				}
				return handle(ctx, cmd)
			}
			release := h.engine.ReleaseDir("blog", *app.CurrentDeploymentID)

			if err := h.engine.Delete(ctx, app, removeVolumes); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			down := h.lastCall("down")
			wantDown := []string{"compose", "-p", "koroku-blog",
				"-f", filepath.Join(release, "compose.yaml"), "-f", filepath.Join(release, compose.OverrideFileName),
				"--env-file", h.engine.EnvFile("blog"), "--env-file", h.engine.ReleaseEnvFile("blog"), "down", "--remove-orphans"}
			if removeVolumes {
				wantDown = append(wantDown, "--volumes")
			}
			if !slices.Equal(down.Args, wantDown) {
				t.Errorf("down args = %v", down.Args)
			}
			calls := h.dockerCalls()
			removedVolumes := slices.Contains(calls, "volume rm koroku-blog_pgdata koroku-blog_storage")
			if removedVolumes != removeVolumes {
				t.Errorf("volume removal = %v, want %v (calls: %v)", removedVolumes, removeVolumes, calls)
			}
			if !slices.Contains(calls, "image rm koroku-blog-web:r1") {
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
	if !slices.Equal(down.Args, []string{"compose", "-p", "koroku-blog", "down", "--remove-orphans", "--volumes"}) {
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
	lines := `{"Service":"web","State":"running","Status":"Up 2 hours (healthy)","Health":"healthy","Name":"koroku-blog-web-1"}
{"Service":"db","State":"exited","Status":"Exited (1) 3 minutes ago","Health":""}
`
	want := []ServiceStatus{
		{Name: "db", State: "exited", Status: "Exited (1) 3 minutes ago"},
		{Name: "web", State: "running", Status: "Up 2 hours (healthy)", Health: "healthy", container: "koroku-blog-web-1"},
	}
	got, err := parseComposePS([]byte(lines))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("JSON lines: %+v, %v", got, err)
	}
	array := `[{"Name":"koroku-blog-web-1","Service":"web","State":"running","Status":"Up 2 hours (healthy)","Health":"healthy"},{"Service":"db","State":"exited","Status":"Exited (1) 3 minutes ago","Health":""}]`
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
	// The running release reserves memory for web (ARCHITECTURE 6.2).
	ctx := context.Background()
	release, _, err := h.store.EnqueueDeployment(ctx, app.ID, "main", shaA, "", h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.StartDeployment(ctx, release.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetDeploymentMemoryReservations(ctx, release.ID, map[string]int64{"web": 256 << 20}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetDeploymentMemoryShapes(ctx, release.ID, map[string]compose.MemoryShape{"web": {}, "job": {OneShot: true}}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.FinishDeployment(ctx, release.ID, store.StatusSucceeded, "", h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	app = h.reload("blog")
	// Settings saved after the deployment: the next one reserves 128m for
	// web, and nothing for the one-shot job.
	app.Environment = store.EnvironmentProduction
	app.ServiceMemoryReservations = map[string]string{"web": "128m", "job": "64m"}
	h.runner.Handler = func(_ context.Context, cmd runner.Cmd) (string, error) {
		joined := strings.Join(cmd.Args, " ")
		switch {
		case strings.HasSuffix(joined, "ps --all --format json"):
			return `{"Name":"koroku-blog-web-1","Service":"web","State":"running","Status":"Up 5 seconds","Health":""}` + "\n" +
				`{"Name":"koroku-blog-job-1","Service":"job","State":"exited","Status":"Exited (0)","Health":""}` + "\n", nil
		case joined == "stats --no-stream --format json koroku-blog-web-1":
			return `{"Name":"koroku-blog-web-1","MemUsage":"26.02MiB / 512MiB","CPUPerc":"12.345%"}` + "\n", nil
		case joined == "system df --format json":
			return `{"Active":"8","Reclaimable":"28.32GB (69%)","Size":"40.97GB","TotalCount":"68","Type":"Images"}
{"Active":"3","Reclaimable":"8.998MB (99%)","Size":"8.998MB","TotalCount":"8","Type":"Containers"}
{"Active":"6","Reclaimable":"0B (0%)","Size":"17.52GB","TotalCount":"59","Type":"Local Volumes"}
{"Active":"0","Reclaimable":"41.52GB","Size":"65.75GB","TotalCount":"663","Type":"Build Cache"}
{"Active":"0","Reclaimable":"1GB","Size":"1GB","TotalCount":"1","Type":"Something New"}
`, nil
		case strings.HasPrefix(joined, "image ls "):
			return "", nil
		case joined == "system df -v --format json":
			return `{"Images":[{"Repository":"<none>","Tag":"<none>","Containers":"0","UniqueSize":"12MB"}],"BuildCache":[]}`, nil
		case joined == "image prune -f":
			return "Total reclaimed space: 12MB\n", nil
		case joined == "builder prune --all -f --filter until=168h":
			return "Total:\t0B\n", errors.New("buildx not available")
		}
		return "", errors.New("unexpected command: " + joined)
	}
	services, err := h.engine.Services(context.Background(), app)
	if err != nil || len(services) != 2 || services[1].Name != "web" || services[1].State != "running" {
		t.Fatalf("Services = %+v, %v", services, err)
	}
	// Memory comes from docker stats, for running containers only.
	web, job := services[1], services[0]
	if web.MemoryBytes == nil || *web.MemoryBytes != 27283947 || web.MemoryLimitBytes == nil || *web.MemoryLimitBytes != 512<<20 {
		t.Errorf("web memory = %v / %v", web.MemoryBytes, web.MemoryLimitBytes)
	}
	if web.CPUPercent == nil || *web.CPUPercent != 12.35 {
		t.Errorf("web cpu = %v, want 12.35", web.CPUPercent)
	}
	if job.MemoryBytes != nil || job.MemoryLimitBytes != nil || job.CPUPercent != nil {
		t.Errorf("a stopped container has no figures: %+v", job)
	}
	if web.MemoryReservationBytes == nil || *web.MemoryReservationBytes != 256<<20 || job.MemoryReservationBytes != nil {
		t.Errorf("reservations come from the running release: web %v, job %v", web.MemoryReservationBytes, job.MemoryReservationBytes)
	}
	if web.NextMemoryReservationBytes == nil || *web.NextMemoryReservationBytes != 128<<20 ||
		job.NextMemoryReservationBytes == nil || *job.NextMemoryReservationBytes != 0 {
		t.Errorf("next reservations follow the saved settings: web %v, job %v", web.NextMemoryReservationBytes, job.NextMemoryReservationBytes)
	}

	usage, err := h.engine.DockerDiskUsage(context.Background())
	wantUsage := []DiskUsageItem{
		// Docker's Reclaimable column is ignored: the figure is what Prune frees.
		{Type: "images", TotalBytes: 40970000000, ReclaimableBytes: 12000000, Count: 68},
		{Type: "containers", TotalBytes: 8998000, ReclaimableBytes: 0, Count: 8},
		{Type: "volumes", TotalBytes: 17520000000, ReclaimableBytes: 0, Count: 59},
		{Type: "build_cache", TotalBytes: 65750000000, ReclaimableBytes: 0, Count: 663},
	}
	if err != nil || !reflect.DeepEqual(usage, wantUsage) {
		t.Errorf("DockerDiskUsage = %+v, %v", usage, err)
	}

	result, err := h.engine.Prune(context.Background(), true, true)
	if err == nil || !strings.Contains(err.Error(), "builder prune") {
		t.Errorf("err = %v", err)
	}
	if result.ReclaimedBytes != 12000000 {
		t.Errorf("ReclaimedBytes = %d, want what Docker reported", result.ReclaimedBytes)
	}
	for _, want := range []string{"$ docker image prune -f", "Total reclaimed space: 12MB", "$ docker builder prune --all -f --filter until=168h"} {
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

func TestParseDockerSize(t *testing.T) {
	cases := map[string]int64{
		"0B": 0, "512B": 512, "1kB": 1000, "8.998MB": 8998000, "1.5GB": 1500000000,
		"26.02MiB": 27283947, "7.413GiB": 7959648141, "2TiB": 2 << 40,
		"": 0, "N/A": 0, "-1MB": 0, "12": 0, "  3MB ": 3000000,
	}
	for in, want := range cases {
		if got := parseDockerSize(in); got != want {
			t.Errorf("parseDockerSize(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestReclaimedBytes(t *testing.T) {
	cases := map[string]int64{
		"Deleted Images:\nuntagged: sha256:abc\n\nTotal reclaimed space: 1.2GB\n": 1200000000,
		"Total reclaimed space: 0B\n": 0,
		"ID\tRECLAIMABLE\tSIZE\tLAST ACCESSED\nabc\ttrue\t1.861MB\t8 days ago\nTotal:\t1.861MB\n": 1861000,
		"":                                   0,
		"Error response from daemon: boom\n": 0,
	}
	for in, want := range cases {
		if got := reclaimedBytes(in); got != want {
			t.Errorf("reclaimedBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

// The reclaimable figure is what Prune removes, not Docker's estimate.
func TestPrunableBytes(t *testing.T) {
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	const (
		old    = "2026-11-01 10:00:00.123456789 +0000 UTC"
		recent = "2026-11-17 10:00:00 +0000 UTC"
	)
	record := func(id, parent, lastUsed, inUse, shared, size string) map[string]string {
		return map[string]string{
			"ID": id, "Parent": parent, "LastUsedAt": lastUsed, "CreatedAt": old,
			"InUse": inUse, "Shared": shared, "Size": size,
		}
	}
	df := map[string]any{
		"Images": []map[string]string{
			{"Repository": "<none>", "Tag": "<none>", "Containers": "0", "UniqueSize": "12MB"},
			{"ID": "sha256:built", "Repository": "<none>", "Tag": "<none>", "Containers": "0", "UniqueSize": "50MB"}, // its layers stay as build cache
			{"Repository": "postgres", "Tag": "<none>", "Containers": "0", "UniqueSize": "3MB"},                      // pulled by digest
			{"Repository": "<none>", "Tag": "<none>", "Containers": "1", "UniqueSize": "30MB"},                       // a stopped container keeps it
			{"Repository": "koroku-blog-web", "Tag": "r3", "Containers": "0", "UniqueSize": "99MB"},
		},
		"BuildCache": []map[string]string{
			// a chain ending in a layer an image uses: all old, so it goes;
			// the shared end frees nothing
			record("a", "", old, "false", "false", "100MB"),
			record("b", "a", old, "false", "false", "50MB"),
			record("c", "b", old, "false", "true", "70MB"),
			// an old base under a recent record stays
			record("d", "", old, "false", "false", "10MB"),
			record("e", "d", recent, "false", "false", "20MB"),
			// in use
			record("f", "", old, "true", "false", "40MB"),
			// never used: its creation time counts
			record("g", "", "", "false", "false", "1MB"),
			// an unreadable time is kept
			record("h", "", "yesterday", "false", "false", "80MB"),
			// a merge of two old records, used recently
			record("x", "", old, "false", "false", "5MB"),
			record("y", "", old, "false", "false", "7MB"),
			record("m", "x, y", recent, "false", "false", "1MB"),
		},
	}
	out, err := json.Marshal(df)
	if err != nil {
		t.Fatal(err)
	}
	got, err := prunableBytes(out, now, []string{"sha256:built"})
	want := map[string]int64{"images": 15000000, "build_cache": 151000000}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("prunableBytes = %v, %v; want %v", got, err, want)
	}
	if _, err := prunableBytes([]byte("Error"), now, nil); err == nil {
		t.Error("garbage must be an error")
	}
}

// docker system df races removals of containers, images and files in
// running containers (containerd image store); errors from the daemon are
// retried, a daemon that cannot be reached is not.
func TestDockerDiskUsageRetriesDaemonErrors(t *testing.T) {
	defer func(d time.Duration) { dockerRetryDelay = d }(dockerRetryDelay)
	dockerRetryDelay = time.Millisecond
	const row = `{"Reclaimable":"0B (0%)","Size":"1GB","TotalCount":"2","Type":"Images"}` + "\n"
	raced := errors.New("Error response from daemon: failed to retrieve container list: rw layer snapshot not found for container 0123456789ab")

	for _, tc := range []struct {
		name      string
		failures  []error // returned by the first calls, in order
		wantErr   string
		wantCalls int
	}{
		{name: "one race", failures: []error{raced}, wantCalls: 2},
		{name: "two races", failures: []error{raced, errors.New("Error response from daemon: failed to calculate image disk usage: content digest sha256:abc: not found")}, wantCalls: 3},
		{name: "file removed in a running container", failures: []error{errors.New("Error response from daemon: failed to retrieve container list: snapshotter.Usage failed for 0123: lstat /var/lib/containerd/io.containerd.snapshotter.v1.overlayfs/snapshots/102/fs/tmp/x: no such file or directory")}, wantCalls: 2},
		{name: "races every time", failures: []error{raced, raced, raced, raced}, wantErr: "rw layer snapshot not found", wantCalls: 3},
		{name: "daemon down", failures: []error{errors.New("Cannot connect to the Docker daemon")}, wantErr: "Cannot connect", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			// The summary and the verbose listing run concurrently; the
			// summary fails as the case says, the listing always works.
			var mu sync.Mutex
			calls := 0
			h.runner.Handler = func(_ context.Context, cmd runner.Cmd) (string, error) {
				switch strings.Join(cmd.Args, " ") {
				case strings.Join(composeBuiltDanglingArgs, " "):
					return "", nil
				case "system df -v --format json":
					return `{"Images":[],"BuildCache":[]}`, nil
				case "system df --format json":
				default:
					return "", errors.New("unexpected command")
				}
				mu.Lock()
				defer mu.Unlock()
				calls++
				if calls <= len(tc.failures) {
					return "", tc.failures[calls-1]
				}
				return row, nil
			}
			usage, err := h.engine.DockerDiskUsage(context.Background())
			if calls != tc.wantCalls {
				t.Errorf("docker system df ran %d times, want %d", calls, tc.wantCalls)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || len(usage) != 1 || usage[0].Count != 2 {
				t.Errorf("DockerDiskUsage = %+v, %v", usage, err)
			}
		})
	}
}
