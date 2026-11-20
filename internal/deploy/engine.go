// Package deploy implements the deploy job: its state machine, the single
// worker that runs jobs one at a time, and the steps of ARCHITECTURE chapter 7.
package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
	"github.com/tricknotes/koroku/internal/system"
)

// Router makes the reverse proxy configuration match the database. Sync is a
// no-op when nothing changed, so ordinary redeploys do not reload the proxy.
type Router interface {
	Sync(ctx context.Context) error
}

// PreDeployBackup backs an app up before its containers are replaced. It is
// used for apps that opted into "backup before deploy".
type PreDeployBackup interface {
	BackupApp(ctx context.Context, app store.App) error
}

// TokenSource mints a short-lived token for fetching an app's repository.
// It returns "" for repositories that need no authentication.
type TokenSource interface {
	RepoToken(ctx context.Context, app store.App) (string, error)
}

// StatusReporter mirrors the status changes of deployments elsewhere (the
// deployments of the GitHub repository, ARCHITECTURE 5.1). Report is called
// after the change is recorded in the database and must return at once:
// reporting never delays or fails a deployment.
type StatusReporter interface {
	Report(change StatusChange)
}

// StatusChange says that a deployment's status changed. The status itself
// is read from the database.
type StatusChange struct {
	DeploymentID int64
	// RolledBack is set on a failed deployment after which the previous
	// release was started again automatically.
	RolledBack bool
}

// Options are the dependencies and settings of the Engine.
type Options struct {
	Store   *store.Store
	Runner  runner.Runner
	Clock   clock.Clock
	Box     *secrets.Box
	System  system.Prober
	Health  HealthChecker
	Fetcher gitrepo.Fetcher
	// Router, Tokens, Backup and Status are optional.
	Router Router
	Tokens TokenSource
	Backup PreDeployBackup
	Status StatusReporter
	// CopyBackups backs up the target of a database copy and reads dump
	// snapshots. Optional.
	CopyBackups CopyBackups
	Logger      *slog.Logger

	// AppsDir holds one directory per app (releases, env file, deploy logs).
	AppsDir string
	// DefaultMemory is the memory limit for services without one.
	DefaultMemory string
	// DiskLimitPercent rejects deployments when the data disk is fuller.
	DiskLimitPercent int
	// WaitTimeout bounds `docker compose up --wait`.
	WaitTimeout time.Duration
	// HealthTimeout bounds the HTTP check after start.
	HealthTimeout time.Duration
	// JobTimeout bounds a whole deployment.
	JobTimeout time.Duration
	// KeepReleases is the number of successful release directories kept.
	KeepReleases int
	// PruneImages runs `docker image prune -f` after a successful deploy.
	PruneImages bool
	// CopyTimeout bounds a whole database copy; CopyMaxBytes bounds the
	// dump it streams (ARCHITECTURE 10.6).
	CopyTimeout  time.Duration
	CopyMaxBytes int64
	// TaskTimeout bounds one run of a one-off command (ARCHITECTURE 9.6).
	TaskTimeout time.Duration
	// KeepTaskRuns is the number of runs (and their logs) kept per app.
	KeepTaskRuns int
}

func (o *Options) applyDefaults() {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Health == nil {
		o.Health = HTTPHealthChecker{}
	}
	if o.Fetcher.Runner == nil {
		o.Fetcher.Runner = o.Runner
	}
	if o.DefaultMemory == "" {
		o.DefaultMemory = "512m"
	}
	if o.DiskLimitPercent == 0 {
		o.DiskLimitPercent = 85
	}
	if o.WaitTimeout == 0 {
		o.WaitTimeout = 300 * time.Second
	}
	if o.HealthTimeout == 0 {
		o.HealthTimeout = 60 * time.Second
	}
	if o.JobTimeout == 0 {
		o.JobTimeout = 30 * time.Minute
	}
	if o.KeepReleases == 0 {
		o.KeepReleases = 3
	}
	if o.CopyTimeout == 0 {
		o.CopyTimeout = defaultCopyTimeout
	}
	if o.CopyMaxBytes == 0 {
		o.CopyMaxBytes = defaultCopyMaxBytes
	}
	if o.TaskTimeout == 0 {
		o.TaskTimeout = 30 * time.Minute
	}
	if o.KeepTaskRuns == 0 {
		o.KeepTaskRuns = 50
	}
}

