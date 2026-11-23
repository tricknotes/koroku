package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// This file copies the database of one app into a staging app
// (ARCHITECTURE 10.6): the source's dump hook (or its newest dump snapshot)
// is streamed into the target's load hook through a pipe, while the
// target's other services are stopped.

// CopyBackups is what a database copy needs from the backup service. It is
// optional; without it the target is not backed up first and snapshot
// sources are refused.
type CopyBackups interface {
	// BackupDump stores the output of the app's dump hook and returns the
	// snapshot ID.
	BackupDump(ctx context.Context, app store.App) (snapshotID string, err error)
	// LatestDump finds the app's newest dump snapshot. The error matches
	// ErrNoDumpSnapshot when there is none.
	LatestDump(ctx context.Context, app store.App) (DumpSnapshot, error)
}

// DumpSnapshot is a stored dump that can be streamed.
type DumpSnapshot struct {
	// ID and Time describe the snapshot for the log.
	ID   string
	Time time.Time
	// Write streams the dump to w.
	Write func(ctx context.Context, w io.Writer) error
}

var (
	// ErrTargetNotStaging is returned when the target of a copy is not a
	// staging app.
	ErrTargetNotStaging = errors.New("data can only be copied into a staging app")
	// ErrSameApp is returned when the source and the target are one app.
	ErrSameApp = errors.New("the source and the target are the same app")
	// ErrNoLoadHook is returned when the target has no load hook.
	ErrNoLoadHook = errors.New("the target app has no load hook")
	// ErrNoDumpHook is returned when a live copy's source has no dump hook.
	ErrNoDumpHook = errors.New("the source app has no dump hook")
	// ErrSourceNotRunning is returned when a live copy's source is not
	// running.
	ErrSourceNotRunning = errors.New("the source app is not running")
	// ErrTargetNotRunning is returned when the target is stopped.
	ErrTargetNotRunning = errors.New("the target app is not running")
	// ErrNoDumpSnapshot is returned when a snapshot copy finds no dump.
	ErrNoDumpSnapshot = errors.New("the source app has no dump snapshot")
	// ErrBackupsNotConfigured is returned for a snapshot copy on a host
	// without backups.
	ErrBackupsNotConfigured = errors.New("backups are not configured")
)

// Step keys of a copy log, in addition to the deploy steps it shares
// (check, backup, start, done, failed).
const (
	StepStop  = "stop"
	StepCopy  = "copy"
	StepAfter = "after"
)

// Defaults of the copy limits.
const (
	defaultCopyTimeout  = 2 * time.Hour
	defaultCopyMaxBytes = int64(20) << 30
)

// CopyRequest asks for one copy.
type CopyRequest struct {
	Source store.App
	Target store.App
	// Kind is store.CopySourceLive or store.CopySourceSnapshot.
	Kind string
	// SkipBackup leaves out the backup of the target: the first copy into
	// a review app that was just created has nothing to keep
	// (ARCHITECTURE 9.9).
	SkipBackup bool
}

// CopyLogPath is the log of one database copy.
func (e *Engine) CopyLogPath(slug string, id int64) string {
	return filepath.Join(e.appDir(slug), "db-copies", strconv.FormatInt(id, 10)+".log")
}

