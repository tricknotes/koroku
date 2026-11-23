package deploy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// Sleeping while idle (ARCHITECTURE 8.4): an app that saw no request for a
// while is stopped with its hostnames routed to koroku, and the next
// request wakes it.

// wakeFailureMemory is how long a failed wake is answered from memory
// instead of starting the containers again for every request.
const wakeFailureMemory = time.Minute

// lockPollInterval is how often a wake tries the app's operation lock
// while another operation holds it.
var lockPollInterval = 100 * time.Millisecond

// wakeCall is one wake in progress; the requests that arrive meanwhile
// wait for it.
type wakeCall struct {
	done chan struct{}
	err  error
}

type wakeFailure struct {
	at  time.Time
	err error
}

// heldLockKey marks a context whose caller holds the operation lock of an
// app, so that work done for it (the backup before a deployment) does not
// wait for that lock.
type heldLockKey struct{ appID int64 }

func withHeldLock(ctx context.Context, appID int64) context.Context {
	return context.WithValue(ctx, heldLockKey{appID}, true)
}

// lockApp waits for the operation lock of the app until ctx ends. The
// lock of a deployment that is only queued is free: a wake does not wait
// behind the single deploy worker.
func (e *Engine) lockApp(ctx context.Context, appID int64) (release func(), err error) {
	if held, _ := ctx.Value(heldLockKey{appID}).(bool); held {
		return func() {}, nil
	}
	lock := e.locks.get(appID)
	for {
		if lock.TryLock() {
			return lock.Unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPollInterval):
		}
	}
}

// wakeTimeout bounds one wake: the containers starting, then the HTTP
// check.
func (e *Engine) wakeTimeout() time.Duration {
	return e.opts.WaitTimeout + e.opts.HealthTimeout + time.Minute
}

// Sleep puts an idle app to sleep: it records the sleep, routes the app's
// hostnames to koroku and stops every service of the project. It returns
// ErrBusy without waiting when a deployment, a command, a copy or another
// operation holds the app; the caller tries again later. An app that is
// stopped, not deployed, already asleep or no longer set to sleep is left
// as it is.
func (e *Engine) Sleep(ctx context.Context, app store.App) error {
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return err
	}
	defer release()
	current, err := e.opts.Store.GetAppByID(ctx, app.ID)
	if err != nil {
		return err
	}
	if !current.IdleSleep {
		return nil
	}
	slept, err := e.opts.Store.MarkAppSleeping(ctx, app.ID, e.opts.Clock.Now())
	if err != nil || !slept {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	// The route goes first: a request that arrives while the containers
	// stop waits for the wake instead of reaching a dying app.
	e.syncRoutes(ctx, "app went to sleep")
	if err := e.runCompose(ctx, e.projectOnly(current), "", "stop"); err != nil {
		undoCtx, cancelUndo := context.WithTimeout(context.WithoutCancel(ctx), e.finishTimeout)
		defer cancelUndo()
		if _, undoErr := e.opts.Store.MarkAppAwake(undoCtx, app.ID); undoErr != nil {
			e.opts.Logger.Error("undoing a failed sleep failed", "app", app.Slug, "error", undoErr)
		}
		e.syncRoutes(undoCtx, "sleep failed")
		return fmt.Errorf("put %s to sleep: %w", app.Slug, err)
	}
	e.opts.Logger.Info("app went to sleep", "app", app.Slug, "idle_minutes", current.IdleSleepMinutes)
	return nil
}

// Wake wakes a sleeping app and returns once it answers, or when ctx ends.
// Callers for the same app share one wake, which runs on its own: it goes
// on when ctx ends. It returns nil at once for an app that is not asleep.
// A wake that failed within the last minute fails again without trying.
func (e *Engine) Wake(ctx context.Context, app store.App) error {
	now := e.opts.Clock.Now()
	e.mu.Lock()
	if f, ok := e.wakeFailed[app.ID]; ok && now.Sub(f.at) < wakeFailureMemory {
		e.mu.Unlock()
		return f.err
	}
	call, ok := e.wakes[app.ID]
	if !ok {
		call = &wakeCall{done: make(chan struct{})}
		e.wakes[app.ID] = call
		go e.runWake(e.lifetime, app, call)
	}
	e.mu.Unlock()
	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (e *Engine) runWake(lifetime context.Context, app store.App, call *wakeCall) {
	ctx, cancel := context.WithTimeout(lifetime, e.wakeTimeout())
	defer cancel()
	err := func() error {
		release, err := e.lockApp(ctx, app.ID)
		if err != nil {
			return fmt.Errorf("wake %s: wait for the app's other operation: %w", app.Slug, err)
		}
		defer release()
		_, err = e.wakeLocked(ctx, app.ID, nil)
		return err
	}()
	e.mu.Lock()
	delete(e.wakes, app.ID)
	if err != nil && lifetime.Err() == nil {
		e.wakeFailed[app.ID] = wakeFailure{at: e.opts.Clock.Now(), err: err}
		e.opts.Logger.Warn("waking the app failed", "app", app.Slug, "error", err)
	} else {
		delete(e.wakeFailed, app.ID)
	}
	call.err = err
	e.mu.Unlock()
	close(call.done)
}

// wakeLocked wakes the app if it sleeps; the caller holds its operation
// lock. say, when set, receives a line for the log of the operation that
// needs the app awake. It reports whether the app was woken.
func (e *Engine) wakeLocked(ctx context.Context, appID int64, say func(format string, args ...any)) (bool, error) {
	app, err := e.opts.Store.GetAppByID(ctx, appID)
	if err != nil {
		return false, err
	}
	if app.SleepingSince == nil {
		return false, nil
	}
	if say != nil {
		say("The app is asleep (since %s); waking it up.", app.SleepingSince.UTC().Format(time.RFC3339))
	}
	// The containers the sleep stopped start as they were: no new
	// containers, no image or policy check (ARCHITECTURE 8.4).
	// A blue/green app names its services: `start` would also try the
	// colour without a container (ARCHITECTURE 7.2).
	start := append([]string{"start", "--wait", "--wait-timeout", e.waitSeconds()}, e.liveServices(app)...)
	err = e.runCompose(ctx, e.projectOnly(app), "", start...)
	if err != nil && ctx.Err() == nil && !e.hasContainers(ctx, app) {
		// Someone removed the containers: start the release again.
		err = e.upCurrent(ctx, app)
	}
	if err == nil {
		err = e.opts.Health.Wait(ctx, healthURL(app), e.opts.HealthTimeout)
	}
	if err != nil {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
		defer cancel()
		if stopErr := e.runCompose(stopCtx, e.projectOnly(app), "", "stop"); stopErr != nil {
			e.opts.Logger.Warn("stopping the app again after a failed wake failed", "app", app.Slug, "error", stopErr)
		}
		return false, fmt.Errorf("wake %s: %w", app.Slug, err)
	}
	if _, err := e.opts.Store.MarkAppAwake(ctx, app.ID); err != nil {
		return false, err
	}
	e.syncRoutes(ctx, "app woke up")
	e.opts.Logger.Info("app woke up", "app", app.Slug)
	if say != nil {
		say("The app is awake.")
	}
	return true, nil
}

// hasContainers reports whether the project has containers, running or
// not. An error counts as having them: the start error is the one to show.
func (e *Engine) hasContainers(ctx context.Context, app store.App) bool {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{
		Name: "docker", Args: e.projectOnly(app).Args("ps", "--all", "--quiet"),
	})
	return err != nil || strings.TrimSpace(string(out)) != ""
}

