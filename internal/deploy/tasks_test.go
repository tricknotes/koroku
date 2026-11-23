package deploy

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// waitTask waits until a run has a final status and its goroutine is done.
func (h *harness) waitTask(id int64) store.TaskRun {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.store.GetTaskRun(context.Background(), id)
		if err != nil {
			h.t.Fatalf("GetTaskRun: %v", err)
		}
		h.engine.mu.Lock()
		_, going := h.engine.tasks[id]
		h.engine.mu.Unlock()
		if store.IsTerminal(run.Status) && !going {
			return run
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("task run %d did not finish", id)
	return store.TaskRun{}
}

func (h *harness) taskLog(slug string, id int64) string {
	h.t.Helper()
	data, err := os.ReadFile(h.engine.TaskLogPath(slug, id))
	if err != nil {
		h.t.Fatalf("read task log: %v", err)
	}
	return string(data)
}

// onRun scripts `docker compose run`: out is printed, err returned.
func (h *harness) onRun(fn func(ctx context.Context, cmd runner.Cmd) error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.onCompose = func(ctx context.Context, sub string, cmd runner.Cmd) error {
		if sub != "run" {
			return nil
		}
		return fn(ctx, cmd)
	}
}

func TestRunTaskUsesTheCurrentRelease(t *testing.T) {
	h, app := deployedApp(t)
	h.setEnv(app, map[string]string{"SECRET_KEY_BASE": "very-secret-value"})
	// The live env file still has a value that was deleted from the
	// database since: the container gets it, so it is masked too.
	if err := os.WriteFile(h.engine.EnvFile("blog"), []byte("OLD_TOKEN='old token value'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.onRun(func(_ context.Context, cmd runner.Cmd) error {
		_, _ = io.WriteString(cmd.Stdout, "== 20261121 CreatePosts: migrated\n")
		_, _ = io.WriteString(cmd.Stderr, "key=very-secret-value old=old token value\n")
		return nil
	})

	run, err := h.engine.RunTask(context.Background(), app, TaskSpec{
		TaskName: "migrate", Service: "web", Command: "bin/rails db:migrate",
	})
	if err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if run.Status != store.StatusRunning || run.TaskName != "migrate" || run.DeploymentID == nil ||
		*run.DeploymentID != *app.CurrentDeploymentID {
		t.Fatalf("run = %+v", run)
	}
	done := h.waitTask(run.ID)
	if done.Status != store.StatusSucceeded || done.ExitCode == nil || *done.ExitCode != 0 || done.Error != "" {
		t.Fatalf("finished = %+v", done)
	}

	call := h.lastCall("run")
	release := h.engine.ReleaseDir("blog", *app.CurrentDeploymentID)
	want := []string{"compose", "-p", "koroku-blog",
		"-f", filepath.Join(release, "compose.yaml"), "-f", filepath.Join(release, compose.OverrideFileName),
		"--env-file", h.engine.EnvFile("blog"), "--env-file", h.engine.ReleaseEnvFile("blog"),
		"run", "--rm", "--no-deps", "-T", "--name", "koroku-blog-task-" + itoa(run.ID),
		"web", "sh", "-c", "bin/rails db:migrate"}
	if !slices.Equal(call.Args, want) {
		t.Errorf("run args =\n%v\nwant\n%v", call.Args, want)
	}
	if call.Dir != release {
		t.Errorf("dir = %q", call.Dir)
	}
	// The release files are checked again before they are used.
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config", "run"}) {
		t.Errorf("docker calls = %v", got)
	}

	log := h.taskLog("blog", run.ID)
	for _, line := range []string{
		"$ bin/rails db:migrate",
		"Running in service web of deployment #" + itoa(*app.CurrentDeploymentID) + ".",
		"== 20261121 CreatePosts: migrated",
		"Exited with code 0.",
	} {
		if !strings.Contains(log, line+"\n") {
			t.Errorf("log lacks %q:\n%s", line, log)
		}
	}
	if strings.Contains(log, "very-secret-value") || strings.Contains(log, "old token value") {
		t.Errorf("values must be masked:\n%s", log)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestRunTaskRecordsTheExitCode(t *testing.T) {
	h, app := deployedApp(t)
	h.onRun(func(_ context.Context, cmd runner.Cmd) error {
		_, _ = io.WriteString(cmd.Stderr, "boom\n")
		return &runner.ExitError{Cmd: "docker", Code: 3}
	})
	run, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	done := h.waitTask(run.ID)
	if done.Status != store.StatusFailed || done.ExitCode == nil || *done.ExitCode != 3 ||
		done.Error != "exited with code 3" || done.TaskName != "" {
		t.Fatalf("finished = %+v", done)
	}
	if log := h.taskLog("blog", run.ID); !strings.Contains(log, "boom\nExited with code 3.\n") {
		t.Errorf("log:\n%s", log)
	}
	// Nothing to clean up: the container ended by itself.
	for _, call := range h.runner.Calls() {
		if len(call.Args) > 0 && call.Args[0] == "rm" {
			t.Errorf("unexpected container removal: %v", call.Args)
		}
	}
}

func TestRunTaskRefusals(t *testing.T) {
	t.Run("not deployed", func(t *testing.T) {
		h := newHarness(t, nil)
		app := h.createApp("blog")
		_, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "true"})
		if !errors.Is(err, ErrNotDeployed) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("release files gone", func(t *testing.T) {
		h, app := deployedApp(t)
		if err := os.RemoveAll(h.engine.ReleaseDir("blog", *app.CurrentDeploymentID)); err != nil {
			t.Fatal(err)
		}
		_, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "true"})
		if !errors.Is(err, ErrNotDeployed) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown service", func(t *testing.T) {
		h, app := deployedApp(t)
		_, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "worker", Command: "true"})
		var unknown *UnknownServiceError
		if !errors.As(err, &unknown) || unknown.Service != "worker" {
			t.Fatalf("err = %v", err)
		}
		if slices.Contains(h.dockerCalls(), "run") {
			t.Error("nothing may run")
		}
		runs, _ := h.store.ListTaskRuns(context.Background(), app.ID, 10)
		if len(runs) != 0 {
			t.Errorf("a refused run must not be recorded: %v", runs)
		}
	})
	t.Run("policy", func(t *testing.T) {
		h, app := deployedApp(t)
		h.mu.Lock()
		h.repoServices = `{"web": {"image": "x", "privileged": true}}`
		h.mu.Unlock()
		_, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "true"})
		var policyErr *compose.PolicyError
		if !errors.As(err, &policyErr) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("deployment queued", func(t *testing.T) {
		h := newHarness(t, nil)
		h.createApp("blog")
		h.start()
		h.deploy("blog", shaA)
		app := h.reload("blog")
		// A deployment that waits for the app lock stays queued.
		release, err := h.engine.beginOp(context.Background(), app)
		if err != nil {
			t.Fatal(err)
		}
		h.enqueue("blog", shaB)
		release()
		_, err = h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "true"})
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestRunningTaskHoldsTheApp(t *testing.T) {
	h, app := deployedApp(t)
	started := make(chan struct{})
	finish := make(chan struct{})
	h.onRun(func(context.Context, runner.Cmd) error {
		close(started)
		<-finish
		return nil
	})
	run, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "sleep 60"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	ctx := context.Background()
	if err := h.engine.Stop(ctx, app); !errors.Is(err, ErrBusy) {
		t.Errorf("Stop err = %v", err)
	}
	if _, err := h.engine.RunTask(ctx, app, TaskSpec{Service: "web", Command: "true"}); !errors.Is(err, ErrBusy) {
		t.Errorf("second RunTask err = %v", err)
	}
	if _, err := h.engine.Enqueue(ctx, app, "main", shaB, ""); !errors.Is(err, ErrBusy) {
		t.Errorf("Enqueue err = %v", err)
	}
	if _, err := h.engine.EnqueueRollback(ctx, app, *app.CurrentDeploymentID); !errors.Is(err, ErrBusy) {
		t.Errorf("EnqueueRollback err = %v", err)
	}
	if active, _ := h.store.HasActiveDeployment(ctx, app.ID); active {
		t.Error("a refused deployment must not be recorded")
	}

	// Another app is not held.
	other := h.createApp("shop")
	if _, err := h.engine.Enqueue(ctx, other, "main", shaA, ""); err != nil {
		t.Errorf("Enqueue of another app: %v", err)
	}

	close(finish)
	h.waitTask(run.ID)
	if _, err := h.engine.Enqueue(ctx, app, "main", shaB, ""); err != nil {
		t.Errorf("Enqueue after the run: %v", err)
	}
}

