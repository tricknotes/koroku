// Package backup stores the platform's data in a restic repository and
// brings it back (ARCHITECTURE chapter 10.2).
//
// Three things are backed up, each as its own restic snapshot:
//
//   - the system: a consistent copy of the SQLite database plus the
//     configuration directory (kind:system);
//   - every named volume of an app's Compose project, read while the app
//     runs (kind:volume);
//   - the output of an app's optional dump hook, streamed from
//     `docker compose exec` into `restic backup --stdin` (kind:dump).
//
// Snapshots are found again through their tags, never through paths or host
// names, so they stay usable after the host is replaced:
//
//	system, kind:system
//	app:<slug>, kind:volume, target:<volume name in the Compose file>
//	app:<slug>, kind:dump,   target:<dump hook service>
//
// restic is driven as a command line program through runner.Runner. The
// repository location, the password file and the storage credentials travel
// in the environment of the command, never in its arguments. restic 0.17 or
// newer is required: the package relies on its exit status 10 ("repository
// does not exist") to initialize the repository on first use.
//
// The package knows nothing about the database, the HTTP API or the
// configuration file. The caller hands in plain values and small interfaces
// (Options) and records runs through Recorder.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
)

// Kind tells what a snapshot holds.
type Kind string

const (
	KindSystem Kind = "system"
	KindVolume Kind = "volume"
	KindDump   Kind = "dump"
)

// Trigger tells why a run started.
type Trigger string

const (
	TriggerScheduled Trigger = "scheduled"
	TriggerManual    Trigger = "manual"
	TriggerPreDeploy Trigger = "pre_deploy"
	// TriggerPreCopy is the dump of a staging app taken before a database
	// copy replaces its data (ARCHITECTURE 10.6). It is not recorded as a
	// run; the copy keeps the snapshot ID.
	TriggerPreCopy Trigger = "pre_copy"
)

// RunStatus is the state of a run.
type RunStatus string

const (
	StatusRunning   RunStatus = "running"
	StatusSucceeded RunStatus = "succeeded"
	StatusFailed    RunStatus = "failed"
)

// App is what the package needs to know about one app.
type App struct {
	// Slug identifies the app in snapshot tags (app:<slug>).
	Slug string
	// Project is the Compose project name (koroku-<slug>).
	Project string
	// ExcludeVolumes lists volumes that are not backed up, by the name
	// written in the Compose file (pgdata, not koroku-blog_pgdata).
	ExcludeVolumes []string
	// DumpService and DumpCommand are the dump hook: the command runs with
	// `sh -c` inside the service's container and its standard output is
	// stored. Both empty means no hook. The hook needs a running container,
	// so leave it empty for an app that is stopped. The exit status of the
	// command decides whether the dump is kept; a pipeline should therefore
	// use `set -o pipefail` or avoid pipes.
	DumpService string
	// DumpExecService is the service whose container the dump command runs
	// in, when it differs from DumpService: the colour the public service
	// of a blue/green app runs in (ARCHITECTURE 7.2). DumpService still
	// names the dump.
	DumpExecService string
	DumpCommand     string
	// WithDumpService, when set, runs the dump inside it: it makes sure
	// the dump service runs while run does (an app that sleeps while idle
	// starts only that service for the dump).
	WithDumpService func(ctx context.Context, run func(context.Context) error) error
	// OutputMasker, when set, returns a masker that knows the secret values
	// the dump command can print: the values of the app's environment
	// variables. What the command writes to its standard error ends up in
	// the message of a failed dump, which everyone who may view the app
	// reads, and tools such as pg_dump print connection strings when they
	// fail. It is called once for every dump, right before the command
	// runs. When it fails, nothing of the command's output is kept. Nil
	// means that the app has no values to hide.
	OutputMasker func(ctx context.Context) (*secrets.Masker, error)
}

// Recorder keeps the history of runs. The caller implements it on the
// database. FinishRun is called exactly once for every StartRun that
// succeeded, unless the process dies in between; runs left in StatusRunning
// by a crash are the caller's to clean up at startup.
type Recorder interface {
	StartRun(ctx context.Context, trigger Trigger, startedAt time.Time) (id int64, err error)
	// FinishRun stores the outcome. errMsg is a short, already masked
	// summary; it is empty when status is StatusSucceeded. targets says
	// what happened to each target; it is empty when the run failed before
	// backing anything up.
	FinishRun(ctx context.Context, id int64, status RunStatus, errMsg string, finishedAt time.Time, targets []TargetRecord) error
}

// TargetStatus is the outcome of one target of a run.
type TargetStatus string