// Engine accepts deployments and runs them one at a time.
type Engine struct {
	opts Options
	wake chan struct{}

	mu      sync.Mutex
	cancels map[int64]context.CancelCauseFunc
	// base is the context of Run; background work such as a database
	// copy ends with it.
	base context.Context

	// locks serializes deployments and manual operations per app.
	locks appLocks

	// admit makes "is a command running?" and "is a deployment queued?"
	// one decision: a deployment is not accepted while a command runs on
	// the app, nor a command while a deployment is queued or running.
	admit sync.Mutex
	// tasks are the runs in progress, by run ID (guarded by mu).
	tasks map[int64]*runningTask
	// lifetime ends when the worker stops (koroku shuts down); runs that
	// outlive the request that started them are bound to it.
	lifetime context.Context

	// finishTimeout bounds the database writes that record a result.
	finishTimeout time.Duration

	// queueing is held by Enqueue from inserting a deployment until its log
	// file is prepared, and briefly by the worker before it opens a log. The
	// worker can see the new row as soon as it is inserted (it also polls),
	// and would otherwise write its first lines before the stale file is
	// removed and the queued line is written, losing or misordering them.
	queueing sync.Mutex
	// afterInsert, when set by tests, runs between inserting a deployment
	// and preparing its log.
	afterInsert func()
}

// New builds an Engine. Call Run to start the worker.
func New(opts Options) *Engine {
	opts.applyDefaults()
	return &Engine{
		opts:    opts,
		wake:    make(chan struct{}, 1),
		cancels: map[int64]context.CancelCauseFunc{},
		tasks:   map[int64]*runningTask{},

		lifetime: context.Background(),

		finishTimeout: 30 * time.Second,
	}
}

// DiskFullError is returned when the disk usage limit blocks a deployment.
type DiskFullError struct {
	UsedPercent  float64
	LimitPercent int
}

func (e *DiskFullError) Error() string {
	return fmt.Sprintf("disk usage is %.0f%%, above the limit of %d%%; free up space before deploying",
		e.UsedPercent, e.LimitPercent)
}

// ErrNotCancelable is returned when a deployment already finished.
var ErrNotCancelable = errors.New("deployment is not queued or running")

var errCanceledByUser = errors.New("canceled by user")

// --- paths ---

func (e *Engine) appDir(slug string) string { return filepath.Join(e.opts.AppsDir, slug) }

// EnvFile is the env file every service of the app reads.
func (e *Engine) EnvFile(slug string) string { return filepath.Join(e.appDir(slug), "env") }

// ReleaseDir is the checkout of one deployment.
func (e *Engine) ReleaseDir(slug string, id int64) string {
	return filepath.Join(e.appDir(slug), "releases", strconv.FormatInt(id, 10))
}

// LogPath is the deploy log of one deployment.
func (e *Engine) LogPath(slug string, id int64) string {
	return filepath.Join(e.appDir(slug), "deploys", strconv.FormatInt(id, 10)+".log")
}

// stateDir holds what is needed to bring a release back up later: the env
// file as it was at deploy time and the Compose file that was used. It lives
// outside the checkout so that secrets never enter a Docker build context.
func (e *Engine) stateDir(slug string, id int64) string {
	return filepath.Join(e.appDir(slug), "release-state", strconv.FormatInt(id, 10))
}

type releaseMeta struct {
	ComposeFile string `json:"compose_file"`
	// Images are the per-release images built for this release. They are
	// removed together with the release.
	Images []string `json:"images,omitempty"`
}

func (e *Engine) writeMeta(slug string, id int64, meta releaseMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(e.stateDir(slug, id), "release.json"), data, 0o600)
}

func (e *Engine) readMeta(slug string, id int64) (releaseMeta, error) {
	var meta releaseMeta
	data, err := os.ReadFile(filepath.Join(e.stateDir(slug, id), "release.json"))
	if err != nil {
		return meta, err
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return meta, err
	}
	return meta, nil
}

// invocation builds the docker compose call for a release.
func (e *Engine) invocation(app store.App, id int64, composeFile string, withOverride bool) compose.Invocation {
	dir := e.ReleaseDir(app.Slug, id)
	files := []string{filepath.Join(dir, composeFile)}
	if withOverride {
		files = append(files, filepath.Join(dir, compose.OverrideFileName))
	}
	return compose.Invocation{ProjectName: app.ProjectName(), Files: files, EnvFile: e.EnvFile(app.Slug)}
}

