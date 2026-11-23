package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/api"
	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// backupRepository builds the restic repository settings from the
// configuration: the same bucket as the log archives, under restic/.
func backupRepository(cfg config.Config) backup.Repository {
	return backup.Repository{
		Endpoint:        cfg.Storage.Endpoint,
		Bucket:          cfg.Storage.Bucket,
		Prefix:          "restic",
		Region:          cfg.Storage.Region,
		AccessKeyID:     cfg.Storage.AccessKeyID,
		SecretAccessKey: cfg.Storage.SecretAccessKey,
		PasswordFile:    cfg.Backup.ResticPasswordFile,
		// systemd gives the service no cache directory of its own.
		CacheDir: filepath.Join(cfg.DataDir, "restic-cache"),
	}
}

// backupsConfigured reports whether backups can run, and why not otherwise.
func backupsConfigured(cfg config.Config) (bool, string) {
	if !cfg.Storage.Configured() {
		return false, "the [storage] section is not configured"
	}
	info, err := os.Stat(cfg.Backup.ResticPasswordFile)
	if err != nil {
		return false, fmt.Sprintf("the restic password file %s is missing", cfg.Backup.ResticPasswordFile)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, fmt.Sprintf("the restic password file %s is readable by others (want mode 0600)", cfg.Backup.ResticPasswordFile)
	}
	return true, ""
}

// backupRecorder stores backup runs in the database.
type backupRecorder struct{ store *store.Store }

func (r backupRecorder) StartRun(ctx context.Context, trigger backup.Trigger, startedAt time.Time) (int64, error) {
	run, err := r.store.StartBackupRun(ctx, string(trigger), startedAt)
	return run.ID, err
}

func (r backupRecorder) FinishRun(ctx context.Context, id int64, status backup.RunStatus, errMsg string, finishedAt time.Time, targets []backup.TargetRecord) error {
	out := make([]store.BackupTarget, 0, len(targets))
	for _, t := range targets {
		out = append(out, store.BackupTarget{
			App: t.App, Kind: string(t.Kind), Target: t.Target, Status: string(t.Status), Error: t.Error,
		})
	}
	return r.store.FinishBackupRunWithTargets(ctx, id, status == backup.StatusSucceeded, errMsg, finishedAt, out)
}

func toBackupApp(app store.App) backup.App {
	out := backup.App{
		Slug:           app.Slug,
		Project:        app.ProjectName(),
		ExcludeVolumes: app.BackupExcludeVolumes,
	}
	// The dump hook runs inside a container, so it only makes sense while
	// the app is up.
	if app.DesiredState == store.DesiredRunning && app.CurrentDeploymentID != nil {
		out.DumpService, out.DumpCommand = app.DumpHookService, app.DumpHookCommand
		if live := app.LiveService(app.DumpHookService); live != app.DumpHookService {
			out.DumpExecService = live
		}
	}
	return out
}

// dumpRunner makes sure an app's dump service runs while a dump is taken:
// a sleeping app starts only that service (ARCHITECTURE 8.4).
// *deploy.Engine implements it.
type dumpRunner interface {
	WithServiceRunning(ctx context.Context, appID int64, service string, fn func(context.Context) error) error
}

// toDumpingApp is toBackupApp for a backup that takes the dump: the dump
// runs through dumps.
func toDumpingApp(app store.App, dumps dumpRunner) backup.App {
	out := toBackupApp(app)
	if out.DumpService != "" && dumps != nil {
		id, service := app.ID, out.DumpService
		out.WithDumpService = func(ctx context.Context, run func(context.Context) error) error {
			return dumps.WithServiceRunning(ctx, id, service, run)
		}
	}
	return out
}

// backupApps lists the apps of the scheduled and manual runs: every app
// but review apps, whose data is a copy of their parent's (ARCHITECTURE
// 10.2).
func backupApps(st *store.Store, dumps dumpRunner) func(ctx context.Context) ([]backup.App, error) {
	return func(ctx context.Context) ([]backup.App, error) {
		apps, err := st.ListApps(ctx)
		if err != nil {
			return nil, err
		}
		reviews, err := st.ListReviewApps(ctx, 0)
		if err != nil {
			return nil, err
		}
		review := map[int64]bool{}
		for _, r := range reviews {
			review[r.AppID] = true
		}
		out := make([]backup.App, 0, len(apps))
		for _, app := range apps {
			if review[app.ID] {
				continue
			}
			out = append(out, toDumpingApp(app, dumps))
		}
		return out, nil
	}
}

