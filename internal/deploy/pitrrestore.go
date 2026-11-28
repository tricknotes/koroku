package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/pitr"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// This file brings the PostgreSQL database of an app back to a point in
// time (ARCHITECTURE 10.9): the app is stopped, the data directory is
// moved aside, a base backup is unpacked in its place with the WAL it
// needs, and PostgreSQL recovers up to the time and starts a new timeline.
// Every step that touches the files runs in a one-off container of the
// database's own image with the release's mounts.

// PITRSnapshots reads the snapshots of point-in-time recovery.
type PITRSnapshots interface {
	WriteBase(ctx context.Context, app store.App, snapshotID string, w io.Writer) error
	WriteWAL(ctx context.Context, app store.App, snapshotID string, w io.Writer) error
}

// PITRHooks is what a restore tells the archiving.
type PITRHooks interface {
	// RequestBase asks for a base backup of the new timeline.
	RequestBase(appID int64)
}

// Refusals of a restore to a point in time.
var (
	ErrPITROff      = errors.New("point-in-time recovery is not set up for this app")
	ErrPITRNotReady = errors.New("point-in-time recovery of this app is not ready")
)

// Step keys of a point-in-time restore log, besides check, stop, start,
// done and failed.
const (
	StepMove    = "move"
	StepBase    = "base"
	StepWAL     = "wal"
	StepRecover = "recover"
)

// trashMount is where the data before the restore is reachable in the
// one-off containers.
const trashMount = "/koroku/trash"

// PITRRestoreLogPath is the log of one restore.
func (e *Engine) PITRRestoreLogPath(slug string, id int64) string {
	return filepath.Join(e.appDir(slug), "pitr-restores", strconv.FormatInt(id, 10)+".log")
}

// StartPITRRestore checks the request, takes the app's operation lock and
// starts the restore in the background.
func (e *Engine) StartPITRRestore(ctx context.Context, app store.App, target time.Time) (store.PITRRestore, error) {
	if app.PITRService == "" {
		return store.PITRRestore{}, ErrPITROff
	}
	if e.opts.PITRSnapshots == nil {
		return store.PITRRestore{}, ErrBackupsNotConfigured
	}
	if app.CurrentDeploymentID == nil {
		return store.PITRRestore{}, ErrNotDeployed
	}
	st, err := e.opts.Store.GetPITRState(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) || err == nil && (st.Status != store.PITRReady || st.Service != app.PITRService) {
		return store.PITRRestore{}, ErrPITRNotReady
	}
	if err != nil {
		return store.PITRRestore{}, err
	}
	now := e.opts.Clock.Now()
	since := now.Add(-9 * 24 * time.Hour)
	bases, err := e.opts.Store.ListPITRBases(ctx, app.ID, since)
	if err != nil {
		return store.PITRRestore{}, err
	}
	batches, err := e.opts.Store.ListPITRWALBatches(ctx, app.ID, since)
	if err != nil {
		return store.PITRRestore{}, err
	}
	gaps, err := e.opts.Store.ListPITRGaps(ctx, app.ID, since)
	if err != nil {
		return store.PITRRestore{}, err
	}
	plan, err := pitr.PlanRestore(target, now, st.ServerVersionNum, bases, batches, gaps)
	if err != nil {
		return store.PITRRestore{}, err
	}

	release, err := e.beginOp(ctx, app)
	if err != nil {
		return store.PITRRestore{}, err
	}
	record, err := e.opts.Store.StartPITRRestore(ctx, app, target, now)
	if err != nil {
		release()
		return store.PITRRestore{}, err
	}
	trash := filepath.Join(e.opts.TrashDir, fmt.Sprintf("%s-pitr-%s", app.Slug, now.UTC().Format("20060102T150405Z")))
	if err := e.opts.Store.SetPITRRestoreDetails(ctx, record.ID, plan.Base.SnapshotID, trash); err != nil {
		release()
		return store.PITRRestore{}, err
	}
	masker := secrets.NewMasker()
	log, err := openDeployLog(e.PITRRestoreLogPath(app.Slug, record.ID), masker, e.opts.Clock.Now)
	if err != nil {
		release()
		e.finishPITRRestore(ctx, record.ID, err, masker)
		return store.PITRRestore{}, err
	}
	job := &pitrJob{e: e, id: record.ID, app: app, target: target, plan: plan, dataDir: st.DataDirectory, trash: trash, log: log, masker: masker}
	go func() {
		defer release()
		defer log.Close()
		jobCtx, cancel := context.WithTimeout(e.baseContext(), e.opts.CopyTimeout)
		defer cancel()
		err := job.run(jobCtx)
		e.finishPITRRestore(jobCtx, record.ID, err, masker)
	}()
	return e.opts.Store.GetPITRRestore(ctx, record.ID)
}