// StartCopy checks the request, takes the operation locks and starts the
// copy in the background. It returns the record of the running copy.
func (e *Engine) StartCopy(ctx context.Context, req CopyRequest) (store.DBCopy, error) {
	src, dst := req.Source, req.Target
	switch {
	case src.ID == dst.ID:
		return store.DBCopy{}, ErrSameApp
	case dst.Environment != store.EnvironmentStaging:
		return store.DBCopy{}, ErrTargetNotStaging
	case dst.LoadHookService == "" || dst.LoadHookCommand == "":
		return store.DBCopy{}, ErrNoLoadHook
	case dst.CurrentDeploymentID == nil:
		return store.DBCopy{}, ErrNotDeployed
	case dst.DesiredState != store.DesiredRunning:
		return store.DBCopy{}, ErrTargetNotRunning
	}
	switch req.Kind {
	case store.CopySourceLive:
		if src.DumpHookService == "" || src.DumpHookCommand == "" {
			return store.DBCopy{}, ErrNoDumpHook
		}
		if src.CurrentDeploymentID == nil || src.DesiredState != store.DesiredRunning {
			return store.DBCopy{}, ErrSourceNotRunning
		}
	case store.CopySourceSnapshot:
		if e.opts.CopyBackups == nil {
			return store.DBCopy{}, ErrBackupsNotConfigured
		}
	default:
		return store.DBCopy{}, fmt.Errorf("unknown copy source kind %q", req.Kind)
	}

	release, err := e.beginOp(ctx, dst)
	if err != nil {
		return store.DBCopy{}, err
	}
	releaseAll := release
	if req.Kind == store.CopySourceLive {
		// The dump runs in the source's containers: no deployment may
		// replace them meanwhile.
		releaseSource, err := e.beginOp(ctx, src)
		if err != nil {
			release()
			return store.DBCopy{}, err
		}
		releaseAll = func() { releaseSource(); release() }
	}

	var snapshot DumpSnapshot
	if req.Kind == store.CopySourceSnapshot {
		lookupCtx, cancel := context.WithTimeout(ctx, time.Minute)
		snapshot, err = e.opts.CopyBackups.LatestDump(lookupCtx, src)
		cancel()
		if err != nil {
			releaseAll()
			return store.DBCopy{}, err
		}
	}

	record, err := e.opts.Store.StartDBCopy(ctx, dst, src, req.Kind, e.opts.Clock.Now())
	if err != nil {
		releaseAll()
		return store.DBCopy{}, err
	}
	masker := secrets.NewMasker()
	log, err := openDeployLog(e.CopyLogPath(dst.Slug, record.ID), masker, e.opts.Clock.Now)
	if err != nil {
		releaseAll()
		e.finishCopy(ctx, record.ID, err, 0, masker)
		return store.DBCopy{}, err
	}

	job := &copyJob{e: e, id: record.ID, req: req, snapshot: snapshot, log: log, masker: masker}
	go func() {
		defer releaseAll()
		defer log.Close()
		jobCtx, cancel := context.WithTimeout(e.baseContext(), e.opts.CopyTimeout)
		defer cancel()
		bytes, err := job.run(jobCtx)
		e.finishCopy(jobCtx, record.ID, err, bytes, masker)
	}()
	return record, nil
}

// baseContext is the context background work runs under: that of Run, or
// the background before Run has started (tests).
func (e *Engine) baseContext() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.base == nil {
		return context.Background()
	}
	return e.base
}

// failInterruptedCopies records copies that a previous process left
// running as failed. The services they stopped may still be down.
func (e *Engine) failInterruptedCopies(ctx context.Context, now time.Time) {
	copies, err := e.opts.Store.FailInterruptedDBCopies(ctx, now)
	if err != nil {
		e.opts.Logger.Error("recovering interrupted database copies failed", "error", err)
		return
	}
	for _, c := range copies {
		e.opts.Logger.Warn("database copy was interrupted by a restart", "copy", c.ID)
		if app, err := e.opts.Store.GetAppByID(ctx, c.TargetAppID); err == nil {
			appendLogLine(e.CopyLogPath(app.Slug, c.ID), "Failed: "+store.CopyInterruptedError+
				". Services stopped for the copy may still be stopped; start the app from the console.")
		}
	}
}

// finishCopy records the end of a copy even when ctx was canceled.
func (e *Engine) finishCopy(ctx context.Context, id int64, runErr error, bytes int64, masker *secrets.Masker) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.finishTimeout)
	defer cancel()
	msg := ""
	if runErr != nil {
		msg = masker.Mask(runErr.Error())
		if len(msg) > 1000 {
			msg = strings.ToValidUTF8(msg[:1000], "") + "..."
		}
	}
	if err := e.opts.Store.FinishDBCopy(ctx, id, runErr == nil, msg, bytes, e.opts.Clock.Now()); err != nil {
		e.opts.Logger.Error("recording the end of a database copy failed", "copy", id, "error", err)
	}
}

// copyJob is one copy in progress.
type copyJob struct {
	e        *Engine
	id       int64
	req      CopyRequest
	snapshot DumpSnapshot
	log      *deployLog
	masker   *secrets.Masker

	// stopped lists the target's services stopped for the load.
	stopped []string
	// loadBegan is set once data may have reached the target.
	loadBegan bool
}