const (
	TargetSucceeded TargetStatus = "succeeded"
	// TargetWarning: stored, but some files could not be read.
	TargetWarning TargetStatus = "warning"
	TargetFailed  TargetStatus = "failed"
)

// TargetRecord is what a Recorder keeps of one target: its outcome and,
// when it did not simply succeed, a short, already masked message.
type TargetRecord struct {
	// App is the slug; empty for the system.
	App    string
	Kind   Kind
	Target string
	Status TargetStatus
	Error  string
}

// AppController stops and starts an app around a volume restore.
//
// Start is only called after Stop succeeded. When Stop returns an error
// (for example because a deployment is in progress) nothing has been touched
// and the restore ends with that error, wrapped; Stop must then have left the
// app running. An implementation that wants an app that was already stopped
// to stay stopped can remember that in Stop and make Start a no-op.
type AppController interface {
	Stop(ctx context.Context, app App) error
	Start(ctx context.Context, app App) error
}

// System describes the system backup.
type System struct {
	// SnapshotDB writes a consistent copy of the database to destPath
	// (VACUUM INTO). destPath does not exist when it is called.
	SnapshotDB func(ctx context.Context, destPath string) error
	// ConfigDir is copied into the snapshot (/etc/koroku). Empty skips it.
	ConfigDir string
	// SkipDirs are directories below ConfigDir that are not configuration
	// and are left out of the copy: the data directory, when it lives
	// inside the configuration directory (the local development setup
	// keeps both under data/dev). Without this the copy would include the
	// database, every release and the staging directory it is writing to.
	SkipDirs []string
}

// Retention is the number of generations kept per snapshot group.
type Retention struct {
	Daily   int
	Weekly  int
	Monthly int
}

// Timeouts bound the external commands. Zero values get the defaults.
type Timeouts struct {
	// Command bounds short commands: docker volume queries, snapshot
	// listings, repository initialization, the database copy. Default 5m.
	Command time.Duration
	// Backup bounds one `restic backup` (one volume, one dump, the system).
	// Default 6h.
	Backup time.Duration
	// Restore bounds one `restic restore` or `restic dump`. Default 6h.
	Restore time.Duration
	// Maintenance bounds `restic forget --prune` and `restic check`, each.
	// Default 6h.
	Maintenance time.Duration
	// Control bounds stopping or starting an app around a restore.
	// Default 15m.
	Control time.Duration
	// LockWait is how long restic waits for a repository lock held by
	// another restic process (--retry-lock). Default 1m.
	LockWait time.Duration
}

// Options are the dependencies and settings of a Manager.
type Options struct {
	Repository Repository
	Runner     runner.Runner
	Clock      clock.Clock
	Logger     *slog.Logger

	// Recorder is optional; without it runs are not recorded.
	Recorder Recorder
	// Apps lists the apps RunAll backs up.
	Apps func(ctx context.Context) ([]App, error)
	// System is optional; without it RunAll skips the system backup.
	System *System
	// Controller is needed by RestoreVolume only.
	Controller AppController

	// WorkDir is a private directory where the system backup is staged
	// (/var/lib/koroku/backup-work). Needed when System is set.
	WorkDir string
	// TrashDir receives the previous contents of a restored volume
	// (/var/lib/koroku/restore-trash). It must be on the same filesystem as
	// the Docker volumes: the contents are moved with rename so that
	// ownership and special files survive untouched.
	TrashDir string
	// TrashRetention is how long CleanTrash keeps them. Default 7 days.
	TrashRetention time.Duration

	// Host is recorded as the host name of every snapshot. It is fixed so
	// that replacing the machine does not change it. Default "koroku".
	Host string
	// Keep defaults to 7 daily, 4 weekly and 6 monthly generations.
	Keep     Retention
	Timeouts Timeouts
}