func (e *Engine) finishPITRRestore(ctx context.Context, id int64, runErr error, masker *secrets.Masker) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.finishTimeout)
	defer cancel()
	msg := ""
	if runErr != nil {
		msg = masker.Mask(runErr.Error())
		if len(msg) > 1000 {
			msg = strings.ToValidUTF8(msg[:1000], "") + "..."
		}
	}
	if err := e.opts.Store.FinishPITRRestore(ctx, id, runErr == nil, msg, e.opts.Clock.Now()); err != nil {
		e.opts.Logger.Error("recording the end of a point-in-time restore failed", "restore", id, "error", err)
	}
}

// failInterruptedPITRRestores records restores a previous process left
// running as failed. The app may be stopped and its data aside.
func (e *Engine) failInterruptedPITRRestores(ctx context.Context, now time.Time) {
	list, err := e.opts.Store.FailInterruptedPITRRestores(ctx, now)
	if err != nil {
		e.opts.Logger.Error("recovering interrupted point-in-time restores failed", "error", err)
		return
	}
	for _, r := range list {
		e.opts.Logger.Warn("point-in-time restore was interrupted by a restart", "restore", r.ID)
		if app, err := e.opts.Store.GetAppByID(ctx, r.AppID); err == nil {
			appendLogLine(e.PITRRestoreLogPath(app.Slug, r.ID), "Failed: "+store.PITRRestoreInterruptedError+
				". The app may be stopped; the data before the restore is in "+r.TrashPath+
				" (see docs/OPERATIONS.md to put it back), or start the app from the console.")
		}
	}
}

// pitrJob is one restore in progress.
type pitrJob struct {
	e       *Engine
	id      int64
	app     store.App
	target  time.Time
	plan    pitr.RestorePlan
	dataDir string
	trash   string
	log     *deployLog
	masker  *secrets.Masker

	wasRunning bool
	// moved is set once the data directory has been moved aside: a
	// failure then puts it back.
	moved bool
}

