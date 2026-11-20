package deploy

import (
	"context"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// fakeReporter records each reported change with the status the database
// held when it was reported.
type fakeReporter struct {
	store *store.Store

	mu      sync.Mutex
	reports []string
}

func (f *fakeReporter) Report(change StatusChange) {
	d, err := f.store.GetDeployment(context.Background(), change.DeploymentID)
	status := d.Status
	if err != nil {
		status = "missing"
	}
	entry := fmt.Sprintf("#%d %s", change.DeploymentID, status)
	if change.RolledBack {
		entry += " rolled-back"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reports = append(f.reports, entry)
}

func (f *fakeReporter) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reports)
}

func newReportingHarness(t *testing.T) (*harness, *fakeReporter) {
	t.Helper()
	reporter := &fakeReporter{}
	h := newHarness(t, func(o *Options) {
		reporter.store = o.Store
		o.Status = reporter
	})
	return h, reporter
}

func TestStatusChangesAreReported(t *testing.T) {
	h, reporter := newReportingHarness(t)
	h.createApp("blog")
	h.start()

	d := h.deploy("blog", shaA)
	want := []string{
		fmt.Sprintf("#%d queued", d.ID),
		fmt.Sprintf("#%d running", d.ID),
		fmt.Sprintf("#%d succeeded", d.ID),
	}
	if got := reporter.got(); !slices.Equal(got, want) {
		t.Errorf("reports = %v, want %v", got, want)
	}
}

func TestFailedDeploymentReportsTheRollback(t *testing.T) {
	h, reporter := newReportingHarness(t)
	h.createApp("blog")
	h.start()
	h.deploy("blog", shaA)

	var upCalls int
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub != "up" {
			return nil
		}
		upCalls++
		if upCalls == 1 {
			_, _ = io.WriteString(cmd.Stderr, "exited (1)\n")
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.mu.Unlock()
	second := h.deploy("blog", shaB)
	if second.Status != store.StatusFailed {
		t.Fatalf("second = %+v", second)
	}
	got := reporter.got()
	if last := got[len(got)-1]; last != fmt.Sprintf("#%d failed rolled-back", second.ID) {
		t.Errorf("last report = %q (all: %v)", last, got)
	}
}

func TestCanceledAndInterruptedDeploymentsAreReported(t *testing.T) {
	h, reporter := newReportingHarness(t)
	app := h.createApp("blog")
	ctx := context.Background()

	// A deployment left running by a previous process.
	stale, _, err := h.store.EnqueueDeployment(ctx, app.ID, "main", shaA, "", h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.StartDeployment(ctx, stale.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	first := h.enqueue("blog", shaA)
	second := h.enqueue("blog", shaB) // supersedes first
	if err := h.engine.Cancel(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	h.start()
	interrupted := fmt.Sprintf("#%d failed", stale.ID)
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(reporter.got(), interrupted) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	got := reporter.got()
	for _, want := range []string{
		fmt.Sprintf("#%d canceled", first.ID),
		fmt.Sprintf("#%d canceled", second.ID),
		interrupted,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("reports lack %q: %v", want, got)
		}
	}
}