func (o *Options) applyDefaults() {
	if o.Clock == nil {
		o.Clock = clock.System{}
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.TrashRetention == 0 {
		o.TrashRetention = 7 * 24 * time.Hour
	}
	if o.Host == "" {
		o.Host = "koroku"
	}
	if o.Keep == (Retention{}) {
		o.Keep = Retention{Daily: 7, Weekly: 4, Monthly: 6}
	}
	t := &o.Timeouts
	if t.Command == 0 {
		t.Command = 5 * time.Minute
	}
	if t.Backup == 0 {
		t.Backup = 6 * time.Hour
	}
	if t.Restore == 0 {
		t.Restore = 6 * time.Hour
	}
	if t.Maintenance == 0 {
		t.Maintenance = 6 * time.Hour
	}
	if t.Control == 0 {
		t.Control = 15 * time.Minute
	}
	if t.LockWait == 0 {
		t.LockWait = time.Minute
	}
}

// Operation names what holds the repository exclusively.
type Operation string

const (
	// OpRun is a backup run (RunAll, Start, BackupApp, BackupDump).
	OpRun Operation = "run"
	// OpRestore is a volume restore.
	OpRestore Operation = "restore"
	// OpMaintenance is a standalone Forget or Check.
	OpMaintenance Operation = "maintenance"
)

// State tells what the Manager is doing right now.
type State struct {
	// Busy is true while a run, a volume restore or maintenance holds the
	// repository; a second one is refused with ErrBusy.
	Busy      bool
	Operation Operation
	// Trigger and RunID are set for OpRun. RunID is 0 until the Recorder
	// has answered.
	Trigger   Trigger
	RunID     int64
	StartedAt time.Time
}

// Running reports whether a backup run is in progress.
func (s State) Running() bool { return s.Busy && s.Operation == OpRun }

// ErrBusy is matched (errors.Is) by the error returned when another run,
// restore or maintenance is in progress. The concrete error is *BusyError.
var ErrBusy = errors.New("another backup operation is in progress")

// BusyError tells what is holding the repository.
type BusyError struct {
	State State
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("%v (%s since %s)", ErrBusy, e.State.Operation, e.State.StartedAt.UTC().Format(time.RFC3339))
}

// Is makes errors.Is(err, ErrBusy) true.
func (e *BusyError) Is(target error) bool { return target == ErrBusy }

var (
	// ErrSnapshotNotFound is matched by the error returned for a snapshot
	// ID that is malformed, does not exist, belongs to another app, or is
	// not of the kind the operation needs (restoring a dump as a volume).
	// One error covers all of them so that callers answer "not found"
	// without telling apart what exists for other apps.
	ErrSnapshotNotFound = errors.New("snapshot not found")
	// ErrNoRepository is returned when a restore finds no repository at
	// the configured location.
	ErrNoRepository = errors.New("no restic repository at the configured location")
	// ErrRunFailed is matched by the error of a run with a failed step.
	ErrRunFailed = errors.New("backup failed")
)

// Manager runs backups and restores against one restic repository.
type Manager struct {
	opts   Options
	masker *secrets.Masker

	mu    sync.Mutex
	state State

	repoMu    sync.Mutex
	repoReady bool
}

// New validates opts and returns a Manager. It runs no command.
func New(opts Options) (*Manager, error) {
	opts.applyDefaults()
	if opts.Runner == nil {
		return nil, errors.New("backup: a Runner is required")
	}
	if err := opts.Repository.validate(); err != nil {
		return nil, err
	}
	if !nameRE.MatchString(opts.Host) {
		return nil, fmt.Errorf("backup: invalid host name %q", opts.Host)
	}
	if opts.System != nil {
		if opts.System.SnapshotDB == nil {
			return nil, errors.New("backup: System.SnapshotDB is required")
		}
		if opts.WorkDir == "" {
			return nil, errors.New("backup: WorkDir is required for the system backup")
		}
	}
	return &Manager{
		opts:   opts,
		masker: secrets.NewMasker(opts.Repository.AccessKeyID, opts.Repository.SecretAccessKey),
	}, nil
}

// State returns what the Manager is doing right now.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// begin claims the repository for one exclusive operation. The returned
// function releases it.
func (m *Manager) begin(op Operation, trigger Trigger) (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Busy {
		return nil, &BusyError{State: m.state}
	}
	m.state = State{Busy: true, Operation: op, Trigger: trigger, StartedAt: m.opts.Clock.Now()}
	return func() {
		m.mu.Lock()
		m.state = State{}
		m.mu.Unlock()
	}, nil
}

func (m *Manager) setRunID(id int64) {
	m.mu.Lock()
	m.state.RunID = id
	m.mu.Unlock()
}

// nameRE is what slugs, volume names and service names may look like. Tags
// are built from them, and restic separates tags with commas.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func (a App) validate() error {
	if !nameRE.MatchString(a.Slug) {
		return fmt.Errorf("backup: invalid app slug %q", a.Slug)
	}
	if !nameRE.MatchString(a.Project) {
		return fmt.Errorf("backup: invalid Compose project name %q for app %s", a.Project, a.Slug)
	}
	if (a.DumpService == "") != (a.DumpCommand == "") {
		return fmt.Errorf("backup: the dump hook of app %s needs both a service and a command", a.Slug)
	}
	if a.DumpService != "" && !nameRE.MatchString(a.DumpService) {
		return fmt.Errorf("backup: invalid dump hook service %q for app %s", a.DumpService, a.Slug)
	}
	return nil
}

// cleanupContext returns a context for work that must still happen after ctx
// was canceled: recording the end of a run, putting a volume back.
func cleanupContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), timeout)
}
