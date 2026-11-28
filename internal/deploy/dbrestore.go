package deploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// This file restores an app's own dump snapshot into its database
// (ARCHITECTURE 10.8). It is a database copy whose source and target are the
// app: the current data is dumped first, the other services are stopped,
// and the chosen dump is streamed into the load hook. A load that fails is
// followed by loading the dump taken first, so that the app is not served
// on half-loaded data.

// StartRestore checks the request, takes the app's operation lock and starts
// the restore in the background. It returns the record of the running
// restore.
func (e *Engine) StartRestore(ctx context.Context, app store.App, snapshotID string) (store.DBCopy, error) {
	switch {
	case app.LoadHookService == "" || app.LoadHookCommand == "":
		return store.DBCopy{}, ErrNoLoadHook
	case app.DumpHookService == "" || app.DumpHookCommand == "":
		return store.DBCopy{}, ErrRestoreNeedsDumpHook
	case app.CurrentDeploymentID == nil:
		return store.DBCopy{}, ErrNotDeployed
	case app.DesiredState != store.DesiredRunning:
		return store.DBCopy{}, ErrTargetNotRunning
	case e.opts.CopyBackups == nil:
		return store.DBCopy{}, ErrBackupsNotConfigured
	}
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return store.DBCopy{}, err
	}
	lookupCtx, cancel := context.WithTimeout(ctx, time.Minute)
	snapshot, err := e.opts.CopyBackups.DumpByID(lookupCtx, app, snapshotID)
	cancel()
	if err != nil {
		release()
		return store.DBCopy{}, err
	}
	if snapshot.Target != app.DumpHookService {
		release()
		return store.DBCopy{}, fmt.Errorf("%w: it was taken in %s, the dump hook runs in %s", ErrDumpOfOtherService, snapshot.Target, app.DumpHookService)
	}

	record, err := e.opts.Store.StartDBRestore(ctx, app, snapshot.ID, e.opts.Clock.Now())
	if err != nil {
		release()
		return store.DBCopy{}, err
	}
	masker := secrets.NewMasker()
	log, err := openDeployLog(e.CopyLogPath(app.Slug, record.ID), masker, e.opts.Clock.Now)
	if err != nil {
		release()
		e.finishCopy(ctx, record.ID, err, 0, masker)
		return store.DBCopy{}, err
	}
	req := CopyRequest{Source: app, Target: app, Kind: store.CopySourceSnapshot, Restore: true, SnapshotID: snapshot.ID}
	job := &copyJob{e: e, id: record.ID, req: req, snapshot: snapshot, log: log, masker: masker}
	go func() {
		defer release()
		defer log.Close()
		jobCtx, cancel := context.WithTimeout(e.baseContext(), e.opts.CopyTimeout)
		defer cancel()
		bytes, err := job.restore(jobCtx)
		e.finishCopy(jobCtx, record.ID, err, bytes, masker)
	}()
	return record, nil
}

