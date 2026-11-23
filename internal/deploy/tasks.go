package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// TaskSpec is what to run: a command in one service of the app. TaskName
// is the registered task it came from, or empty for an ad-hoc command.
type TaskSpec struct {
	TaskName string
	Service  string
	Command  string
	// Scheduled marks a run started by the task's schedule
	// (ARCHITECTURE 9.6), DueAt being the scheduled time it is for. A
	// scheduled run does not wake a sleeping app: it starts only the
	// services the task's service depends on.
	Scheduled bool
	DueAt     time.Time
}

// UnknownServiceError is returned when the service to run a command in is
// not part of the app's current release.
type UnknownServiceError struct {
	Service  string
	Services []string
}

func (e *UnknownServiceError) Error() string {
	return fmt.Sprintf("service %q is not in the current release (services: %v)", e.Service, e.Services)
}

type runningTask struct {
	appID  int64
	cancel context.CancelCauseFunc
	// done is closed when the run has ended and its result is recorded.
	done chan struct{}
}

// cleanupTimeout bounds the removal of a run's container after it was
// stopped, and the database writes that record the result.
const cleanupTimeout = 30 * time.Second

// TaskLogPath is the output log of one run.
func (e *Engine) TaskLogPath(slug string, id int64) string {
	return filepath.Join(e.appDir(slug), "tasks", strconv.FormatInt(id, 10)+".log")
}

// taskContainerName names the container of a run, so that it can be
// removed when the run is stopped: killing `docker compose run` leaves its
// container running.
func taskContainerName(app store.App, id int64) string {
	return fmt.Sprintf("%s-task-%d", app.ProjectName(), id)
}

func (e *Engine) taskRunning(appID int64) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, t := range e.tasks {
		if t.appID == appID {
			return true
		}
	}
	return false
}

// RunTask starts a command in a service of the app's current release
// (ARCHITECTURE 9.6) and returns the run as recorded. The command runs in
// the background with the app's operation lock held; follow it through its
// log and the database. It fails with ErrNotDeployed without the files of a
// current release, ErrBusy while a deployment is queued or running or
// another operation holds the app, *UnknownServiceError for a service the
// release does not have and *compose.PolicyError when the release files no
// longer pass the policy.
func (e *Engine) RunTask(ctx context.Context, app store.App, spec TaskSpec) (store.TaskRun, error) {
	if app.CurrentDeploymentID == nil {
		return store.TaskRun{}, ErrNotDeployed
	}
	e.admit.Lock()
	release, err := e.beginOp(ctx, app)
	e.admit.Unlock()
	if err != nil {
		return store.TaskRun{}, err
	}
	run, err := e.startTask(ctx, app, spec, release)
	if err != nil {
		release()
		return store.TaskRun{}, err
	}
	return run, nil
}