func (j *copyJob) run(ctx context.Context) (bytes int64, err error) {
	e, src, dst := j.e, j.req.Source, j.req.Target
	// Neither app's variables may show up in the log.
	for _, app := range []store.App{src, dst} {
		if err := e.maskEnv(ctx, app, j.masker); err != nil {
			return 0, err
		}
	}

	defer func() {
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("the copy took longer than %s: %w", e.opts.CopyTimeout, err)
			}
			j.log.Step(StepFailed, "Copy failed: %s", j.masker.Mask(err.Error()))
		}
		if len(j.stopped) == 0 {
			return
		}
		if err != nil && j.loadBegan && dst.PostLoadCommand != "" {
			// Half-loaded production data may still hold what the
			// post-load command was to remove: do not serve it.
			j.log.Printf("The services stopped for the copy stay stopped, because the command after the copy did not run to the end: %s.", strings.Join(j.stopped, ", "))
			j.log.Printf("Run the copy again, or start the app from the console.")
			return
		}
		if startErr := j.startStopped(ctx); startErr != nil {
			err = fmt.Errorf("%w; %w", err, startErr)
		}
	}()

	j.log.Step(StepCheck, "Checking %s and %s", src.Slug, dst.Slug)
	switch j.req.Kind {
	case store.CopySourceLive:
		j.log.Printf("Source: %s, live dump in service %s: %s", src.Slug, src.DumpHookService, src.DumpHookCommand)
	default:
		j.log.Printf("Source: snapshot %s of %s, taken %s", shortSnapshotID(j.snapshot.ID), src.Slug, j.snapshot.Time.UTC().Format(time.RFC3339))
	}
	j.log.Printf("Target: %s, load hook in service %s: %s", dst.Slug, dst.LoadHookService, dst.LoadHookCommand)
	// The copy holds the locks of the apps it runs in; a sleeping one is
	// woken under them (ARCHITECTURE 8.4).
	awake := []store.App{dst}
	if j.req.Kind == store.CopySourceLive {
		awake = append(awake, src)
	}
	for _, app := range awake {
		if _, err := e.wakeLocked(ctx, app.ID, func(format string, args ...any) {
			j.log.Printf("%s: "+format, append([]any{app.Slug}, args...)...)
		}); err != nil {
			return 0, err
		}
	}
	running, err := e.runningServices(ctx, dst)
	if err != nil {
		return 0, err
	}
	if !slices.Contains(running, dst.LiveService(dst.LoadHookService)) {
		return 0, fmt.Errorf("the load hook service %s of %s is not running", dst.LoadHookService, dst.Slug)
	}

	j.log.Step(StepBackup, "Backing up the data of %s", dst.Slug)
	switch {
	case j.req.SkipBackup:
		j.log.Printf("%s was just created; there is no data to back up first.", dst.Slug)
	case e.opts.CopyBackups == nil:
		j.log.Printf("Backups are not configured; the current data of %s is not backed up first.", dst.Slug)
	case dst.DumpHookService == "" || dst.DumpHookCommand == "":
		j.log.Printf("%s has no dump hook; its current data is not backed up first.", dst.Slug)
	default:
		backupCtx, cancel := context.WithTimeout(ctx, time.Hour)
		id, err := e.opts.CopyBackups.BackupDump(backupCtx, dst)
		cancel()
		if err != nil {
			return 0, fmt.Errorf("backup before the copy: %w", err)
		}
		if err := e.opts.Store.SetDBCopyBackup(ctx, j.id, id); err != nil {
			return 0, err
		}
		j.log.Printf("Stored the dump of %s as snapshot %s.", dst.Slug, shortSnapshotID(id))
	}

	j.log.Step(StepStop, "Stopping the other services of %s", dst.Slug)
	var others []string
	for _, name := range running {
		if name != dst.LiveService(dst.LoadHookService) {
			others = append(others, name)
		}
	}
	if len(others) == 0 {
		j.log.Printf("No other service is running.")
	} else {
		stopCtx, cancel := context.WithTimeout(ctx, opTimeout)
		err := e.composeToLog(stopCtx, j.log, "", append([]string{"compose", "-p", dst.ProjectName(), "stop"}, others...)...)
		cancel()
		// Some may have stopped before the command failed: start all of
		// them again in any case.
		j.stopped = others
		if err != nil {
			return 0, err
		}
		j.log.Printf("Stopped: %s", strings.Join(others, ", "))
	}

	j.log.Step(StepCopy, "Copying the data")
	j.loadBegan = true
	bytes, err = j.pipe(ctx)
	if err != nil {
		return bytes, err
	}
	j.log.Printf("Copied %s.", formatBytes(bytes))

	if dst.PostLoadCommand != "" {
		j.log.Step(StepAfter, "Running the command after the copy in %s", dst.PostLoadService)
		if err := j.afterLoad(ctx); err != nil {
			return bytes, err
		}
	}

	if len(j.stopped) > 0 {
		j.log.Step(StepStart, "Starting the stopped services")
		err := j.startStopped(ctx)
		if err != nil {
			return bytes, err
		}
	}
	j.log.Step(StepDone, "Copied the data of %s into %s", src.Slug, dst.Slug)
	return bytes, nil
}

