package deploy

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

func (h *harness) setTasks(app store.App, tasks ...store.Task) {
	h.t.Helper()
	if err := h.store.ReplaceTasks(context.Background(), app.ID, tasks); err != nil {
		h.t.Fatalf("ReplaceTasks: %v", err)
	}
}

func (h *harness) task(app store.App, name string) store.Task {
	h.t.Helper()
	task, err := h.store.GetTask(context.Background(), app.ID, name)
	if err != nil {
		h.t.Fatalf("GetTask: %v", err)
	}
	return task
}

func (h *harness) taskRuns(app store.App) []store.TaskRun {
	h.t.Helper()
	runs, err := h.store.ListTaskRuns(context.Background(), app.ID, 50)
	if err != nil {
		h.t.Fatalf("ListTaskRuns: %v", err)
	}
	return runs
}

// drain runs the scheduler's queue until nothing more can run.
func drain(s *TaskScheduler) {
	for s.step(context.Background()) {
	}
}

var nightly = store.Task{Name: "nightly", Service: "web", Command: "bin/rails runner 'Cleanup.perform_now'", Schedule: "*/5 * * * *"}

func TestSchedulerRunsATaskAtItsTimeOnly(t *testing.T) {
	h, app := deployedApp(t)
	h.setTasks(app, nightly, store.Task{Name: "by hand", Service: "web", Command: "true"})
	s := h.engine.NewTaskScheduler(time.UTC)
	ctx := context.Background()

	// The time when koroku starts counting is not a run, even when it
	// matches the schedule (03:00): nothing is made up for.
	s.tick(ctx)
	drain(s)
	if runs := h.taskRuns(app); len(runs) != 0 {
		t.Fatalf("runs before the first time = %+v", runs)
	}
	h.clock.Advance(4 * time.Minute)
	s.tick(ctx)
	drain(s)
	if runs := h.taskRuns(app); len(runs) != 0 {
		t.Fatalf("runs before 03:05 = %+v", runs)
	}

	h.clock.Advance(time.Minute + 10*time.Second)
	s.tick(ctx)
	drain(s)
	runs := h.taskRuns(app)
	if len(runs) != 1 {
		t.Fatalf("runs = %+v", runs)
	}
	run := runs[0]
	if run.Trigger != store.TaskTriggerScheduled || run.Actor != nil || run.TaskName != "nightly" ||
		run.Status != store.StatusSucceeded || run.Command != nightly.Command {
		t.Errorf("run = %+v", run)
	}
	log := h.taskLog("blog", run.ID)
	if !strings.Contains(log, `Scheduled run of task "nightly" for 2026-11-18T03:05:00Z.`) {
		t.Errorf("log:\n%s", log)
	}
	if calls := h.dockerCalls(); !slices.Equal(calls, []string{"config", "run"}) {
		t.Errorf("an awake app runs as by hand: %v", calls)
	}

	// Polling again within the same minute does not run it twice, and a
	// long gap (koroku suspended) runs it once, not once per missed time.
	s.tick(ctx)
	drain(s)
	h.clock.Advance(time.Hour)
	s.tick(ctx)
	drain(s)
	if runs := h.taskRuns(app); len(runs) != 2 {
		t.Fatalf("runs after a gap = %d", len(runs))
	}
}

func TestSchedulerCountsAChangedScheduleFromTheChange(t *testing.T) {
	h, app := deployedApp(t)
	h.setTasks(app, nightly)
	s := h.engine.NewTaskScheduler(time.UTC)
	ctx := context.Background()
	s.tick(ctx)
	h.clock.Advance(4 * time.Minute)
	changed := nightly
	changed.Schedule = "4 * * * *"
	h.setTasks(app, changed)
	// 03:04 is the new time, but the schedule was only seen at 03:04.
	s.tick(ctx)
	drain(s)
	if runs := h.taskRuns(app); len(runs) != 0 {
		t.Fatalf("runs = %+v", runs)
	}
	h.clock.Advance(time.Hour)
	s.tick(ctx)
	drain(s)
	if runs := h.taskRuns(app); len(runs) != 1 {
		t.Fatalf("runs after 04:04 = %+v", runs)
	}
}