func (j *copyJob) restore(ctx context.Context) (bytes int64, err error) {
	e, app := j.e, j.req.Target
	if err := e.maskEnv(ctx, app, j.masker); err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("the restore took longer than %s: %w", e.opts.CopyTimeout, err)
			}
			j.log.Step(StepFailed, "Restore failed: %s", j.masker.Mask(err.Error()))
		}
		if len(j.stopped) == 0 || (err != nil && j.keepStopped) {
			return
		}
		if startErr := j.startStopped(ctx); startErr != nil {
			err = fmt.Errorf("%w; %w", err, startErr)
		}
	}()

	j.log.Step(StepCheck, "Checking %s", app.Slug)
	j.log.Printf("Dump to restore: snapshot %s, taken %s in service %s", shortSnapshotID(j.snapshot.ID), j.snapshot.Time.UTC().Format(time.RFC3339), j.snapshot.Target)
	j.log.Printf("Load hook in service %s: %s", app.LoadHookService, app.LoadHookCommand)
	if _, err := e.wakeLocked(ctx, app.ID, j.log.Printf); err != nil {
		return 0, err
	}
	running, err := e.runningServices(ctx, app)
	if err != nil {
		return 0, err
	}
	if !slices.Contains(running, app.LiveService(app.LoadHookService)) {
		return 0, fmt.Errorf("the load hook service %s is not running", app.LoadHookService)
	}

	// The dump of the current data is what a failed load falls back to,
	// and what undoes a restore of the wrong point: without it, nothing is
	// changed.
	j.log.Step(StepBackup, "Backing up the current data of %s", app.Slug)
	backupCtx, cancel := context.WithTimeout(ctx, time.Hour)
	backupID, err := e.opts.CopyBackups.BackupDump(backupCtx, app)
	cancel()
	if err != nil {
		return 0, fmt.Errorf("backup before the restore: %w", err)
	}
	if err := e.opts.Store.SetDBCopyBackup(ctx, j.id, backupID); err != nil {
		return 0, err
	}
	j.log.Printf("Stored the current data as snapshot %s.", shortSnapshotID(backupID))

	j.log.Step(StepStop, "Stopping the other services of %s", app.Slug)
	if err := j.stopOthers(ctx, running); err != nil {
		return 0, err
	}

	j.log.Step(StepLoad, "Loading the dump of %s", j.snapshot.Time.UTC().Format(time.RFC3339))
	j.loadBegan = true
	bytes, err = j.pipe(ctx)
	if err != nil {
		j.reload(ctx, backupID, err)
		return bytes, err
	}
	j.log.Printf("Loaded %s.", formatBytes(bytes))

	if len(j.stopped) > 0 {
		j.log.Step(StepStart, "Starting the stopped services")
		if err := j.startStopped(ctx); err != nil {
			return bytes, err
		}
	}
	j.log.Step(StepDone, "Restored the dump of %s", j.snapshot.Time.UTC().Format(time.RFC3339))
	return bytes, nil
}

// reload loads the dump taken before the restore after the chosen one
// failed to load. When that fails too, the database may hold part of a
// dump, and the stopped services stay down.
func (j *copyJob) reload(ctx context.Context, backupID string, loadErr error) {
	e, app := j.e, j.req.Target
	j.log.Printf("Loading the dump failed: %s", j.masker.Mask(loadErr.Error()))
	j.log.Step(StepReload, "Loading the data of %s back from snapshot %s", app.Slug, shortSnapshotID(backupID))
	// The restore may have run out of time: the reload gets its own.
	reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.opts.CopyTimeout)
	defer cancel()
	snapshot, err := e.opts.CopyBackups.DumpByID(reloadCtx, app, backupID)
	if err == nil {
		j.snapshot = snapshot
		_, err = j.pipe(reloadCtx)
	}
	if err != nil {
		j.keepStopped = true
		j.log.Printf("Loading the data back failed: %s", j.masker.Mask(err.Error()))
		if len(j.stopped) > 0 {
			j.log.Printf("The stopped services stay stopped, because the database may hold part of a dump: %s.", strings.Join(j.stopped, ", "))
		}
		j.log.Printf("The data before the restore is snapshot %s; restore it from the backups tab, or start the app from the console.", shortSnapshotID(backupID))
		return
	}
	j.log.Printf("Loaded the data before the restore back.")
}

// stopOthers stops the running services other than the load hook's.
func (j *copyJob) stopOthers(ctx context.Context, running []string) error {
	dst := j.req.Target
	var others []string
	for _, name := range running {
		if name != dst.LiveService(dst.LoadHookService) {
			others = append(others, name)
		}
	}
	if len(others) == 0 {
		j.log.Printf("No other service is running.")
		return nil
	}
	stopCtx, cancel := context.WithTimeout(ctx, opTimeout)
	err := j.e.composeToLog(stopCtx, j.log, "", append([]string{"compose", "-p", dst.ProjectName(), "stop"}, others...)...)
	cancel()
	// Some may have stopped before the command failed: start all of them
	// again in any case.
	j.stopped = others
	if err != nil {
		return err
	}
	j.log.Printf("Stopped: %s", strings.Join(others, ", "))
	return nil
}
