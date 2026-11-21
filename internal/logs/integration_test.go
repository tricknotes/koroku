//go:build integration

package logs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

// integrationCompose is a project with a service that prints a counter five
// times a second, and a second service whose name has dashes and ends in a
// digit, with two replicas.
const integrationCompose = `services:
  web:
    image: busybox:1.37
    stop_grace_period: 1s
    command: ["sh", "-c", "i=0; while true; do i=$$((i+1)); echo tick $$i; sleep 0.2; done"]
  job-worker-2:
    image: busybox:1.37
    stop_grace_period: 1s
    command: ["sh", "-c", "echo worker up; echo problem >&2; sleep 600"]
    deploy:
      replicas: 2
`

// recordingRunner runs commands for real and remembers them.
type recordingRunner struct {
	mu    sync.Mutex
	calls []runner.Cmd
}

func (r *recordingRunner) Run(ctx context.Context, cmd runner.Cmd) error {
	r.mu.Lock()
	r.calls = append(r.calls, cmd)
	r.mu.Unlock()
	return runner.Exec{}.Run(ctx, cmd)
}

func (r *recordingRunner) seen() []runner.Cmd {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runner.Cmd(nil), r.calls...)
}

// ticks returns the counter values stored for the web service, in file order.
func ticks(t *testing.T, dir string) []int {
	t.Helper()
	var out []int
	for _, text := range texts(serviceContent(t, dir, "blog", "web")) {
		n, err := strconv.Atoi(strings.TrimPrefix(text, "tick "))
		if err != nil {
			t.Fatalf("unexpected line in the web log: %q", text)
		}
		out = append(out, n)
	}
	return out
}

func TestCollectFromRealCompose(t *testing.T) {
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose is not available: %v: %s", err, out)
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	// Everything this test creates in Docker carries this project name and
	// is removed again; nothing else is touched.
	project := "koroku-logs-it-" + hex.EncodeToString(suffix)
	composeFile := filepath.Join(t.TempDir(), "compose.yaml")
	if err := os.WriteFile(composeFile, []byte(integrationCompose), 0o600); err != nil {
		t.Fatal(err)
	}
	compose := func(args ...string) ([]byte, error) {
		full := append([]string{"compose", "-p", project, "-f", composeFile}, args...)
		return exec.Command("docker", full...).CombinedOutput()
	}
	t.Cleanup(func() {
		if out, err := compose("down", "-v", "--timeout", "1"); err != nil {
			t.Errorf("cleanup: docker compose down: %v: %s", err, out)
		}
	})
	if out, err := compose("up", "-d", "--quiet-pull"); err != nil {
		t.Fatalf("docker compose up: %v: %s", err, out)
	}

	// The collector runs the plugin directly, as in production, when it is
	// in one of the places the docker CLI looks.
	t.Logf("compose plugin: %q", FindComposePlugin())
	dir := t.TempDir()
	app := App{Slug: "blog", Project: project}
	newCollector := func(run runner.Runner) *Collector {
		c, err := New(Options{
			Dir:               dir,
			Runner:            run,
			Location:          jst,
			Apps:              oneApp(app),
			ComposePlugin:     FindComposePlugin,
			ReconcileInterval: 100 * time.Millisecond,
			RestartDelay:      100 * time.Millisecond,
		})
		if err != nil {
			t.Fatal(err)
		}
		// The stream counts as live once its output pauses. The pause is
		// two seconds in production; the ticks are 200ms apart.
		c.settleIdle = 100 * time.Millisecond
		return c
	}
	marker := filepath.Join(dir, "blog", resumeMarkerName)
	live := func() bool {
		_, err := os.Stat(marker)
		return errors.Is(err, os.ErrNotExist)
	}
	waitLong := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// First collector: reads the backlog and follows for a moment.
	first := &recordingRunner{}
	c1 := newCollector(first)
	lines, cancelLive := c1.Subscribe("blog")
	stop := start(t, func(ctx context.Context) { _ = c1.Run(ctx) })
	waitLong("the first ticks and both replicas", func() bool {
		return len(ticks(t, dir)) >= 5 && len(serviceContent(t, dir, "blog", "job-worker-2")) >= 4 && live()
	})
	select {
	case line := <-lines:
		if line.Service != "web" && line.Service != "job-worker-2" {
			t.Errorf("live line from unexpected service %q", line.Service)
		}
	default:
		t.Error("the subscriber received nothing")
	}
	cancelLive()
	stop()

	wantStrings(t, "service directories", fileNames(t, filepath.Join(dir, "blog")), []string{"job-worker-2", "web"})
	worker := texts(serviceContent(t, dir, "blog", "job-worker-2"))
	counts := map[string]int{}
	for _, text := range worker {
		counts[text]++
	}
	// Two replicas, one line each on stdout and stderr.
	if len(worker) != 4 || counts["worker up"] != 2 || counts["problem"] != 2 {
		t.Errorf("job-worker-2 lines = %q, want two times %q and two times %q", worker, "worker up", "problem")
	}
	stoppedAt := len(ticks(t, dir))

	// The collector is down while the app keeps logging.
	time.Sleep(2 * time.Second)

	// Second collector, as after a restart of koroku: fills the gap from
	// Docker and writes no line twice.
	second := &recordingRunner{}
	c2 := newCollector(second)
	stop = start(t, func(ctx context.Context) { _ = c2.Run(ctx) })
	waitLong("the ticks after the restart", func() bool { return len(ticks(t, dir)) >= stoppedAt+12 && live() })
	stop()

	got := ticks(t, dir)
	for i, n := range got {
		if n != i+1 {
			t.Fatalf("tick %d is at position %d: a line is missing or doubled (%d ticks before the restart): %v", n, i+1, stoppedAt, got)
		}
	}
	if worker := serviceContent(t, dir, "blog", "job-worker-2"); len(worker) != 4 {
		t.Errorf("job-worker-2 has %d lines after the restart, want 4: %q", len(worker), worker)
	}

	calls := second.seen()
	if len(calls) == 0 {
		t.Fatal("the second collector ran no command")
	}
	args := strings.Join(calls[0].Args, " ")
	if !strings.Contains(args, "logs --follow --timestamps --no-color") {
		t.Errorf("follow command = docker %s", args)
	}
	// It resumed from the last entry rather than from the top, and real
	// Docker accepted the timestamp.
	if n := len(calls[0].Args); n < 2 || calls[0].Args[n-2] != "--since" {
		t.Errorf("the second collector did not resume with --since: docker %s", args)
	} else if since, err := time.Parse(time.RFC3339Nano, calls[0].Args[n-1]); err != nil || time.Since(since) > 5*time.Minute {
		t.Errorf("--since %s is not a recent timestamp (%v)", calls[0].Args[n-1], err)
	}
	if strings.Contains(strings.Join(first.seen()[0].Args, " "), "--since") {
		t.Errorf("the first collector used --since with nothing collected before")
	}
	t.Logf("first run: docker %s", strings.Join(first.seen()[0].Args, " "))
	t.Logf("second run: docker %s", args)
	t.Logf("%d ticks before the restart, %d after", stoppedAt, len(got))

	// The lines can be read back through the public API.
	tail, err := c2.Tail("blog", "web", 3)
	if err != nil || len(tail) != 3 || tail[2].Text != "tick "+strconv.Itoa(len(got)) {
		t.Errorf("Tail = %+v, %v", tail, err)
	}
	if time.Since(tail[2].Time) > 5*time.Minute || tail[2].Time.After(time.Now().Add(time.Minute)) {
		t.Errorf("timestamp %s of the last tick is not close to now", tail[2].Time)
	}
}