// --- accepting and canceling ---

// CheckDisk returns a *DiskFullError when the data disk is above the limit.
// A failure to read disk usage does not block deployments.
func (e *Engine) CheckDisk() error {
	if e.opts.System == nil {
		return nil
	}
	disk, err := e.opts.System.DiskUsage()
	if err != nil {
		e.opts.Logger.Warn("disk usage unavailable; skipping the deploy disk check", "error", err)
		return nil
	}
	if disk.UsedPercent > float64(e.opts.DiskLimitPercent) {
		return &DiskFullError{UsedPercent: disk.UsedPercent, LimitPercent: e.opts.DiskLimitPercent}
	}
	return nil
}

// Enqueue records a deployment of the given commit and wakes the worker.
// Older queued deployments of the same app are canceled.
func (e *Engine) Enqueue(ctx context.Context, app store.App, ref, sha, message string) (store.Deployment, error) {
	return e.enqueue(ctx, app, ref, sha, message, nil)
}

func (e *Engine) enqueue(ctx context.Context, app store.App, ref, sha, message string, promotedFrom *int64) (store.Deployment, error) {
	if !gitrepo.IsFullSHA(sha) {
		return store.Deployment{}, fmt.Errorf("commit %q is not a full SHA", sha)
	}
	if err := e.CheckDisk(); err != nil {
		return store.Deployment{}, err
	}
	// A deployment would wait for the command, and with it every other
	// app's deployment behind the single worker (ARCHITECTURE 9.6).
	e.admit.Lock()
	defer e.admit.Unlock()
	if e.taskRunning(app.ID) {
		return store.Deployment{}, ErrBusy
	}
	e.queueing.Lock()
	defer e.queueing.Unlock()
	d, superseded, err := e.opts.Store.EnqueuePromotion(ctx, app.ID, ref, sha, message, promotedFrom, e.opts.Clock.Now())
	if err != nil {
		return store.Deployment{}, err
	}
	if e.afterInsert != nil {
		e.afterInsert()
	}
	for _, id := range superseded {
		appendLogLine(e.LogPath(app.Slug, id), "Canceled: "+store.SupersededError+".")
		e.report(StatusChange{DeploymentID: id})
	}
	e.report(StatusChange{DeploymentID: d.ID})
	// A file under this ID can only be left over from an earlier database
	// (the local state was reset but the data directory kept); the new
	// deployment must not show its lines.
	path := e.LogPath(app.Slug, d.ID)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.opts.Logger.Warn("removing a stale deploy log failed", "path", path, "error", err)
	}
	if promotedFrom != nil {
		appendLogLine(path, fmt.Sprintf("Queued deployment of %s, promoting deployment #%d.", sha, *promotedFrom))
	} else {
		appendLogLine(path, fmt.Sprintf("Queued deployment of %s.", sha))
	}
	e.notify()
	return d, nil
}

// Cancel stops a queued or running deployment.
func (e *Engine) Cancel(ctx context.Context, id int64) error {
	d, err := e.opts.Store.GetDeployment(ctx, id)
	if err != nil {
		return err
	}
	err = e.opts.Store.CancelQueuedDeployment(ctx, id, e.opts.Clock.Now())
	if err == nil {
		if app, appErr := e.opts.Store.GetAppByID(ctx, d.AppID); appErr == nil {
			appendLogLine(e.LogPath(app.Slug, id), "Canceled before it started.")
		}
		e.report(StatusChange{DeploymentID: id})
		return nil
	}
	if !errors.Is(err, store.ErrInvalidTransition) {
		return err
	}
	e.mu.Lock()
	cancel, running := e.cancels[id]
	e.mu.Unlock()
	if !running {
		return ErrNotCancelable
	}
	cancel(errCanceledByUser)
	return nil
}

// report hands a status change to the reporter, if there is one.
func (e *Engine) report(change StatusChange) {
	if e.opts.Status != nil {
		e.opts.Status.Report(change)
	}
}

func (e *Engine) notify() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// --- worker ---