// WithServiceRunning runs fn while service of the app runs. An awake app
// runs fn as it is. For a sleeping app, under its operation lock, only
// service is started for fn and the project is stopped again afterwards:
// the app stays asleep and its route does not change (ARCHITECTURE 8.4).
// The dump hook of a backup uses it.
func (e *Engine) WithServiceRunning(ctx context.Context, appID int64, service string, fn func(context.Context) error) error {
	return e.withServicesRunning(ctx, appID, []string{service}, nil, fn)
}

// withServicesRunning is WithServiceRunning for several services, which
// may be none: then a sleeping app runs fn with nothing started. A caller
// that holds the app's lock passes a context from withHeldLock. say, when
// set, receives lines for the log of the operation.
func (e *Engine) withServicesRunning(ctx context.Context, appID int64, services []string,
	say func(format string, args ...any), fn func(context.Context) error) error {
	app, err := e.opts.Store.GetAppByID(ctx, appID)
	if err != nil {
		return err
	}
	if app.SleepingSince == nil {
		return fn(ctx)
	}
	release, err := e.lockApp(ctx, appID)
	if err != nil {
		return fmt.Errorf("wait for the app's other operation: %w", err)
	}
	defer release()
	if app, err = e.opts.Store.GetAppByID(ctx, appID); err != nil {
		return err
	}
	if app.SleepingSince == nil {
		return fn(ctx)
	}
	if len(services) == 0 {
		if say != nil {
			say("The app is asleep and stays asleep: the service depends on no other service.")
		}
		return fn(ctx)
	}
	// The public service of a blue/green app runs in its colour.
	live := make([]string, len(services))
	for i, name := range services {
		live[i] = app.LiveService(name)
	}
	services = live
	if say != nil {
		say("The app is asleep and stays asleep; starting only %s.", strings.Join(services, ", "))
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
		defer cancel()
		if err := e.runCompose(stopCtx, e.projectOnly(app), "", "stop"); err != nil {
			e.opts.Logger.Warn("stopping the sleeping app again failed", "app", app.Slug, "error", err)
			if say != nil {
				say("Stopping %s again failed: %v", strings.Join(services, ", "), err)
			}
		} else if say != nil {
			say("Stopped %s again.", strings.Join(services, ", "))
		}
	}()
	start := append([]string{"start", "--wait", "--wait-timeout", e.waitSeconds()}, services...)
	err = e.runCompose(ctx, e.projectOnly(app), "", start...)
	if err != nil && ctx.Err() == nil && !e.hasContainers(ctx, app) {
		// Someone removed the containers: create the services again from
		// the release.
		err = e.upCurrent(ctx, app, services...)
	}
	if err != nil {
		return &ServicesStartError{Services: services, Err: err}
	}
	return fn(ctx)
}

// ServicesStartError is the failure to start services of a sleeping app
// for an operation that needs them.
type ServicesStartError struct {
	Services []string
	Err      error
}

func (e *ServicesStartError) Error() string {
	return fmt.Sprintf("start %s of the sleeping app: %v", strings.Join(e.Services, ", "), e.Err)
}

func (e *ServicesStartError) Unwrap() error { return e.Err }

// endSleep records that a deployment is about to start the app's
// containers: whatever it starts runs awake.
func (e *Engine) endSleep(ctx context.Context, app store.App, log *deployLog) error {
	woke, err := e.opts.Store.MarkAppAwake(ctx, app.ID)
	if err != nil {
		return err
	}
	if woke {
		log.Printf("The app was asleep; this deployment wakes it.")
	}
	return nil
}

func healthURL(app store.App) string {
	return fmt.Sprintf("http://127.0.0.1:%d%s", app.LivePort(), app.HealthcheckPath)
}
