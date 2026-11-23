package deploy

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// withContainers makes the harness answer the container listing and
// inspection of the start step with containers, as `docker container
// inspect` prints them.
func (h *harness) withContainers(inspect string) {
	base := h.handle
	h.runner.Handler = func(ctx context.Context, cmd runner.Cmd) (string, error) {
		if cmd.Name == "docker" && len(cmd.Args) > 1 {
			switch {
			case cmd.Args[0] == "ps" && cmd.Args[1] == "--all" && strings.Contains(strings.Join(cmd.Args, " "), "--quiet"):
				return "c1\nc2\nc3\nc4\n", nil
			case cmd.Args[0] == "container" && cmd.Args[1] == "inspect":
				return inspect, nil
			}
		}
		return base(ctx, cmd)
	}
}

const gracefulContainers = `[
 {"Id": "c1", "State": {"Status": "running"}, "Config": {"StopTimeout": 300, "Labels": {"com.docker.compose.service": "scheduler"}}},
 {"Id": "c2", "State": {"Status": "running"}, "Config": {"StopTimeout": 30, "Labels": {"com.docker.compose.service": "worker"}}},
 {"Id": "c3", "State": {"Status": "running"}, "Config": {"Labels": {"com.docker.compose.service": "web"}}},
 {"Id": "c4", "State": {"Status": "exited"}, "Config": {"StopTimeout": 600, "Labels": {"com.docker.compose.service": "migrate"}}}
]`

func TestStartStepLogsStopGracePeriods(t *testing.T) {
	h := newHarness(t, nil)
	h.repoServices = `{
		"web": {"image": "example/app", "ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}]},
		"scheduler": {"image": "example/app", "stop_grace_period": "2m0s"},
		"worker": {"image": "example/app", "stop_grace_period": "5s"}
	}`
	h.withContainers(gracefulContainers)
	h.createApp("blog")
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, h.log("blog", d.ID))
	}
	log := h.log("blog", d.ID)
	start := log[strings.Index(log, " start Starting containers"):]
	// The running containers wait with the grace period they were created
	// with; a stopped one is not stopped again.
	for _, want := range []string{
		"Stopping the running containers may wait up to 5m30s: scheduler 5m0s, worker 30s\n",
		"This release sets a longer stop grace period: scheduler 2m0s\n",
	} {
		if !strings.Contains(start, want) {
			t.Errorf("the start step lacks %q:\n%s", want, log)
		}
	}
}

func TestStartStepIsQuietWithDefaultGracePeriods(t *testing.T) {
	h := newHarness(t, nil)
	h.withContainers(`[{"Id": "c1", "State": {"Status": "running"}, "Config": {"StopTimeout": 10, "Labels": {"com.docker.compose.service": "web"}}}]`)
	h.createApp("blog")
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	if log := h.log("blog", d.ID); strings.Contains(log, "grace period") || strings.Contains(log, "may wait") {
		t.Errorf("nothing about grace periods expected:\n%s", log)
	}
}

func TestStartStepContinuesWhenContainersCannotBeRead(t *testing.T) {
	h := newHarness(t, nil)
	base := h.handle
	h.runner.Handler = func(ctx context.Context, cmd runner.Cmd) (string, error) {
		if cmd.Name == "docker" && cmd.Args[0] == "ps" && strings.Contains(strings.Join(cmd.Args, " "), "--quiet") {
			return "", &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return base(ctx, cmd)
	}
	h.createApp("blog")
	h.start()
	if d := h.deploy("blog", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, h.log("blog", d.ID))
	}
}

// TestTimeoutsAllowTheStopGracePeriod checks that the operations that stop
// containers give Docker the longest total grace period the policy allows
// on top of their own work (ARCHITECTURE 6).
func TestTimeoutsAllowTheStopGracePeriod(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	if d := h.deploy("blog", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}

	var mu sync.Mutex
	left := map[string]time.Duration{}
	base := h.handle
	h.runner.Handler = func(ctx context.Context, cmd runner.Cmd) (string, error) {
		if sub, _ := composeSubcommand(cmd.Args); cmd.Name == "docker" {
			if deadline, ok := ctx.Deadline(); ok {
				mu.Lock()
				if _, seen := left[sub]; !seen {
					left[sub] = time.Until(deadline)
				}
				mu.Unlock()
			}
		}
		return base(ctx, cmd)
	}
	app, err := h.store.GetApp(context.Background(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := h.engine.Restart(ctx, app); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if err := h.engine.Stop(ctx, app); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.engine.Start(ctx, app); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for sub, min := range map[string]time.Duration{
		"stop":    compose.MaxStopGracePeriod + 2*time.Minute,
		"restart": compose.MaxStopGracePeriod + 2*time.Minute,
		// up: stopping what it recreates, then --wait-timeout (300 s).
		"up": compose.MaxStopGracePeriod + 300*time.Second,
	} {
		got, ok := left[sub]
		if !ok {
			t.Errorf("no %s call with a deadline", sub)
			continue
		}
		if got < min {
			t.Errorf("%s had %s left, want at least %s", sub, got, min)
		}
	}
}

func TestRollbackAllowsTheStopGracePeriod(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	if d := h.deploy("blog", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	var mu sync.Mutex
	var ups []time.Duration
	h.mu.Lock()
	h.onCompose = func(ctx context.Context, sub string, _ runner.Cmd) error {
		if sub != "up" {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		deadline, _ := ctx.Deadline()
		ups = append(ups, time.Until(deadline))
		if len(ups) == 1 {
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.mu.Unlock()
	if d := h.deploy("blog", shaB); d.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", d)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ups) != 2 {
		t.Fatalf("up calls = %d", len(ups))
	}
	// The rollback stops the failed release's containers, then waits.
	if min := compose.MaxStopGracePeriod + 300*time.Second; ups[1] < min {
		t.Errorf("the rollback's up had %s left, want at least %s", ups[1], min)
	}
}
