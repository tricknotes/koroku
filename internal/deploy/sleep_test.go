package deploy

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// sleepyApp returns a harness with one deployed app set to sleep while idle.
func sleepyApp(t *testing.T) (*harness, store.App) {
	t.Helper()
	h, app := deployedApp(t)
	app.IdleSleep = true
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	return h, h.reload("blog")
}

// putToSleep puts the app to sleep and forgets the calls it made.
func (h *harness) putToSleep(app store.App) store.App {
	h.t.Helper()
	if err := h.engine.Sleep(context.Background(), app); err != nil {
		h.t.Fatalf("Sleep: %v", err)
	}
	h.runner.Reset()
	h.router.mu.Lock()
	h.router.calls = 0
	h.router.mu.Unlock()
	return h.reload(app.Slug)
}

func TestSleepRoutesFirstThenStops(t *testing.T) {
	h, app := sleepyApp(t)
	var callsAtSync []string
	h.router.onSync = func() {
		callsAtSync = h.dockerCalls()
		if a := h.reload("blog"); a.SleepingSince == nil {
			t.Error("the route was synced before the sleep was recorded")
		}
	}
	if err := h.engine.Sleep(context.Background(), app); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if len(callsAtSync) != 0 {
		t.Errorf("the route must change before the containers stop; calls then: %v", callsAtSync)
	}
	stop := h.lastCall("stop")
	if !slices.Equal(stop.Args, []string{"compose", "-p", "koroku-blog", "stop"}) {
		t.Errorf("stop args = %v", stop.Args)
	}
	got := h.reload("blog")
	if got.SleepingSince == nil || !got.SleepingSince.Equal(h.clock.Now()) {
		t.Errorf("SleepingSince = %v", got.SleepingSince)
	}
	if got.DesiredState != store.DesiredRunning {
		t.Errorf("sleeping must not change the desired state: %q", got.DesiredState)
	}
}

func TestSleepLeavesAppsThatMustNotSleep(t *testing.T) {
	h, app := deployedApp(t) // idle sleep off
	if err := h.engine.Sleep(context.Background(), app); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if calls := h.dockerCalls(); len(calls) != 0 || h.reload("blog").SleepingSince != nil {
		t.Errorf("an app without the setting must not sleep: %v", calls)
	}

	h, app = sleepyApp(t)
	if err := h.engine.Stop(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	h.runner.Reset()
	if err := h.engine.Sleep(context.Background(), h.reload("blog")); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	if calls := h.dockerCalls(); len(calls) != 0 || h.reload("blog").SleepingSince != nil {
		t.Errorf("a stopped app must not sleep: %v", calls)
	}
}

func TestSleepIsRefusedWhileTheAppIsBusy(t *testing.T) {
	h, app := sleepyApp(t)
	release, err := h.engine.beginOp(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine.Sleep(context.Background(), app); !errors.Is(err, ErrBusy) {
		t.Errorf("Sleep while an operation runs = %v, want ErrBusy", err)
	}
	release()
	if h.reload("blog").SleepingSince != nil {
		t.Error("a busy app went to sleep")
	}
}

func TestFailedSleepIsUndone(t *testing.T) {
	h, app := sleepyApp(t)
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "stop" {
			return errors.New("daemon unavailable")
		}
		return nil
	}
	if err := h.engine.Sleep(context.Background(), app); err == nil {
		t.Fatal("Sleep succeeded although stop failed")
	}
	if h.reload("blog").SleepingSince != nil {
		t.Error("the app is recorded asleep although its containers still run")
	}
	if h.router.calls != 2 {
		t.Errorf("the route must be switched and switched back: %d syncs", h.router.calls)
	}
}

func TestWakeStartsTheStoppedContainers(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	var awakeAtSync bool
	h.router.onSync = func() { awakeAtSync = h.reload("blog").SleepingSince == nil }

	if err := h.engine.Wake(context.Background(), app); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	start := h.lastCall("start")
	if !slices.Equal(start.Args, []string{"compose", "-p", "koroku-blog", "start", "--wait", "--wait-timeout", "300"}) {
		t.Errorf("start args = %v", start.Args)
	}
	if calls := h.dockerCalls(); !slices.Equal(calls, []string{"start"}) {
		t.Errorf("a wake reuses the containers as they are: %v", calls)
	}
	if got := h.health.urls[len(h.health.urls)-1]; got != "http://127.0.0.1:20000/up" {
		t.Errorf("health check url = %q", got)
	}
	if h.reload("blog").SleepingSince != nil || !awakeAtSync {
		t.Error("the app must be recorded awake before the route is synced")
	}
	// Waking an awake app does nothing.
	h.runner.Reset()
	if err := h.engine.Wake(context.Background(), h.reload("blog")); err != nil || len(h.dockerCalls()) != 0 {
		t.Errorf("Wake of an awake app = %v, calls %v", err, h.dockerCalls())
	}
}

func TestConcurrentRequestsShareOneWake(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	gate := make(chan struct{})
	var starts atomic.Int32
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "start" {
			starts.Add(1)
			<-gate
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- h.engine.Wake(context.Background(), app)
		}()
	}
	waitFor(t, func() bool { return starts.Load() == 1 })
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("Wake: %v", err)
		}
	}
	if n := starts.Load(); n != 1 {
		t.Errorf("start ran %d times, want once", n)
	}
}

func TestWakeOutlivesTheRequest(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	// Another operation holds the app: the wake waits for it.
	release, err := h.engine.beginOp(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.engine.Wake(ctx, app); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wake while the app is held = %v", err)
	}
	if len(h.dockerCalls()) != 0 {
		t.Error("the wake must wait for the other operation")
	}
	release()
	waitFor(t, func() bool { return h.reload("blog").SleepingSince == nil })
}