// startTask checks the release, records the run and starts it. From a nil
// error on, release belongs to the run and is called when it ends.
func (e *Engine) startTask(ctx context.Context, app store.App, spec TaskSpec, release func()) (store.TaskRun, error) {
	inv, dir, ok := e.currentInvocation(app)
	if !ok {
		return store.TaskRun{}, ErrNotDeployed
	}
	deploymentID := *app.CurrentDeploymentID
	meta, err := e.readMeta(app.Slug, deploymentID)
	if err != nil {
		return store.TaskRun{}, ErrNotDeployed
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Minute)
	project, err := e.revalidatedProject(checkCtx, app, deploymentID, meta.ComposeFile)
	cancel()
	if err != nil {
		return store.TaskRun{}, err
	}
	if _, ok := project.Services[spec.Service]; !ok {
		return store.TaskRun{}, &UnknownServiceError{Service: spec.Service, Services: project.ServiceNames()}
	}
	var deps []string
	if spec.Scheduled {
		deps = project.Dependencies(spec.Service)
	}
	masker, err := e.taskMasker(ctx, app)
	if err != nil {
		return store.TaskRun{}, err
	}

	// The checks above took a while: a deployment may have been accepted
	// meanwhile. Checking again and registering the run under the admit
	// lock makes the two exclude each other.
	e.admit.Lock()
	active, err := e.opts.Store.HasActiveDeployment(ctx, app.ID)
	if err != nil || active {
		e.admit.Unlock()
		if err != nil {
			return store.TaskRun{}, err
		}
		return store.TaskRun{}, ErrBusy
	}
	trigger := store.TaskTriggerManual
	if spec.Scheduled {
		trigger = store.TaskTriggerScheduled
	}
	run, err := e.opts.Store.StartTaskRun(ctx, store.TaskRun{
		AppID: app.ID, TaskName: spec.TaskName, Service: spec.Service, Command: spec.Command,
		DeploymentID: &deploymentID, Trigger: trigger,
	}, e.opts.Clock.Now())
	if err != nil {
		e.admit.Unlock()
		return store.TaskRun{}, err
	}
	e.mu.Lock()
	lifetime := e.lifetime
	jobCtx, cancelJob := context.WithCancelCause(lifetime)
	done := make(chan struct{})
	e.tasks[run.ID] = &runningTask{appID: app.ID, cancel: cancelJob, done: done}
	e.mu.Unlock()
	e.admit.Unlock()

	path := e.TaskLogPath(app.Slug, run.ID)
	// A file under this ID can only be left over from an earlier database.
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.opts.Logger.Warn("removing a stale task log failed", "path", path, "error", err)
	}
	log, err := openDeployLog(path, masker, e.opts.Clock.Now)
	if err != nil {
		e.mu.Lock()
		delete(e.tasks, run.ID)
		e.mu.Unlock()
		cancelJob(nil)
		e.finishTask(run.ID, store.StatusFailed, nil, "the log could not be opened")
		e.queuePendingAutoDeploy(lifetime, app.ID)
		return store.TaskRun{}, err
	}

	go func() {
		// The result is recorded last, once the app's lock is free and a
		// deferred push is queued: a run seen as finished means the app
		// takes the next operation (ARCHITECTURE 9.6).
		var record func()
		defer func() {
			release()
			if record != nil {
				record()
			}
		}()
		defer func() {
			e.mu.Lock()
			delete(e.tasks, run.ID)
			e.mu.Unlock()
			cancelJob(nil)
			// A push that arrived during the run waited for it. The run is
			// no longer registered, so a push from now on is queued
			// directly (ARCHITECTURE 5.2).
			e.queuePendingAutoDeploy(lifetime, app.ID)
			close(done)
		}()
		defer log.Close()
		record = e.executeTask(lifetime, jobCtx, app, run, spec, deps, inv.Args(
			"run", "--rm", "--no-deps", "-T", "--name", taskContainerName(app, run.ID),
			spec.Service, "sh", "-c", spec.Command,
		), dir, log, masker)
	}()
	return run, nil
}

// taskMasker hides the values a run's container can print: the saved
// variables and the ones in the live env file, which may still hold values
// deleted from the database since the last deployment.
func (e *Engine) taskMasker(ctx context.Context, app store.App) (*secrets.Masker, error) {
	masker := secrets.NewMasker()
	if _, _, err := e.renderEnv(ctx, app, "", masker); err != nil {
		return nil, err
	}
	content, err := os.ReadFile(e.EnvFile(app.Slug))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read env file: %w", err)
	}
	masker.Add(ParseEnvFileValues(string(content))...)
	return masker, nil
}