// startStopped starts the services stopped for the copy. It also runs when
// ctx is already canceled: the target must not stay down because the copy
// failed or timed out.
func (j *copyJob) startStopped(ctx context.Context) error {
	if len(j.stopped) == 0 {
		return nil
	}
	services := j.stopped
	j.stopped = nil
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	err := j.e.composeToLog(ctx, j.log, "", append([]string{"compose", "-p", j.req.Target.ProjectName(), "start"}, services...)...)
	if err != nil {
		j.log.Printf("Starting %s failed; start the app from the console.", strings.Join(services, ", "))
		return fmt.Errorf("start the stopped services: %w", err)
	}
	j.log.Printf("Started: %s", strings.Join(services, ", "))
	return nil
}

// pipe streams the dump into the load hook. The two commands are joined by
// an operating system pipe whose read end is the load hook's standard
// input. This process writes the dump into it, counting the bytes, and
// keeps the write end open until it knows how the dump ended:
//
//   - the dump succeeds: the write end is closed and the load hook reads
//     the end of its input;
//   - the dump fails, prints nothing or exceeds the size limit: the load
//     hook is killed while it still waits for input, so it never takes a
//     truncated dump for a complete one;
//   - the load hook ends first: the dump is stopped and the copy fails.
func (j *copyJob) pipe(ctx context.Context) (int64, error) {
	e, dst := j.e, j.req.Target
	pr, pw, err := os.Pipe()
	if err != nil {
		return 0, fmt.Errorf("create pipe: %w", err)
	}
	defer pr.Close()
	defer pw.Close()

	dumpCtx, stopDump := context.WithCancel(ctx)
	defer stopDump()
	loadCtx, stopLoad := context.WithCancel(ctx)
	defer stopLoad()

	counter := &limitWriter{dst: pw, limit: e.opts.CopyMaxBytes, exceeded: stopDump}
	dumpOut := j.prefixed("dump")
	dumpDone := make(chan error, 1)
	go func() {
		var err error
		if j.req.Kind == store.CopySourceLive {
			src := j.req.Source
			err = e.opts.Runner.Run(dumpCtx, runner.Cmd{
				Name:   "docker",
				Args:   hookArgs(src, src.DumpHookService, src.DumpHookCommand),
				Stdout: counter,
				Stderr: dumpOut,
			})
		} else {
			err = j.snapshot.Write(dumpCtx, counter)
		}
		_ = dumpOut.Flush()
		dumpDone <- err
	}()

	loadOut := j.prefixed("load")
	loadDone := make(chan error, 1)
	go func() {
		err := e.opts.Runner.Run(loadCtx, runner.Cmd{
			Name:   "docker",
			Args:   hookArgs(dst, dst.LoadHookService, dst.LoadHookCommand),
			Stdin:  pr,
			Stdout: loadOut,
			Stderr: loadOut,
		})
		_ = loadOut.Flush()
		loadDone <- err
	}()

	var dumpErr, loadErr error
	select {
	case dumpErr = <-dumpDone:
		if dumpErr != nil || counter.n == 0 || counter.over {
			// Kill the load hook before it can see the end of its input.
			stopLoad()
			loadErr = <-loadDone
			switch {
			case counter.over:
				return counter.n, fmt.Errorf("the dump is larger than the limit of %s", formatBytes(e.opts.CopyMaxBytes))
			case dumpErr != nil:
				return counter.n, fmt.Errorf("the dump command failed: %w", dumpErr)
			default:
				return 0, errors.New("the dump command printed nothing")
			}
		}
		_ = pw.Close()
		loadErr = <-loadDone
	case loadErr = <-loadDone:
		// The load hook cannot have read the end of its input yet.
		stopDump()
		_ = pr.Close()
		dumpErr = <-dumpDone
		if loadErr == nil {
			loadErr = errors.New("it ended before the dump did")
		}
	}
	if loadErr != nil {
		return counter.n, fmt.Errorf("the load command failed: %w", loadErr)
	}
	return counter.n, nil
}