func TestFailedWakeIsRememberedForAMinute(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	h.health.err = errors.New("health check did not pass within 1m0s (last result: HTTP 502)")

	if err := h.engine.Wake(context.Background(), app); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("Wake = %v", err)
	}
	if calls := h.dockerCalls(); !slices.Equal(calls, []string{"start", "stop"}) {
		t.Errorf("a failed wake stops the app again: %v", calls)
	}
	if h.reload("blog").SleepingSince == nil {
		t.Error("a failed wake must leave the app asleep")
	}

	h.runner.Reset()
	if err := h.engine.Wake(context.Background(), app); err == nil {
		t.Fatal("the failure must be answered again")
	}
	if calls := h.dockerCalls(); len(calls) != 0 {
		t.Errorf("a failure within the minute must not start the app again: %v", calls)
	}

	h.health.err = nil
	h.clock.Advance(2 * time.Minute)
	if err := h.engine.Wake(context.Background(), app); err != nil {
		t.Fatalf("Wake after a minute: %v", err)
	}
}

func TestWakeWithoutContainersStartsTheRelease(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "start" {
			return errors.New("no containers to start")
		}
		return nil
	}
	if err := h.engine.Wake(context.Background(), app); err != nil {
		t.Fatalf("Wake: %v", err)
	}
	if calls := h.dockerCalls(); !slices.Equal(calls, []string{"start", "ps", "config", "up"}) {
		t.Errorf("docker calls = %v", calls)
	}
}

func TestDeploymentWakesASleepingApp(t *testing.T) {
	h, app := sleepyApp(t)
	h.putToSleep(app)
	d := h.deploy("blog", shaB)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	if h.reload("blog").SleepingSince != nil {
		t.Error("the deployment left the app asleep")
	}
	if log := h.log("blog", d.ID); !strings.Contains(log, "The app was asleep; this deployment wakes it.") {
		t.Errorf("log:\n%s", log)
	}
}

func TestFailedBuildKeepsTheAppAsleep(t *testing.T) {
	h, app := sleepyApp(t)
	h.putToSleep(app)
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "build" {
			return errors.New("build failed")
		}
		return nil
	}
	if d := h.deploy("blog", shaB); d.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", d)
	}
	if h.reload("blog").SleepingSince == nil {
		t.Error("a deployment that never touched the containers woke the app")
	}
}

func TestRestartWakesASleepingApp(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	if err := h.engine.Restart(context.Background(), app); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if calls := h.dockerCalls(); !slices.Equal(calls, []string{"start"}) {
		t.Errorf("docker calls = %v", calls)
	}
	if h.reload("blog").SleepingSince != nil {
		t.Error("the app is still asleep")
	}
}

func TestTaskWakesASleepingAppFirst(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	run, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "bin/rails db:migrate:status"})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if done := h.waitTask(run.ID); done.Status != store.StatusSucceeded {
		t.Fatalf("run = %+v", done)
	}
	if calls := h.dockerCalls(); !slices.Equal(calls, []string{"config", "start", "run"}) {
		t.Errorf("docker calls = %v", calls)
	}
	if log := h.taskLog("blog", run.ID); !strings.Contains(log, "waking it up.") || !strings.Contains(log, "The app is awake.") {
		t.Errorf("log:\n%s", log)
	}
}

func TestWithServiceRunningStartsOnlyTheServiceOfASleepingApp(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	ran := false
	err := h.engine.WithServiceRunning(context.Background(), app.ID, "db", func(context.Context) error {
		ran = true
		if calls := h.dockerCalls(); !slices.Equal(calls, []string{"start"}) {
			t.Errorf("calls before the dump = %v", calls)
		}
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("WithServiceRunning = %v, ran %v", err, ran)
	}
	start := h.lastCall("start")
	if !slices.Equal(start.Args, []string{"compose", "-p", "koroku-blog", "start", "--wait", "--wait-timeout", "300", "db"}) {
		t.Errorf("start args = %v", start.Args)
	}
	if calls := h.dockerCalls(); !slices.Equal(calls, []string{"start", "stop"}) {
		t.Errorf("docker calls = %v", calls)
	}
	if h.reload("blog").SleepingSince == nil || h.router.calls != 0 {
		t.Error("the app must stay asleep with its route unchanged")
	}

	// An awake app runs the function as it is.
	h, app = sleepyApp(t)
	ran = false
	if err := h.engine.WithServiceRunning(context.Background(), app.ID, "db", func(context.Context) error {
		ran = true
		return nil
	}); err != nil || !ran || len(h.dockerCalls()) != 0 {
		t.Errorf("awake: err %v, ran %v, calls %v", err, ran, h.dockerCalls())
	}
}

func TestBackupBeforeDeployOfASleepingAppDoesNotDeadlock(t *testing.T) {
	var h *harness
	backup := &fakeBackup{}
	h = newHarness(t, func(o *Options) { o.Backup = backup })
	backup.hook = func(ctx context.Context, app store.App) error {
		return h.engine.WithServiceRunning(ctx, app.ID, "db", func(context.Context) error { return nil })
	}
	app := h.createApp("blog")
	h.start()
	if d := h.deploy("blog", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	app = h.reload("blog")
	app.IdleSleep, app.BackupBeforeDeploy = true, true
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.putToSleep(h.reload("blog"))
	if d := h.deploy("blog", shaB); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, h.log("blog", d.ID))
	}
	if calls := h.dockerCalls(); !slices.Contains(calls, "start") {
		t.Errorf("the dump service was not started for the backup: %v", calls)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