// executeTask runs the command and writes its log. It returns how to record
// the result, which the caller does once the app's lock is released.
func (e *Engine) executeTask(lifetime, jobCtx context.Context, app store.App, run store.TaskRun,
	spec TaskSpec, deps []string, args []string, dir string, log *deployLog, masker *secrets.Masker) (record func()) {
	logger := e.opts.Logger.With("app", app.Slug, "task_run", run.ID)
	ctx, cancel := context.WithTimeout(jobCtx, e.opts.TaskTimeout)
	defer cancel()

	log.Printf("$ %s", run.Command)
	if spec.Scheduled {
		log.Printf("Scheduled run of task %q for %s.", run.TaskName, spec.DueAt.Format(time.RFC3339))
	}
	log.Printf("Running in service %s of deployment #%d.", run.Service, *run.DeploymentID)
	logger.Info("command started", "service", run.Service, "task", run.TaskName, "trigger", run.Trigger)
	command := func(ctx context.Context) error {
		return e.opts.Runner.Run(ctx, runner.Cmd{
			Name: "docker", Args: args, Dir: dir, Stdout: log.Writer(), Stderr: log.Writer(),
		})
	}
	var err error
	if spec.Scheduled {
		// A scheduled run leaves a sleeping app asleep and starts only
		// the services its service depends on, under the lock this run
		// holds (ARCHITECTURE 8.4).
		err = e.withServicesRunning(withHeldLock(ctx, app.ID), app.ID, deps, log.Printf, command)
	} else {
		// `run --no-deps` needs the services it talks to running: a
		// sleeping app is woken first, under the lock this run holds
		// (ARCHITECTURE 8.4).
		wakeCtx, cancelWake := context.WithTimeout(ctx, e.wakeTimeout())
		_, err = e.wakeLocked(wakeCtx, app.ID, log.Printf)
		cancelWake()
		if err == nil {
			err = command(ctx)
		}
	}

	status, message := store.StatusSucceeded, ""
	var exitCode *int
	var exitErr *runner.ExitError
	var startErr *ServicesStartError
	switch {
	case err == nil:
		code := 0
		exitCode = &code
		log.Printf("Exited with code 0.")
	case lifetime.Err() != nil:
		status, message = store.StatusFailed, store.InterruptedTaskError
		log.Printf("Interrupted: koroku is shutting down.")
	case errors.Is(context.Cause(jobCtx), errCanceledByUser):
		status, message = store.StatusCanceled, "canceled by user"
		log.Printf("Canceled.")
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		status, message = store.StatusFailed, fmt.Sprintf("timed out after %s", e.opts.TaskTimeout)
		log.Printf("Timed out after %s.", e.opts.TaskTimeout)
	case errors.As(err, &startErr):
		// Not the command's exit code: the services it needs did not start.
		status, message = store.StatusFailed, masker.Mask(startErr.Error())
		log.Printf("Failed to run: %s", message)
	case errors.As(err, &exitErr):
		code := exitErr.Code
		exitCode = &code
		status, message = store.StatusFailed, fmt.Sprintf("exited with code %d", code)
		log.Printf("Exited with code %d.", code)
	default:
		status, message = store.StatusFailed, masker.Mask(err.Error())
		log.Printf("Failed to run: %s", message)
	}
	if ctx.Err() != nil {
		// Stopping `docker compose run` leaves the container running.
		e.removeTaskContainer(app, run.ID)
	}
	return func() {
		e.finishTask(run.ID, status, exitCode, message)
		logger.Info("command finished", "status", status)
		e.pruneTaskRuns(app)
	}
}

func (e *Engine) removeTaskContainer(app store.App, id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	// Best effort: the container may be gone already.
	if err := e.opts.Runner.Run(ctx, runner.Cmd{
		Name: "docker", Args: []string{"rm", "-f", taskContainerName(app, id)},
	}); err != nil {
		e.opts.Logger.Debug("removing a task container failed", "app", app.Slug, "task_run", id, "error", err)
	}
}

func (e *Engine) finishTask(id int64, status string, exitCode *int, message string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := e.opts.Store.FinishTaskRun(ctx, id, status, exitCode, message, e.opts.Clock.Now()); err != nil {
		e.opts.Logger.Error("recording the result of a command failed", "task_run", id, "error", err)
	}
}