// appController stops and starts apps around a volume restore. An app that
// was already stopped stays stopped afterwards.
type appController struct {
	store  *store.Store
	engine *deploy.Engine

	mu         sync.Mutex
	wasStopped map[string]bool
}

func (c *appController) Stop(ctx context.Context, app backup.App) error {
	current, err := c.store.GetApp(ctx, app.Slug)
	if err != nil {
		return err
	}
	if current.DesiredState != store.DesiredRunning {
		c.mu.Lock()
		if c.wasStopped == nil {
			c.wasStopped = map[string]bool{}
		}
		c.wasStopped[app.Slug] = true
		c.mu.Unlock()
		return nil
	}
	return c.engine.Stop(ctx, current)
}

func (c *appController) Start(ctx context.Context, app backup.App) error {
	c.mu.Lock()
	stopped := c.wasStopped[app.Slug]
	delete(c.wasStopped, app.Slug)
	c.mu.Unlock()
	if stopped {
		return nil
	}
	current, err := c.store.GetApp(ctx, app.Slug)
	if err != nil {
		return err
	}
	return c.engine.Start(ctx, current)
}

// WithServiceRunning implements dumpRunner with the engine.
func (c *appController) WithServiceRunning(ctx context.Context, appID int64, service string, fn func(context.Context) error) error {
	if c.engine == nil {
		return fn(ctx)
	}
	return c.engine.WithServiceRunning(ctx, appID, service, fn)
}

// appVolumes lists the named volumes of an app for the API. It goes through
// the backup package so that the API and the backup runs agree on what the
// volumes of an app are, but it needs no backup repository: it is wired up
// whether or not backups are configured.
type appVolumes struct{ runner runner.Runner }

func (v appVolumes) VolumeNames(ctx context.Context, app store.App) ([]string, error) {
	return backup.VolumeNames(ctx, v.runner, app.ProjectName())
}

// backupManager is the part of *backup.Manager the service uses.
type backupManager interface {
	State() backup.State
	Start(ctx context.Context, trigger backup.Trigger) (*backup.Run, error)
	BackupApp(ctx context.Context, app backup.App) (backup.RunResult, error)
	BackupDump(ctx context.Context, app backup.App) (string, error)
	Snapshots(ctx context.Context, app backup.App) ([]backup.Snapshot, error)
	RestoreVolume(ctx context.Context, app backup.App, snapshotID string) (backup.RestoreResult, error)
	LookupDump(ctx context.Context, app backup.App, snapshotID string) (backup.DumpFile, error)
	WriteDump(ctx context.Context, file backup.DumpFile, w io.Writer) error
}

// backupService adapts the backup manager to the API and the deploy engine.
type backupService struct {
	manager   backupManager
	scheduler *backup.Scheduler
	clock     clock.Clock
	// serverCtx outlives single requests: a manual run keeps going after
	// the request that started it has returned.
	serverCtx context.Context
	dumps     dumpRunner
}

func (b *backupService) Running() bool { return b.manager.State().Running() }

func (b *backupService) NextRun() time.Time { return b.scheduler.Next(b.clock.Now()) }

func (b *backupService) StartManual(context.Context) (store.BackupRun, error) {
	run, err := b.manager.Start(b.serverCtx, backup.TriggerManual)
	if errors.Is(err, backup.ErrBusy) {
		return store.BackupRun{}, api.ErrBackupBusy
	}
	if err != nil {
		return store.BackupRun{}, err
	}
	return store.BackupRun{
		ID:        run.ID,
		Trigger:   store.BackupTriggerManual,
		Status:    store.BackupRunning,
		StartedAt: run.StartedAt,
	}, nil
}

func (b *backupService) Snapshots(ctx context.Context, app store.App) ([]api.BackupSnapshot, error) {
	snapshots, err := b.manager.Snapshots(ctx, toBackupApp(app))
	if err != nil {
		return nil, err
	}
	out := make([]api.BackupSnapshot, 0, len(snapshots))
	for _, s := range snapshots {
		item := api.BackupSnapshot{ID: s.ID, Time: s.Time, Kind: string(s.Kind), Target: s.Target}
		if s.Size > 0 {
			size := s.Size
			item.SizeBytes = &size
		}
		out = append(out, item)
	}
	return out, nil
}