func (j *pitrJob) run(ctx context.Context) (err error) {
	e, app := j.e, j.app
	service := app.PITRService
	if err := e.maskEnv(ctx, app, j.masker); err != nil {
		return err
	}
	j.wasRunning = app.DesiredState == store.DesiredRunning
	defer func() {
		if err == nil {
			return
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("the restore took longer than %s: %w", e.opts.CopyTimeout, err)
		}
		j.log.Step(StepFailed, "Restore failed: %s", j.masker.Mask(err.Error()))
		if j.moved {
			if perr := j.putBack(); perr != nil {
				err = fmt.Errorf("%w; putting the data back failed: %w", err, perr)
				j.log.Printf("Putting the data back failed: %s", j.masker.Mask(perr.Error()))
				j.log.Printf("The data before the restore is in %s (see docs/OPERATIONS.md).", j.trash)
				return
			}
			j.log.Printf("Put the data before the restore back.")
		}
		if j.wasRunning {
			if serr := j.startApp(); serr != nil {
				j.log.Printf("Starting the app failed: %s", j.masker.Mask(serr.Error()))
			}
		}
	}()

	j.log.Step(StepCheck, "Checking %s", app.Slug)
	j.log.Printf("Target: %s", j.target.UTC().Format(time.RFC3339))
	j.log.Printf("Base backup: snapshot %s, taken %s", shortSnapshotID(j.plan.Base.SnapshotID), j.plan.Base.FinishedAt.UTC().Format(time.RFC3339))
	j.log.Printf("WAL in the backups: %d batches; the WAL not shipped yet is read from the host", len(j.plan.Batches))

	j.log.Step(StepStop, "Stopping %s", app.Slug)
	if j.wasRunning {
		if err := e.opts.Store.SetAppDesiredState(ctx, app.ID, store.DesiredStopped, e.opts.Clock.Now()); err != nil {
			return err
		}
		e.syncRoutes(ctx, "point-in-time restore")
	}
	stopCtx, cancel := context.WithTimeout(ctx, opTimeout)
	err = e.composeToLog(stopCtx, j.log, "", e.projectOnly(app).Args("stop")...)
	cancel()
	if err != nil {
		return err
	}

	j.log.Step(StepMove, "Moving the data directory aside to %s", j.trash)
	if err := os.MkdirAll(j.trash, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", j.trash, err)
	}
	j.moved = true
	if err := j.oneOff(ctx, nil, `set -e; cp -a "$1/." `+trashMount+`/; find "$1" -mindepth 1 -delete`, j.dataDir); err != nil {
		return fmt.Errorf("move the data directory aside: %w", err)
	}

	j.log.Step(StepBase, "Unpacking the base backup %s", shortSnapshotID(j.plan.Base.SnapshotID))
	if err := j.stream(ctx, func(ctx context.Context, w io.Writer) error {
		return e.opts.PITRSnapshots.WriteBase(ctx, app, j.plan.Base.SnapshotID, w)
	}, `set -e; tar -x -C "$1"; chown -R postgres:postgres "$1"; chmod 700 "$1"`, j.dataDir); err != nil {
		return fmt.Errorf("unpack the base backup: %w", err)
	}

	j.log.Step(StepWAL, "Gathering the WAL")
	if err := j.oneOff(ctx, nil, `set -e; rm -rf "$1/restore"; mkdir "$1/restore"`, compose.WALMountTarget); err != nil {
		return err
	}
	for _, batch := range j.plan.Batches {
		if err := j.stream(ctx, func(ctx context.Context, w io.Writer) error {
			return e.opts.PITRSnapshots.WriteWAL(ctx, app, batch.SnapshotID, w)
		}, `tar -x -C "$1/restore"`, compose.WALMountTarget); err != nil {
			return fmt.Errorf("read WAL snapshot %s: %w", shortSnapshotID(batch.SnapshotID), err)
		}
	}
	// The WAL not shipped yet, and what the old data directory had not
	// archived, reach to the minutes before the restore.
	if err := j.oneOff(ctx, nil, `set -e; cd "$1"
for f in [0-9A-F]*; do [ -f "$f" ] && case "$f" in *.tmp) ;; *) cp -n "$f" restore/ ;; esac; done
for f in `+trashMount+`/pg_wal/[0-9A-F]*; do [ -f "$f" ] && cp -n "$f" restore/; done
chown -R postgres:postgres restore; ls restore | wc -l`, compose.WALMountTarget); err != nil {
		return fmt.Errorf("gather the WAL kept on the host: %w", err)
	}

	j.log.Step(StepRecover, "Recovering to %s", j.target.UTC().Format(time.RFC3339))
	settings := []string{
		"restore_command = 'cp " + compose.WALMountTarget + "/restore/%f %p'",
		"recovery_target_time = '" + j.target.UTC().Format("2006-01-02 15:04:05.999999") + "+00'",
		"recovery_target_action = 'promote'",
		"recovery_target_timeline = 'current'",
	}
	if err := j.oneOff(ctx, nil, `set -e; d="$1"; shift; touch "$d/recovery.signal"; printf '%s\n' "$@" >> "$d/postgresql.auto.conf"; chown postgres:postgres "$d/recovery.signal" "$d/postgresql.auto.conf"`,
		append([]string{j.dataDir}, settings...)...); err != nil {
		return fmt.Errorf("write the recovery settings: %w", err)
	}
	if err := j.recover(ctx); err != nil {
		return err
	}

	j.log.Step(StepCleanup, "Removing the recovery settings")
	for _, name := range []string{"restore_command", "recovery_target_time", "recovery_target_action", "recovery_target_timeline"} {
		if err := j.sql(ctx, "ALTER SYSTEM RESET "+name); err != nil {
			return err
		}
	}
	if err := j.sql(ctx, "SELECT pg_reload_conf()"); err != nil {
		return err
	}
	if err := j.exec(ctx, "sh", "-c", `rm -rf "$1/restore"`, "sh", compose.WALMountTarget); err != nil {
		return err
	}
	// From here on the data is the restored one: a failure does not put the
	// old data back.
	j.moved = false

	if j.wasRunning {
		j.log.Step(StepStart, "Starting %s", app.Slug)
		if err := j.startApp(); err != nil {
			return err
		}
	} else {
		j.log.Step(StepStart, "Stopping %s again: it was stopped before the restore", service)
		stopCtx, cancel := context.WithTimeout(ctx, opTimeout)
		_ = e.composeToLog(stopCtx, j.log, "", e.projectOnly(app).Args("stop")...)
		cancel()
	}
	if e.opts.PITR != nil {
		e.opts.PITR.RequestBase(app.ID)
		j.log.Printf("A base backup of the new timeline is taken next.")
	}
	j.log.Printf("The data before the restore stays in %s for 7 days.", j.trash)
	j.log.Step(StepDone, "Restored %s to %s", service, j.target.UTC().Format(time.RFC3339))
	return nil
}