func TestScheduledRunOfASleepingAppStartsOnlyTheDependencies(t *testing.T) {
	h, app := sleepyApp(t)
	h.mu.Lock()
	h.repoServices = strings.Replace(defaultRepoServices, `"web": {"build": {"context": "."},`,
		`"web": {"build": {"context": "."}, "depends_on": {"db": {"condition": "service_healthy", "required": true}},`, 1)
	h.mu.Unlock()
	app = h.putToSleep(app)
	h.setTasks(app, nightly)
	var during []string
	h.onRun(func(context.Context, runner.Cmd) error {
		during = h.dockerCalls()
		return nil
	})
	s := h.engine.NewTaskScheduler(time.UTC)
	s.tick(context.Background())
	h.clock.Advance(5 * time.Minute)
	s.tick(context.Background())
	drain(s)

	runs := h.taskRuns(app)
	if len(runs) != 1 || runs[0].Status != store.StatusSucceeded {
		t.Fatalf("runs = %+v", runs)
	}
	if !slices.Equal(during, []string{"config", "start", "run"}) {
		t.Errorf("calls before the command = %v", during)
	}
	start := h.lastCall("start")
	if !slices.Equal(start.Args, []string{"compose", "-p", "koroku-blog", "start", "--wait", "--wait-timeout", "300", "db"}) {
		t.Errorf("start args = %v", start.Args)
	}
	if calls := h.dockerCalls(); !slices.Equal(calls, []string{"config", "start", "run", "stop"}) {
		t.Errorf("docker calls = %v", calls)
	}
	if got := h.reload("blog"); got.SleepingSince == nil || !got.SleepingSince.Equal(app.SleepingSince.UTC()) {
		t.Errorf("the app must stay asleep: %v", got.SleepingSince)
	}
	h.router.mu.Lock()
	routed := h.router.calls
	h.router.mu.Unlock()
	if routed != 0 {
		t.Errorf("the route changed %d times", routed)
	}
	log := h.taskLog("blog", runs[0].ID)
	for _, want := range []string{"starting only db.", "Stopped db again.", "Exited with code 0."} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestScheduledRunOfASleepingAppWithoutDependencies(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	h.setTasks(app, nightly)
	s := h.engine.NewTaskScheduler(time.UTC)
	s.tick(context.Background())
	h.clock.Advance(5 * time.Minute)
	s.tick(context.Background())
	drain(s)
	if calls := h.dockerCalls(); !slices.Equal(calls, []string{"config", "run"}) {
		t.Errorf("nothing is started for a service without dependencies: %v", calls)
	}
	if h.reload("blog").SleepingSince == nil {
		t.Error("the app woke up")
	}
}

func TestScheduledRunFailsWhenTheDependenciesDoNotStart(t *testing.T) {
	h, app := sleepyApp(t)
	h.mu.Lock()
	h.repoServices = strings.Replace(defaultRepoServices, `"web": {"build": {"context": "."},`,
		`"web": {"build": {"context": "."}, "depends_on": {"db": {"condition": "service_started"}},`, 1)
	h.mu.Unlock()
	app = h.putToSleep(app)
	h.setTasks(app, nightly)
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		// No containers are listed, so the services are created again
		// with up, which fails too.
		if sub == "start" || sub == "up" {
			return &runner.ExitError{Code: 1}
		}
		return nil
	}
	h.mu.Unlock()
	s := h.engine.NewTaskScheduler(time.UTC)
	s.tick(context.Background())
	h.clock.Advance(5 * time.Minute)
	s.tick(context.Background())
	drain(s)
	runs := h.taskRuns(app)
	if len(runs) != 1 || runs[0].Status != store.StatusFailed || runs[0].ExitCode != nil ||
		!strings.Contains(runs[0].Error, "start db of the sleeping app") {
		t.Fatalf("runs = %+v", runs)
	}
	if calls := h.dockerCalls(); slices.Contains(calls, "run") || calls[len(calls)-1] != "stop" {
		t.Errorf("docker calls = %v", calls)
	}
}