// pruneTaskRuns keeps the newest runs of the app and deletes the logs of
// the others.
func (e *Engine) pruneTaskRuns(app store.App) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	ids, err := e.opts.Store.PruneTaskRuns(ctx, app.ID, e.opts.KeepTaskRuns)
	if err != nil {
		e.opts.Logger.Warn("pruning old task runs failed", "app", app.Slug, "error", err)
		return
	}
	for _, id := range ids {
		if err := os.Remove(e.TaskLogPath(app.Slug, id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			e.opts.Logger.Warn("removing an old task log failed", "app", app.Slug, "task_run", id, "error", err)
		}
	}
}

// WaitTask waits until the run ends and its result is recorded, or until
// ctx ends. It returns at once for a run that is not in progress.
func (e *Engine) WaitTask(ctx context.Context, id int64) error {
	e.mu.Lock()
	t, ok := e.tasks[id]
	e.mu.Unlock()
	if !ok {
		return nil
	}
	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// CancelTask stops a running command. ErrNotCancelable means it has ended.
func (e *Engine) CancelTask(ctx context.Context, id int64) error {
	if _, err := e.opts.Store.GetTaskRun(ctx, id); err != nil {
		return err
	}
	e.mu.Lock()
	t, ok := e.tasks[id]
	e.mu.Unlock()
	if !ok {
		return ErrNotCancelable
	}
	t.cancel(errCanceledByUser)
	return nil
}

// failInterruptedTaskRuns records runs left running by the previous
// process as failed. Their containers may still exist; they are removed.
func (e *Engine) failInterruptedTaskRuns(ctx context.Context, now time.Time) error {
	runs, err := e.opts.Store.FailInterruptedTaskRuns(ctx, now)
	if err != nil {
		return fmt.Errorf("recover interrupted task runs: %w", err)
	}
	for _, run := range runs {
		app, err := e.opts.Store.GetAppByID(ctx, run.AppID)
		if err != nil {
			continue
		}
		e.opts.Logger.Warn("a command was interrupted by a restart", "app", app.Slug, "task_run", run.ID)
		appendLogLine(e.TaskLogPath(app.Slug, run.ID), "Failed: "+store.InterruptedTaskError+".")
		e.removeTaskContainer(app, run.ID)
		if app.SleepingSince != nil {
			// A scheduled run may have started services of the sleeping
			// app; they must not keep running (ARCHITECTURE 9.6).
			stopCtx, cancel := context.WithTimeout(ctx, opTimeout)
			if err := e.runCompose(stopCtx, e.projectOnly(app), "", "stop"); err != nil {
				e.opts.Logger.Warn("stopping the sleeping app after an interrupted command failed", "app", app.Slug, "error", err)
			}
			cancel()
		}
	}
	return nil
}

// ConsoleCmd builds the command of `koroku run` (ARCHITECTURE 9.6): the
// command runs in a new container of the service, from the current
// release like a task run, but attached to the caller's terminal. With tty
// false (the input is not a terminal) no TTY is allocated and the input is
// passed through. The caller runs it with its own standard streams. It
// fails like RunTask, except that it takes no lock and records nothing.
func (e *Engine) ConsoleCmd(ctx context.Context, app store.App, service string, command []string, tty bool) (runner.Cmd, error) {
	inv, dir, ok := e.currentInvocation(app)
	if !ok {
		return runner.Cmd{}, ErrNotDeployed
	}
	meta, err := e.readMeta(app.Slug, *app.CurrentDeploymentID)
	if err != nil {
		return runner.Cmd{}, ErrNotDeployed
	}
	project, err := e.revalidatedProject(ctx, app, *app.CurrentDeploymentID, meta.ComposeFile)
	if err != nil {
		return runner.Cmd{}, err
	}
	if _, ok := project.Services[service]; !ok {
		return runner.Cmd{}, &UnknownServiceError{Service: service, Services: project.ServiceNames()}
	}
	if len(command) == 0 {
		command = []string{"sh"}
	}
	sub := []string{"run", "--rm", "--no-deps"}
	if !tty {
		sub = append(sub, "-T")
	}
	sub = append(sub, service)
	sub = append(sub, command...)
	return runner.Cmd{Name: "docker", Args: inv.Args(sub...), Env: inv.Env(), Dir: dir}, nil
}