// oneOff runs a shell script in a one-off container of the database's
// service, with the release's mounts and the trash directory. Its first
// arguments are $1, $2, ...
func (j *pitrJob) oneOff(ctx context.Context, stdin io.Reader, script string, args ...string) error {
	inv, dir, ok := j.e.currentInvocation(j.app)
	if !ok {
		return errors.New("the files of the running release are gone; deploy the app again")
	}
	runArgs := inv.Args(append([]string{"run", "--rm", "--no-deps", "-T", "--user", "root",
		"--volume", j.trash + ":" + trashMount, "--entrypoint", "sh", j.app.PITRService, "-c", script, "sh"}, args...)...)
	ctx, cancel := context.WithTimeout(ctx, j.e.opts.CopyTimeout)
	defer cancel()
	return j.e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: runArgs, Env: inv.Env(), CleanEnv: true, Dir: dir,
		Stdin: stdin, Stdout: j.log.Writer(), Stderr: j.log.Writer()})
}

// stream pipes what write writes into a one-off container's script.
func (j *pitrJob) stream(ctx context.Context, write func(context.Context, io.Writer) error, script string, args ...string) error {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := write(ctx, pw)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	runErr := j.oneOff(ctx, pr, script, args...)
	_ = pr.Close()
	writeErr := <-done
	if writeErr != nil {
		return writeErr
	}
	return runErr
}

// exec runs a command in the running database's container.
func (j *pitrJob) exec(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return j.e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker",
		Args:   append([]string{"compose", "-p", j.app.ProjectName(), "exec", "-T", j.app.PITRService}, args...),
		Stdout: io.Discard, Stderr: j.log.Writer()})
}

func (j *pitrJob) sql(ctx context.Context, query string) error {
	return j.exec(ctx, pitr.PsqlCommand(query)...)
}

func (j *pitrJob) query(ctx context.Context, query string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := runner.Output(ctx, j.e.opts.Runner, runner.Cmd{Name: "docker",
		Args: append([]string{"compose", "-p", j.app.ProjectName(), "exec", "-T", j.app.PITRService}, pitr.PsqlCommand(query)...)})
	return strings.TrimSpace(string(out)), err
}

// recover starts the database alone and waits until it has left recovery.
// PostgreSQL stops with an error when the WAL ends short of the target.
func (j *pitrJob) recover(ctx context.Context) error {
	e, app := j.e, j.app
	inv, dir, ok := e.currentInvocation(app)
	if !ok {
		return errors.New("the files of the running release are gone; deploy the app again")
	}
	upCtx, cancel := context.WithTimeout(ctx, e.upTimeout())
	err := e.composeToLog(upCtx, j.log, dir, inv.Args("up", "-d", "--no-deps", app.PITRService)...)
	cancel()
	if err != nil {
		return err
	}
	for {
		if out, err := j.query(ctx, "SELECT pg_is_in_recovery()"); err == nil && out == "f" {
			j.log.Printf("The database left recovery and started a new timeline.")
			return nil
		}
		state, _ := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker",
			Args: e.projectOnly(app).Args("ps", "--all", "--format", "{{.State}}", app.PITRService)})
		if s := strings.TrimSpace(string(state)); s == "exited" || s == "dead" {
			logs, _ := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker",
				Args: e.projectOnly(app).Args("logs", "--no-log-prefix", "--tail", "5", app.PITRService)})
			return fmt.Errorf("PostgreSQL stopped during the recovery: %s", lastOutputLine(string(logs)))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the recovery did not finish: %w", ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// putBack replaces the data directory with what was moved aside. It runs
// even when the restore's context is done.
func (j *pitrJob) putBack() error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(j.e.baseContext()), j.e.opts.CopyTimeout)
	defer cancel()
	stopCtx, cancelStop := context.WithTimeout(ctx, opTimeout)
	_ = j.e.composeToLog(stopCtx, j.log, "", j.e.projectOnly(j.app).Args("stop", j.app.PITRService)...)
	cancelStop()
	return j.oneOff(ctx, nil, `set -e; find "$1" -mindepth 1 -delete; cp -a `+trashMount+`/. "$1"/`, j.dataDir)
}

// startApp brings the whole release up and marks the app running.
func (j *pitrJob) startApp() error {
	e, app := j.e, j.app
	ctx, cancel := context.WithTimeout(context.WithoutCancel(e.baseContext()), e.upTimeout())
	defer cancel()
	if err := e.upCurrent(ctx, app); err != nil {
		return err
	}
	if err := e.opts.Store.SetAppDesiredState(ctx, app.ID, store.DesiredRunning, e.opts.Clock.Now()); err != nil {
		return err
	}
	e.syncRoutes(ctx, "point-in-time restore finished")
	return nil
}