// mapBackupError turns the backup package's typed errors into the API's.
func mapBackupError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, backup.ErrSnapshotNotFound):
		return fmt.Errorf("%w: %w", api.ErrSnapshotNotFound, err)
	case errors.Is(err, backup.ErrBusy):
		return fmt.Errorf("%w: %w", api.ErrBackupBusy, err)
	default:
		return err
	}
}

func (b *backupService) RestoreVolume(ctx context.Context, app store.App, snapshotID string) error {
	_, err := b.manager.RestoreVolume(ctx, toBackupApp(app), snapshotID)
	return mapBackupError(err)
}

func (b *backupService) OpenDump(ctx context.Context, app store.App, snapshotID string) (string, func(io.Writer) error, error) {
	file, err := b.manager.LookupDump(ctx, toBackupApp(app), snapshotID)
	if err != nil {
		return "", nil, mapBackupError(err)
	}
	return file.Name, func(w io.Writer) error { return b.manager.WriteDump(ctx, file, w) }, nil
}

// BackupApp implements deploy.PreDeployBackup: one app, before its
// containers are replaced.
func (b *backupService) BackupApp(ctx context.Context, app store.App) error {
	result, err := b.manager.BackupApp(ctx, toDumpingApp(app, b.dumps))
	if errors.Is(err, backup.ErrBusy) {
		return errors.New("another backup is running; deploy again when it has finished")
	}
	if err != nil {
		return err
	}
	if result.Status != backup.StatusSucceeded {
		return errors.New(result.Error)
	}
	return nil
}

// BackupDump implements deploy.CopyBackups: the target of a database copy,
// before its data is replaced.
func (b *backupService) BackupDump(ctx context.Context, app store.App) (string, error) {
	id, err := b.manager.BackupDump(ctx, toDumpingApp(app, b.dumps))
	if errors.Is(err, backup.ErrBusy) {
		return "", errors.New("another backup is running; copy again when it has finished")
	}
	return id, err
}

// LatestDump implements deploy.CopyBackups: the newest dump of the source
// of a database copy.
func (b *backupService) LatestDump(ctx context.Context, app store.App) (deploy.DumpSnapshot, error) {
	snapshots, err := b.manager.Snapshots(ctx, toBackupApp(app))
	if err != nil {
		return deploy.DumpSnapshot{}, err
	}
	for _, s := range snapshots { // newest first
		if s.Kind != backup.KindDump {
			continue
		}
		file, err := b.manager.LookupDump(ctx, toBackupApp(app), s.ID)
		if err != nil {
			return deploy.DumpSnapshot{}, err
		}
		return deploy.DumpSnapshot{
			ID:   s.ID,
			Time: s.Time,
			Write: func(ctx context.Context, w io.Writer) error {
				return b.manager.WriteDump(ctx, file, w)
			},
		}, nil
	}
	return deploy.DumpSnapshot{}, deploy.ErrNoDumpSnapshot
}

// backupDeps are the shared dependencies of the backup service.
type backupDeps struct {
	runner     runner.Runner
	clock      clock.Clock
	logger     *slog.Logger
	controller *appController
}

// newBackupService builds the manager and its scheduler. The caller starts
// the scheduler loop and gives the controller its engine.
func newBackupService(ctx context.Context, cfg config.Config, configDir string, st *store.Store,
	deps backupDeps) (*backupService, error) {
	manager, err := backup.New(backup.Options{
		Repository: backupRepository(cfg),
		Runner:     deps.runner,
		Clock:      deps.clock,
		Logger:     deps.logger,
		Recorder:   backupRecorder{store: st},
		Apps:       backupApps(st, deps.controller),
		System: &backup.System{
			SnapshotDB: st.VacuumInto,
			ConfigDir:  configDir,
			SkipDirs:   []string{cfg.DataDir},
		},
		Controller: deps.controller,
		WorkDir:    filepath.Join(cfg.DataDir, "backup-work"),
		TrashDir:   filepath.Join(cfg.DataDir, "restore-trash"),
	})
	if err != nil {
		return nil, err
	}
	return &backupService{
		manager: manager,
		scheduler: &backup.Scheduler{
			Manager:  manager,
			Clock:    deps.clock,
			Hour:     cfg.Backup.Hour,
			Location: cfg.Location(),
			Logger:   deps.logger,
		},
		clock:     deps.clock,
		serverCtx: ctx,
		dumps:     deps.controller,
	}, nil
}