func TestCancelTask(t *testing.T) {
	h, app := deployedApp(t)
	started := make(chan struct{})
	h.onRun(func(ctx context.Context, _ runner.Cmd) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	run, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "sleep 600"})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := h.engine.CancelTask(context.Background(), run.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	done := h.waitTask(run.ID)
	if done.Status != store.StatusCanceled || done.ExitCode != nil || done.Error != "canceled by user" {
		t.Fatalf("finished = %+v", done)
	}
	if !strings.HasSuffix(h.taskLog("blog", run.ID), "Canceled.\n") {
		t.Errorf("log:\n%s", h.taskLog("blog", run.ID))
	}
	assertContainerRemoved(t, h, run.ID)
	if err := h.engine.CancelTask(context.Background(), run.ID); !errors.Is(err, ErrNotCancelable) {
		t.Errorf("second cancel err = %v", err)
	}
	if err := h.engine.CancelTask(context.Background(), 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cancel of a missing run err = %v", err)
	}
}

func assertContainerRemoved(t *testing.T, h *harness, id int64) {
	t.Helper()
	want := []string{"rm", "-f", "koroku-blog-task-" + itoa(id)}
	for _, call := range h.runner.Calls() {
		if call.Name == "docker" && slices.Equal(call.Args, want) {
			return
		}
	}
	t.Errorf("container not removed; calls: %v", h.runner.Calls())
}

