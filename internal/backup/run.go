package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// TargetResult is the outcome of one snapshot attempt.
type TargetResult struct {
	// App is the slug; empty for the system.
	App  string
	Kind Kind
	// Target is the volume name or the dump hook service; empty for the
	// system and for an app whose volumes could not even be listed.
	Target string
	// SnapshotID is the full restic ID of the stored snapshot.
	SnapshotID string
	// Warning is set when a volume snapshot was stored although some files
	// could not be read (they changed or vanished while the app ran).
	Warning string
	// Err is nil when the snapshot was stored.
	Err error
}

func (t TargetResult) label() string {
	switch {
	case t.Kind == KindSystem:
		return "system"
	case t.Target == "":
		return fmt.Sprintf("%s %ss", t.App, t.Kind)
	default:
		return fmt.Sprintf("%s %s %s", t.App, t.Kind, t.Target)
	}
}

// RunResult is the outcome of a run.
type RunResult struct {
	// ID is the Recorder's ID of the run (0 without a Recorder).
	ID         int64
	Trigger    Trigger
	Status     RunStatus
	StartedAt  time.Time
	FinishedAt time.Time
	Targets    []TargetResult
	// RepositoryErr is set when the repository could not be opened or the
	// apps could not be listed; nothing was backed up then.
	RepositoryErr error
	// ForgetErr and CheckErr report the maintenance of a scheduled run.
	ForgetErr error
	CheckErr  error
	// Error is the short summary handed to the Recorder; empty on success.
	Error string
}

// Run is a run in progress, as returned by Start.
type Run struct {
	ID        int64
	Trigger   Trigger
	StartedAt time.Time

	done   chan struct{}
	result RunResult
	err    error
}

// Done is closed when the run has finished and has been recorded.
func (r *Run) Done() <-chan struct{} { return r.done }

// Wait blocks until the run has finished. The error matches ErrRunFailed
// when a step failed; the result tells which.
func (r *Run) Wait() (RunResult, error) {
	<-r.done
	return r.result, r.err
}

// plan says what one run does.
type plan struct {
	system bool
	// apps lists what to back up.
	apps func(ctx context.Context) ([]App, error)
	// forget applies the retention policy afterwards; check verifies the
	// repository; trash removes expired restore trash.
	forget bool
	check  bool
	trash  bool
}

// Start begins a run that backs up the system and every app, and returns as
// soon as the run is recorded. A scheduled run also applies the retention
// policy afterwards. The error matches ErrBusy when something else holds
// the repository.
//
// ctx bounds the whole run, which goes on after Start returns: pass the
// context of the server, not that of an HTTP request.
func (m *Manager) Start(ctx context.Context, trigger Trigger) (*Run, error) {
	return m.start(ctx, trigger, plan{
		system: m.opts.System != nil,
		apps:   m.opts.Apps,
		forget: trigger == TriggerScheduled,
	})
}

// RunAll is Start followed by Wait.
func (m *Manager) RunAll(ctx context.Context, trigger Trigger) (RunResult, error) {
	run, err := m.Start(ctx, trigger)
	if err != nil {
		return RunResult{}, err
	}
	return run.Wait()
}

// BackupApp backs up the volumes and the dump of one app and waits for it.
// It is recorded as a run with TriggerPreDeploy. The error matches ErrBusy
// when another operation is in progress and ErrRunFailed when a snapshot
// could not be stored.
func (m *Manager) BackupApp(ctx context.Context, app App) (RunResult, error) {
	if err := app.validate(); err != nil {
		return RunResult{}, err
	}
	run, err := m.start(ctx, TriggerPreDeploy, plan{
		apps: func(context.Context) ([]App, error) { return []App{app}, nil },
	})
	if err != nil {
		return RunResult{}, err
	}
	return run.Wait()
}

func (m *Manager) start(ctx context.Context, trigger Trigger, p plan) (*Run, error) {
	if p.apps == nil {
		return nil, errors.New("backup: Options.Apps is required to back up every app")
	}
	release, err := m.begin(OpRun, trigger)
	if err != nil {
		return nil, err
	}
	run := &Run{Trigger: trigger, StartedAt: m.State().StartedAt, done: make(chan struct{})}
	if m.opts.Recorder != nil {
		id, err := m.opts.Recorder.StartRun(ctx, trigger, run.StartedAt)
		if err != nil {
			release()
			return nil, fmt.Errorf("record the start of the backup run: %w", err)
		}
		run.ID = id
		m.setRunID(id)
	}
	go func() {
		defer close(run.done)
		defer release()
		run.result, run.err = m.execute(ctx, run, p)
	}()
	return run, nil
}