// afterLoad runs the post-load command. A service that is running (the
// load hook's) runs it with exec; a service stopped for the copy runs it in
// a one-off container of the current release, so that the app is not
// served before the command has finished.
func (j *copyJob) afterLoad(ctx context.Context) error {
	e, dst := j.e, j.req.Target
	out := j.prefixed("after")
	defer func() { _ = out.Flush() }()
	var args []string
	dir := ""
	if dst.PostLoadService == dst.LoadHookService {
		// The only service still running.
		args = hookArgs(dst, dst.PostLoadService, dst.PostLoadCommand)
	} else {
		inv, releaseDir, ok := e.currentInvocation(dst)
		if !ok {
			return errors.New("the files of the running release are gone; deploy the app again")
		}
		dir = releaseDir
		args = inv.Args("run", "--rm", "--no-deps", "-T", dst.PostLoadService, "sh", "-c", dst.PostLoadCommand)
	}
	err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args, CleanEnv: true, Dir: dir, Stdout: out, Stderr: out})
	if err != nil {
		return fmt.Errorf("the command after the copy failed: %w", err)
	}
	return nil
}

// prefixed returns a writer that puts command output into the log, one
// masked line at a time, each starting with "<name> | ".
func (j *copyJob) prefixed(name string) *secrets.LineWriter {
	return secrets.NewLineWriter(prefixWriter{dst: j.log.Writer(), prefix: name + " | "}, j.masker)
}

// prefixWriter receives whole lines from a LineWriter.
type prefixWriter struct {
	dst    io.Writer
	prefix string
}

func (p prefixWriter) Write(b []byte) (int, error) {
	if _, err := io.WriteString(p.dst, p.prefix+string(b)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// limitWriter counts what passes through and stops at the limit.
type limitWriter struct {
	dst      io.Writer
	limit    int64
	exceeded func()
	n        int64
	over     bool
}

var errCopyTooLarge = errors.New("the dump is larger than the limit")

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.over || w.n+int64(len(p)) > w.limit {
		w.over = true
		w.exceeded()
		return 0, errCopyTooLarge
	}
	n, err := w.dst.Write(p)
	w.n += int64(n)
	return n, err
}

// hookArgs runs a hook's command line with `sh -c` inside the service's
// container; no shell runs on the host. The public service of a blue/green
// app runs in its colour (ARCHITECTURE 7.2).
func hookArgs(app store.App, service, command string) []string {
	return []string{"compose", "-p", app.ProjectName(), "exec", "-T", app.LiveService(service), "sh", "-c", command}
}

// runningServices lists the services of the app that have a running
// container, sorted and without duplicates.
func (e *Engine) runningServices(ctx context.Context, app store.App) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{
		Name: "docker", Args: e.projectOnly(app).Args("ps", "--format", "json"),
	})
	if err != nil {
		return nil, fmt.Errorf("docker compose ps: %w", err)
	}
	services, err := parseComposePS(out)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, svc := range services {
		if svc.State == "running" && !slices.Contains(names, svc.Name) {
			names = append(names, svc.Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// composeToLog runs a docker command with its output in the log.
func (e *Engine) composeToLog(ctx context.Context, log *deployLog, dir string, args ...string) error {
	if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args, Dir: dir, Stdout: log.Writer(), Stderr: log.Writer()}); err != nil {
		return fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// maskEnv registers the values of the app's variables with the masker.
func (e *Engine) maskEnv(ctx context.Context, app store.App, masker *secrets.Masker) error {
	if e.opts.Box == nil {
		return nil
	}
	rows, err := e.opts.Store.ListEnvVars(ctx, app.ID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		plain, err := e.opts.Box.Open(row.ValueEncrypted, secrets.EnvVarContext(app.ID, row.Key))
		if err != nil {
			return fmt.Errorf("environment variable %s of %s: %w", row.Key, app.Slug, err)
		}
		masker.Add(string(plain))
	}
	return nil
}

func shortSnapshotID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// formatBytes renders a size for the log: "18.4 MB".
func formatBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