func TestTaskTimeout(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.TaskTimeout = 50 * time.Millisecond })
	h.createApp("blog")
	h.start()
	h.deploy("blog", shaA)
	app := h.reload("blog")
	h.onRun(func(ctx context.Context, _ runner.Cmd) error {
		<-ctx.Done()
		return ctx.Err()
	})
	run, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "sleep 600"})
	if err != nil {
		t.Fatal(err)
	}
	done := h.waitTask(run.ID)
	if done.Status != store.StatusFailed || done.Error != "timed out after 50ms" {
		t.Fatalf("finished = %+v", done)
	}
	assertContainerRemoved(t, h, run.ID)
}

func TestTaskRunsArePruned(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.KeepTaskRuns = 2 })
	h.createApp("blog")
	h.start()
	h.deploy("blog", shaA)
	app := h.reload("blog")
	var ids []int64
	for i := 0; i < 3; i++ {
		run, err := h.engine.RunTask(context.Background(), app, TaskSpec{Service: "web", Command: "true"})
		if err != nil {
			t.Fatal(err)
		}
		h.waitTask(run.ID)
		ids = append(ids, run.ID)
	}
	runs, _ := h.store.ListTaskRuns(context.Background(), app.ID, 10)
	if len(runs) != 2 || runs[0].ID != ids[2] || runs[1].ID != ids[1] {
		t.Fatalf("runs = %+v", runs)
	}
	if _, err := os.Stat(h.engine.TaskLogPath("blog", ids[0])); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("log of the pruned run: %v", err)
	}
	if _, err := os.Stat(h.engine.TaskLogPath("blog", ids[2])); err != nil {
		t.Errorf("log of a kept run: %v", err)
	}
}

func TestInterruptedTaskRunsAreFailedOnStart(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	run, err := h.store.StartTaskRun(context.Background(), store.TaskRun{
		AppID: app.ID, Service: "web", Command: "sleep 600",
	}, h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.start()
	// Recovery marks the run failed in the database before it appends the
	// log line, so wait for both rather than reading the log as soon as the
	// status changes.
	want := "Failed: " + store.InterruptedTaskError
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := h.store.GetTaskRun(context.Background(), run.ID)
		log, _ := os.ReadFile(h.engine.TaskLogPath("blog", run.ID))
		if got.Status == store.StatusFailed && strings.Contains(string(log), want) {
			if got.Error != store.InterruptedTaskError {
				t.Errorf("error = %q", got.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run = %+v, log = %q", got, log)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestConsoleCmd(t *testing.T) {
	h, app := deployedApp(t)
	release := h.engine.ReleaseDir("blog", *app.CurrentDeploymentID)
	base := []string{"compose", "-p", "koroku-blog",
		"-f", filepath.Join(release, "compose.yaml"), "-f", filepath.Join(release, compose.OverrideFileName),
		"--env-file", h.engine.EnvFile("blog"), "--env-file", h.engine.ReleaseEnvFile("blog")}

	cmd, err := h.engine.ConsoleCmd(context.Background(), app, "web", []string{"bin/rails", "console"}, true)
	if err != nil {
		t.Fatalf("ConsoleCmd: %v", err)
	}
	want := append(slices.Clone(base), "run", "--rm", "--no-deps", "web", "bin/rails", "console")
	if cmd.Name != "docker" || !slices.Equal(cmd.Args, want) || cmd.Dir != release {
		t.Errorf("cmd = %+v\nwant args %v", cmd, want)
	}
	// The release files are checked again; nothing runs yet.
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config"}) {
		t.Errorf("docker calls = %v", got)
	}

	// Without a terminal no TTY is allocated; no command opens a shell.
	cmd, err = h.engine.ConsoleCmd(context.Background(), app, "web", nil, false)
	if err != nil {
		t.Fatalf("ConsoleCmd: %v", err)
	}
	want = append(slices.Clone(base), "run", "--rm", "--no-deps", "-T", "web", "sh")
	if !slices.Equal(cmd.Args, want) {
		t.Errorf("args = %v\nwant %v", cmd.Args, want)
	}
	// Nothing is recorded.
	runs, _ := h.store.ListTaskRuns(context.Background(), app.ID, 10)
	if len(runs) != 0 {
		t.Errorf("runs = %v", runs)
	}
}

func TestConsoleCmdRefusals(t *testing.T) {
	t.Run("not deployed", func(t *testing.T) {
		h := newHarness(t, nil)
		app := h.createApp("blog")
		if _, err := h.engine.ConsoleCmd(context.Background(), app, "web", nil, true); !errors.Is(err, ErrNotDeployed) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown service", func(t *testing.T) {
		h, app := deployedApp(t)
		_, err := h.engine.ConsoleCmd(context.Background(), app, "worker", nil, true)
		var unknown *UnknownServiceError
		if !errors.As(err, &unknown) || unknown.Service != "worker" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("policy", func(t *testing.T) {
		h, app := deployedApp(t)
		h.mu.Lock()
		h.repoServices = `{"web": {"image": "x", "privileged": true}}`
		h.mu.Unlock()
		_, err := h.engine.ConsoleCmd(context.Background(), app, "web", nil, true)
		var policyErr *compose.PolicyError
		if !errors.As(err, &policyErr) {
			t.Fatalf("err = %v", err)
		}
	})
}