// Run processes queued deployments until ctx is canceled. Only one
// deployment runs at a time across all apps, which serializes build load.
func (e *Engine) Run(ctx context.Context) error {
	e.mu.Lock()
	e.base = ctx
	e.lifetime = ctx
	e.mu.Unlock()
	now := e.opts.Clock.Now()
	e.failInterruptedCopies(ctx, now)
	if err := e.failInterruptedTaskRuns(ctx, now); err != nil {
		return err
	}
	interrupted, err := e.opts.Store.FailInterruptedDeployments(ctx, now)
	if err != nil {
		return fmt.Errorf("recover interrupted deployments: %w", err)
	}
	for _, id := range interrupted {
		e.opts.Logger.Warn("deployment was interrupted by a restart", "deployment", id)
		e.report(StatusChange{DeploymentID: id})
		if d, err := e.opts.Store.GetDeployment(ctx, id); err == nil {
			if app, err := e.opts.Store.GetAppByID(ctx, d.AppID); err == nil {
				appendLogLine(e.LogPath(app.Slug, id), "Failed: "+store.InterruptedError+".")
			}
		}
	}
	for {
		if ctx.Err() != nil {
			return nil
		}
		d, err := e.opts.Store.NextQueuedDeployment(ctx)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
				e.opts.Logger.Error("reading the deploy queue failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-e.wake:
			case <-time.After(15 * time.Second):
			}
			continue
		}
		e.process(ctx, d)
	}
}

func (e *Engine) process(ctx context.Context, d store.Deployment) {
	logger := e.opts.Logger.With("deployment", d.ID)

	jobCtx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	jobCtx, cancelTimeout := context.WithTimeout(jobCtx, e.opts.JobTimeout)
	defer cancelTimeout()

	// Register before the status changes so that Cancel always finds either
	// a queued row or a cancel function.
	e.mu.Lock()
	e.cancels[d.ID] = cancelCause
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		delete(e.cancels, d.ID)
		e.mu.Unlock()
	}()

	if err := e.opts.Store.StartDeployment(ctx, d.ID, e.opts.Clock.Now()); err != nil {
		if !errors.Is(err, store.ErrInvalidTransition) {
			logger.Error("starting the deployment failed", "error", err)
		}
		return
	}
	e.report(StatusChange{DeploymentID: d.ID})

	// Recording the result must work even while koroku is shutting down, so
	// it gets its own context. The context is created when it is needed: a
	// timeout started here would already have expired after a long deploy.
	rolledBack := false
	finish := func(status, message string) error {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.finishTimeout)
		defer cancel()
		if err := e.opts.Store.FinishDeployment(finishCtx, d.ID, status, message, e.opts.Clock.Now()); err != nil {
			return err
		}
		e.report(StatusChange{DeploymentID: d.ID, RolledBack: rolledBack})
		return nil
	}

	app, err := e.opts.Store.GetAppByID(ctx, d.AppID)
	if err != nil {
		logger.Error("loading the app failed", "error", err)
		_ = finish(store.StatusFailed, "the app could not be loaded")
		return
	}
	logger = logger.With("app", app.Slug)

	masker := secrets.NewMasker()
	// Wait until Enqueue has prepared this deployment's log file.
	e.queueing.Lock()
	e.queueing.Unlock()
	log, err := openDeployLog(e.LogPath(app.Slug, d.ID), masker, e.opts.Clock.Now)
	if err != nil {
		logger.Error("opening the deploy log failed", "error", err)
		_ = finish(store.StatusFailed, "the deploy log could not be opened")
		return
	}
	defer log.Close()

	// A manual operation (stop, restart, ...) on this app finishes first.
	appLock := e.locks.get(app.ID)
	appLock.Lock()
	defer appLock.Unlock()

	logger.Info("deployment started", "commit", d.CommitSHA)
	run := e.execute
	if d.SourceDeploymentID != nil {
		run = e.executeRollback
	}
	touched, runErr := run(jobCtx, d, app, log)

	status, message := store.StatusSucceeded, ""
	if runErr != nil {
		status, message = e.classify(ctx, jobCtx, runErr, masker)
		log.Printf("%s", "")
		log.Step(StepFailed, "Deployment %s: %s", status, message)
		if touched {
			if ctx.Err() != nil {
				log.Printf("koroku is shutting down; the previous release is not restored automatically.")
			} else {
				rolledBack = e.rollback(app, log)
			}
		}
	}

	if err := finish(status, message); err != nil {
		logger.Error("recording the deployment result failed", "error", err)
		return
	}
	logger.Info("deployment finished", "status", status)

	if status == store.StatusSucceeded {
		log.Step(StepDone, "Deployment succeeded")
		if app.DesiredState != store.DesiredRunning {
			stateCtx, cancelState := context.WithTimeout(context.WithoutCancel(ctx), e.finishTimeout)
			_ = e.opts.Store.SetAppDesiredState(stateCtx, app.ID, store.DesiredRunning, e.opts.Clock.Now())
			cancelState()
		}
		if e.opts.Router != nil {
			// The route step ran before this deployment became the current
			// one, so an app's first release (or one that was stopped) is
			// still routed to the 503 page. Sync is a no-op when nothing
			// changed; a failure is retried by the periodic sync.
			routeCtx, cancelRoute := context.WithTimeout(context.WithoutCancel(ctx), e.finishTimeout)
			if err := e.opts.Router.Sync(routeCtx); err != nil {
				logger.Error("updating routes after the deployment failed; will retry", "error", err)
			}
			cancelRoute()
		}
		cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancelCleanup()
		e.cleanupAfterSuccess(cleanupCtx, app, log)
	} else if ctx.Err() == nil {
		// A failed checkout is never needed again; its log stays. During
		// shutdown it is left for the cleanup after the next success.
		cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancelCleanup()
		e.removeRelease(cleanupCtx, app.Slug, d.ID)
	}
}