func TestSchedulerSkipsStoppedAndUndeployedApps(t *testing.T) {
	h, app := deployedApp(t)
	h.setTasks(app, nightly)
	if err := h.engine.Stop(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	fresh := h.createApp("shop")
	h.setTasks(fresh, nightly)
	h.runner.Reset()
	s := h.engine.NewTaskScheduler(time.UTC)
	s.tick(context.Background())
	h.clock.Advance(5 * time.Minute)
	s.tick(context.Background())
	drain(s)
	if runs := h.taskRuns(app); len(runs) != 0 {
		t.Fatalf("a stopped app ran %+v", runs)
	}
	if calls := h.dockerCalls(); len(calls) != 0 {
		t.Errorf("docker calls = %v", calls)
	}
	due := time.Date(2026, 11, 18, 3, 5, 0, 0, time.UTC)
	if got := h.task(app, "nightly"); got.LastSkipReason != store.TaskSkipStopped || !got.LastSkippedAt.Equal(due) {
		t.Errorf("stopped app task = %+v", got)
	}
	if got := h.task(fresh, "nightly"); got.LastSkipReason != store.TaskSkipNotDeployed {
		t.Errorf("undeployed app task = %+v", got)
	}
}

func TestSchedulerSkipsATimeWhileThePreviousRunGoesOn(t *testing.T) {
	h, app := deployedApp(t)
	h.setTasks(app, nightly)
	release := make(chan struct{})
	h.onRun(func(ctx context.Context, _ runner.Cmd) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	s := h.engine.NewTaskScheduler(time.UTC)
	ctx := context.Background()
	s.tick(ctx)
	h.clock.Advance(5 * time.Minute)
	s.tick(ctx)
	stepped := make(chan struct{})
	go func() {
		defer close(stepped)
		drain(s)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(h.taskRuns(app)) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	h.clock.Advance(5 * time.Minute)
	s.tick(ctx)
	if got := h.task(app, "nightly"); got.LastSkipReason != store.TaskSkipOverlap ||
		!got.LastSkippedAt.Equal(time.Date(2026, 11, 18, 3, 10, 0, 0, time.UTC)) {
		t.Errorf("task = %+v", got)
	}
	close(release)
	<-stepped
	if runs := h.taskRuns(app); len(runs) != 1 {
		t.Errorf("runs = %d, want the first one only", len(runs))
	}
}

func TestSchedulerWaitsForABusyAppThenSkips(t *testing.T) {
	h, app := deployedApp(t)
	other := h.createApp("shop")
	if d := h.deploy("shop", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	other = h.reload("shop")
	h.setTasks(app, nightly)
	h.setTasks(other, nightly)
	release, err := h.engine.beginOp(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	s := h.engine.NewTaskScheduler(time.UTC)
	ctx := context.Background()
	s.tick(ctx)
	h.clock.Advance(5 * time.Minute)
	s.tick(ctx)
	// blog is busy: shop, queued behind it, runs first.
	drain(s)
	if runs := h.taskRuns(other); len(runs) != 1 {
		t.Fatalf("shop runs = %+v", runs)
	}
	if runs := h.taskRuns(app); len(runs) != 0 {
		t.Fatalf("blog ran while busy: %+v", runs)
	}
	if len(s.queued()) != 1 {
		t.Fatalf("queue = %+v", s.queued())
	}
	// Still busy ten minutes after its time: skipped.
	h.clock.Advance(TaskBusyWait)
	drain(s)
	release()
	if got := h.task(app, "nightly"); got.LastSkipReason != store.TaskSkipBusy {
		t.Errorf("task = %+v", got)
	}
	if len(s.queued()) != 0 {
		t.Errorf("queue = %+v", s.queued())
	}
}

func TestSchedulerRunsOneTaskAtATime(t *testing.T) {
	h, app := deployedApp(t)
	other := h.createApp("shop")
	if d := h.deploy("shop", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	other = h.reload("shop")
	h.setTasks(app, nightly)
	h.setTasks(other, nightly)
	release := make(chan struct{})
	running := make(chan string, 2)
	h.onRun(func(ctx context.Context, cmd runner.Cmd) error {
		running <- strings.Join(cmd.Args, " ")
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	s := h.engine.NewTaskScheduler(time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.tick(ctx)
	h.clock.Advance(5 * time.Minute)
	s.tick(ctx)
	go s.work(ctx)
	<-running
	select {
	case <-running:
		t.Fatal("two scheduled runs at once")
	case <-time.After(200 * time.Millisecond):
	}
	release <- struct{}{}
	select {
	case <-running:
	case <-time.After(5 * time.Second):
		t.Fatal("the second run did not start after the first")
	}
	close(release)
}

func TestSchedulerSkipsARunThatCannotStart(t *testing.T) {
	h, app := deployedApp(t)
	broken := nightly
	broken.Service = "missing"
	h.setTasks(app, broken)
	s := h.engine.NewTaskScheduler(time.UTC)
	s.tick(context.Background())
	h.clock.Advance(5 * time.Minute)
	s.tick(context.Background())
	drain(s)
	got := h.task(app, "nightly")
	if got.LastSkipReason != store.TaskSkipError || !strings.Contains(got.LastSkipDetail, `service "missing"`) {
		t.Errorf("task = %+v", got)
	}
	if runs := h.taskRuns(app); len(runs) != 0 {
		t.Errorf("runs = %+v", runs)
	}
}

func TestWaitTaskReturnsForUnknownRuns(t *testing.T) {
	h, _ := deployedApp(t)
	if err := h.engine.WaitTask(context.Background(), 42); err != nil {
		t.Errorf("WaitTask = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.engine.WaitTask(ctx, 42); err != nil && !errors.Is(err, context.Canceled) {
		t.Errorf("WaitTask = %v", err)
	}
}

func TestInterruptedRunOfASleepingAppStopsItAgain(t *testing.T) {
	h, app := sleepyApp(t)
	app = h.putToSleep(app)
	if _, err := h.store.StartTaskRun(context.Background(), store.TaskRun{
		AppID: app.ID, TaskName: "nightly", Service: "web", Command: "sleep 600", Trigger: store.TaskTriggerScheduled,
	}, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	// What the next start of koroku does.
	if err := h.engine.failInterruptedTaskRuns(context.Background(), h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if calls := h.dockerCalls(); !slices.Contains(calls, "stop") {
		t.Fatalf("the sleeping app was not stopped again: %v", calls)
	}
}