// execute performs the steps of a run and records its end.
func (m *Manager) execute(ctx context.Context, run *Run, p plan) (RunResult, error) {
	log := m.opts.Logger.With("run", run.ID, "trigger", string(run.Trigger))
	log.Info("backup run started")
	res := RunResult{ID: run.ID, Trigger: run.Trigger, StartedAt: run.StartedAt}

	res.RepositoryErr = m.prepare(ctx, true)
	var apps []App
	if res.RepositoryErr == nil {
		if apps, res.RepositoryErr = p.apps(ctx); res.RepositoryErr != nil {
			res.RepositoryErr = fmt.Errorf("list apps: %w", res.RepositoryErr)
		}
	}
	if res.RepositoryErr == nil {
		if p.system {
			res.Targets = append(res.Targets, m.backupSystem(ctx))
		}
		for _, app := range apps {
			res.Targets = append(res.Targets, m.backupApp(ctx, app)...)
		}
		// Maintenance runs even when a snapshot failed: one broken app
		// must not let the repository grow without bound.
		if p.forget && ctx.Err() == nil {
			res.ForgetErr = m.forget(ctx)
		}
		if p.check && ctx.Err() == nil {
			res.CheckErr = m.check(ctx)
		}
	}
	if p.trash && ctx.Err() == nil {
		if _, err := m.CleanTrash(ctx); err != nil {
			log.Warn("cleaning the restore trash failed", "error", m.masker.Mask(err.Error()))
		}
	}

	res.Error = m.summarize(res)
	res.Status = StatusSucceeded
	if res.Error != "" {
		res.Status = StatusFailed
	}
	res.FinishedAt = m.opts.Clock.Now()

	for _, t := range res.Targets {
		switch {
		case t.Err != nil:
			log.Error("backup failed", "target", t.label(), "error", m.masker.Mask(t.Err.Error()))
		case t.Warning != "":
			log.Warn("backup stored with unreadable files", "target", t.label(), "snapshot", t.SnapshotID, "warning", t.Warning)
		default:
			log.Info("backup stored", "target", t.label(), "snapshot", t.SnapshotID)
		}
	}
	log.Info("backup run finished", "status", string(res.Status), "error", res.Error)

	var err error
	if res.Status == StatusFailed {
		err = fmt.Errorf("%w: %s", ErrRunFailed, res.Error)
	}
	if m.opts.Recorder != nil {
		// The run must be closed even when ctx was canceled.
		recCtx, cancel := cleanupContext(ctx, 30*time.Second)
		defer cancel()
		if rerr := m.opts.Recorder.FinishRun(recCtx, res.ID, res.Status, res.Error, res.FinishedAt); rerr != nil {
			log.Error("recording the end of the backup run failed", "error", rerr)
			err = errors.Join(err, fmt.Errorf("record the end of the backup run: %w", rerr))
		}
	}
	return res, err
}

// prepare opens the repository before an exclusive operation and clears
// stale locks.
func (m *Manager) prepare(ctx context.Context, create bool) error {
	if err := m.ensureRepository(ctx, create); err != nil {
		return fmt.Errorf("open the repository: %w", err)
	}
	if err := m.unlockStale(ctx); err != nil {
		return fmt.Errorf("remove stale repository locks: %w", err)
	}
	return nil
}

// errorSummaryBytes bounds the error text stored for a run.
const errorSummaryBytes = 1000

// summarize builds the short error text of a run. Empty means success.
func (m *Manager) summarize(res RunResult) string {
	var parts []string
	if res.RepositoryErr != nil {
		parts = append(parts, res.RepositoryErr.Error())
	}
	for _, t := range res.Targets {
		if t.Err != nil {
			parts = append(parts, t.label()+": "+t.Err.Error())
		}
	}
	if res.ForgetErr != nil {
		parts = append(parts, "retention: "+res.ForgetErr.Error())
	}
	if res.CheckErr != nil {
		parts = append(parts, "repository check: "+res.CheckErr.Error())
	}
	text := m.masker.Mask(strings.Join(parts, " | "))
	if len(text) > errorSummaryBytes {
		text = strings.ToValidUTF8(text[:errorSummaryBytes], "") + "..."
	}
	return text
}

// backupApp stores one snapshot per volume and one for the dump hook. A
// failure of one target does not stop the others.
func (m *Manager) backupApp(ctx context.Context, app App) []TargetResult {
	if err := app.validate(); err != nil {
		return []TargetResult{{App: app.Slug, Kind: KindVolume, Err: err}}
	}
	var results []TargetResult
	vols, err := m.volumes(ctx, app.Project)
	if err != nil {
		results = append(results, TargetResult{App: app.Slug, Kind: KindVolume, Err: fmt.Errorf("list volumes: %w", err)})
	}
	for _, v := range vols {
		if slices.Contains(app.ExcludeVolumes, v.Short) {
			continue
		}
		results = append(results, m.backupVolume(ctx, app, v))
	}
	if app.DumpService != "" {
		results = append(results, m.backupDump(ctx, app))
	}
	return results
}

// backupVolume stores the contents of one volume while the app runs.
func (m *Manager) backupVolume(ctx context.Context, app App, v volume) TargetResult {
	res := TargetResult{App: app.Slug, Kind: KindVolume, Target: v.Short}
	report, err := m.backupDir(ctx, v.Mountpoint, appTags(app.Slug, KindVolume, v.Short))
	res.SnapshotID = report.snapshotID()
	switch {
	case err == nil:
	case exitCode(err) == exitIncomplete && res.SnapshotID != "":
		// A live volume loses and changes files under restic's feet. The
		// snapshot holds everything that could be read; say so instead of
		// calling a usable backup a failure every night.
		res.Warning = "some files could not be read: " + report.problemText()
	default:
		res.SnapshotID = ""
		res.Err = err
	}
	return res
}