// classify maps a job error to the final status and the message stored in
// the database. The message is masked like the log.
func (e *Engine) classify(parent, job context.Context, err error, masker *secrets.Masker) (string, string) {
	switch {
	case parent.Err() != nil:
		return store.StatusFailed, store.InterruptedError
	case errors.Is(context.Cause(job), errCanceledByUser):
		return store.StatusCanceled, "canceled by user"
	case errors.Is(job.Err(), context.DeadlineExceeded):
		return store.StatusFailed, fmt.Sprintf("timed out after %s", e.opts.JobTimeout)
	default:
		return store.StatusFailed, masker.Mask(err.Error())
	}
}

// removeRelease deletes the checkout, the state and the images of a release.
func (e *Engine) removeRelease(ctx context.Context, slug string, id int64) {
	if meta, err := e.readMeta(slug, id); err == nil && len(meta.Images) > 0 {
		args := append([]string{"image", "rm"}, meta.Images...)
		// Best effort: an image may never have been built.
		if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args}); err != nil {
			e.opts.Logger.Debug("removing release images failed", "app", slug, "release", id, "error", err)
		}
	}
	_ = os.RemoveAll(e.ReleaseDir(slug, id))
	_ = os.RemoveAll(e.stateDir(slug, id))
}

// cleanupAfterSuccess keeps the newest successful releases and drops the
// rest, then prunes dangling images.
func (e *Engine) cleanupAfterSuccess(ctx context.Context, app store.App, log *deployLog) {
	// Rollbacks have no files of their own and may point at the same
	// release, so the newest KeepReleases distinct releases are kept, plus
	// whatever runs now (ARCHITECTURE 7.1).
	succeeded, err := e.opts.Store.SucceededDeployments(ctx, app.ID, 200)
	if err != nil {
		e.opts.Logger.Warn("listing releases to keep failed", "app", app.Slug, "error", err)
		return
	}
	keep := map[string]bool{}
	for _, d := range succeeded {
		if len(keep) >= e.opts.KeepReleases {
			break
		}
		keep[strconv.FormatInt(RollbackTarget(d), 10)] = true
	}
	if current, err := e.opts.Store.GetAppByID(ctx, app.ID); err == nil && current.CurrentDeploymentID != nil {
		keep[strconv.FormatInt(*current.CurrentDeploymentID, 10)] = true
	}
	stale := map[int64]bool{}
	for _, parent := range []string{"releases", "release-state"} {
		entries, err := os.ReadDir(filepath.Join(e.appDir(app.Slug), parent))
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if id, err := strconv.ParseInt(entry.Name(), 10, 64); err == nil && !keep[entry.Name()] {
				stale[id] = true
			}
		}
	}
	for id := range stale {
		e.removeRelease(ctx, app.Slug, id)
	}
	if e.opts.PruneImages {
		log.Step(StepCleanup, "Pruning dangling images")
		pruneCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := e.opts.Runner.Run(pruneCtx, runner.Cmd{
			Name: "docker", Args: []string{"image", "prune", "-f"},
			Stdout: log.Writer(), Stderr: log.Writer(),
		}); err != nil {
			log.Printf("image prune failed (ignored): %v", err)
		}
	}
}
