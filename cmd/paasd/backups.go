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

func (r backupRecorder) FinishRun(ctx context.Context, id int64, status backup.RunStatus, errMsg string, finishedAt time.Time) error {
	return r.store.FinishBackupRun(ctx, id, status == backup.StatusSucceeded, errMsg, finishedAt)
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
	}
	return out
}

// backupApps lists every app for the scheduled and manual runs.
func backupApps(st *store.Store) func(ctx context.Context) ([]backup.App, error) {
	return func(ctx context.Context) ([]backup.App, error) {
		apps, err := st.ListApps(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]backup.App, 0, len(apps))
		for _, app := range apps {
			out = append(out, toBackupApp(app))
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

// backupManager is the part of *backup.Manager the service uses.
type backupManager interface {
	State() backup.State
	Start(ctx context.Context, trigger backup.Trigger) (*backup.Run, error)
	BackupApp(ctx context.Context, app backup.App) (backup.RunResult, error)
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
	result, err := b.manager.BackupApp(ctx, toBackupApp(app))
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
		Apps:       backupApps(st),
		System: &backup.System{
			SnapshotDB: st.VacuumInto,
			ConfigDir:  configDir,
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
	}, nil
}
