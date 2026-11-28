// Package pitr archives the WAL of a PostgreSQL service and takes its base
// backups, so that its database can be brought back to a point in time
// (ARCHITECTURE 10.9).
//
// Everything that touches the database's files runs inside the service's
// own containers (docker compose exec): the uid of the postgres user and
// the data directory are those of the image, which koroku does not need to
// know. Snapshots go through the backup package's repository.
package pitr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// ArchiveCommand copies a finished WAL segment into the mounted archive
// directory. Without the mount it succeeds and keeps nothing: a failing
// command would make PostgreSQL keep every segment until the disk is full
// (ARCHITECTURE 10.9). A segment archived twice must be the same.
const ArchiveCommand = `test -d /koroku/wal || exit 0; f=/koroku/wal/%f; if [ -f "$f" ]; then cmp -s %p "$f"; else cp %p "$f.tmp" && mv "$f.tmp" "$f"; fi`

// ArchiveTimeout forces a segment switch at least this often, so that a
// quiet database still reaches the archive.
const ArchiveTimeout = "5min"

// MinServerVersion is the oldest PostgreSQL supported: recovery.signal and
// a recovery that stops with an error short of its target.
const MinServerVersion = 140000

// Activity entries of the preparation (ARCHITECTURE 10.7).
const (
	ActPrepared = "pitr.prepared"
	ActFailed   = "pitr.failed"
)

// Engine is what the service needs from the deploy engine.
type Engine interface {
	// WithAppLock runs fn under the app's operation lock.
	WithAppLock(ctx context.Context, appID int64, fn func(context.Context) error) error
	// WithServiceRunning runs fn while the service runs, starting only it
	// for an app that sleeps.
	WithServiceRunning(ctx context.Context, appID int64, service string, fn func(context.Context) error) error
	// OutputMasker knows the values of the app's variables.
	OutputMasker(ctx context.Context, app store.App) (*secrets.Masker, error)
}

// Repository stores the snapshots.
type Repository interface {
	BackupStream(ctx context.Context, s backup.Stream) (snapshotID string, err error)
}

// Options are the dependencies and settings of a Service.
type Options struct {
	Store  *store.Store
	Repo   Repository
	Engine Engine
	Runner runner.Runner
	Clock  clock.Clock
	Logger *slog.Logger
	// AppsDir holds the archive directories (compose.WALDir).
	AppsDir string
	// Hour, Minute and Location are the time of the daily backup run; the
	// daily base backups follow it.
	Hour, Minute int
	Location     *time.Location
	// PollInterval is how often the loop reads the clock. Default 30s.
	PollInterval time.Duration
	// MaxLocalBytes bounds the WAL waiting on the host when shipping
	// fails. Default 2 GiB.
	MaxLocalBytes int64
	// DiskFull reports a data disk at or above the log collector's limit
	// (ARCHITECTURE 10.1); the WAL waiting on the host is dropped then.
	// Optional.
	DiskFull func() bool
	// CommandTimeout bounds one docker command. Default 2m.
	CommandTimeout time.Duration
	// RestartTimeout bounds the restart of the database. Default 5m.
	RestartTimeout time.Duration
}

// Service runs the archiving of every app with point-in-time recovery.
type Service struct {
	opts Options

	// mu serializes the work on the apps: the loop and the preparations
	// started by deployments.
	mu sync.Mutex

	queue    chan int64
	pendMu   sync.Mutex
	needBase map[int64]bool
}

// New returns a Service.
func New(opts Options) *Service {
	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 30 * time.Second
	}
	if opts.MaxLocalBytes <= 0 {
		opts.MaxLocalBytes = 2 << 30
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 2 * time.Minute
	}
	if opts.RestartTimeout <= 0 {
		opts.RestartTimeout = 5 * time.Minute
	}
	return &Service{opts: opts, queue: make(chan int64, 64), needBase: map[int64]bool{}}
}

// Available reports whether snapshots can be stored: backups are
// configured.
func (s *Service) Available() bool { return s.opts.Repo != nil }

// --- docker -------------------------------------------------------------

func execArgs(app store.App, service string, args ...string) []string {
	return append([]string{"compose", "-p", app.ProjectName(), "exec", "-T", service}, args...)
}

// run runs a docker command and returns its standard output. The error
// carries the end of the standard error.
func (s *Service) run(ctx context.Context, args []string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.CommandTimeout)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err := s.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args, Stdout: &stdout, Stderr: &stderr})
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, lastLines(stderr.String(), 3))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// sh runs a shell script in the service's container as the image's user.
// Further arguments are $1, $2, ... of the script.
func (s *Service) sh(ctx context.Context, app store.App, service, script string, args ...string) (string, error) {
	return s.run(ctx, execArgs(app, service, append([]string{"sh", "-c", script, "sh"}, args...)...))
}

// psql connects over the local socket as the image's database user, which
// the official image trusts (ARCHITECTURE 10.9).
const psqlScript = `exec psql -X -tA -v ON_ERROR_STOP=1 -U "${POSTGRES_USER:-postgres}" -d "${POSTGRES_DB:-${POSTGRES_USER:-postgres}}" -c "$1"`

func (s *Service) sql(ctx context.Context, app store.App, service, query string) (string, error) {
	out, err := s.sh(ctx, app, service, psqlScript, query)
	if err != nil {
		return "", fmt.Errorf("psql %q: %w", query, err)
	}
	return out, nil
}

const baseBackupScript = `exec pg_basebackup -U "${POSTGRES_USER:-postgres}" -D - -Ft -X none --checkpoint=fast --no-manifest`

func lastLines(text string, n int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// mask hides the values of the app's variables in a message that is kept
// and shown with the app.
func (s *Service) mask(ctx context.Context, app store.App, err error) string {
	msg := err.Error()
	if m, merr := s.opts.Engine.OutputMasker(ctx, app); merr == nil && m != nil {
		msg = m.Mask(msg)
	} else if merr != nil {
		return "the reason is not kept, because the values to hide in it could not be loaded"
	}
	if len(msg) > 1000 {
		msg = strings.ToValidUTF8(msg[:1000], "") + "..."
	}
	return msg
}

func (s *Service) backupApp(app store.App) backup.App {
	return backup.App{
		Slug: app.Slug, Project: app.ProjectName(),
		OutputMasker: func(ctx context.Context) (*secrets.Masker, error) { return s.opts.Engine.OutputMasker(ctx, app) },
	}
}

// --- preparation --------------------------------------------------------

// ErrNotMounted is the reason of a preparation that found no archive
// directory: the running release was deployed before the setting.
var ErrNotMounted = errors.New("the archive directory is not mounted into the service; deploy the app again")

// Prepare sets the archiving of the app up after a deployment
// (ARCHITECTURE 10.9): it checks the service, gives it the archive
// directory, writes the settings with ALTER SYSTEM, restarts the database
// when archive_mode changes, and takes a base backup when one is due.
func (s *Service) Prepare(ctx context.Context, app store.App) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prepare(ctx, app)
}

func (s *Service) prepare(ctx context.Context, app store.App) error {
	service := app.PITRService
	if service == "" {
		return nil
	}
	now := s.opts.Clock.Now()
	st, err := s.opts.Store.GetPITRState(ctx, app.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if st.Service != service {
		st = store.PITRState{AppID: app.ID, Service: service}
	}
	wasReady := st.Status == store.PITRReady
	if !wasReady {
		st.Status, st.Error, st.UpdatedAt = store.PITRPreparing, "", now
		if err := s.opts.Store.PutPITRState(ctx, st); err != nil {
			return err
		}
	}

	var needBase bool
	err = s.opts.Engine.WithAppLock(ctx, app.ID, func(ctx context.Context) error {
		return s.opts.Engine.WithServiceRunning(ctx, app.ID, service, func(ctx context.Context) error {
			var err error
			needBase, err = s.setUp(ctx, app, &st, wasReady)
			return err
		})
	})
	st.UpdatedAt = s.opts.Clock.Now()
	switch {
	case errors.Is(err, ErrNotMounted) && wasReady:
		// A rollback to a release from before the setting: nothing is
		// archived while it runs (ARCHITECTURE 10.9).
		if gerr := s.opts.Store.OpenPITRGap(ctx, app.ID, service, store.PITRGapMountMissing, now); gerr != nil {
			return gerr
		}
		st.Warning = "The running release does not mount the archive directory: nothing is archived until the app is deployed again."
		return s.opts.Store.PutPITRState(ctx, st)
	case err != nil:
		st.Status, st.Error = store.PITRFailed, s.mask(ctx, app, err)
		_ = s.opts.Store.PutPITRState(ctx, st)
		s.record(ctx, app, ActFailed, map[string]any{"service": service, "error": st.Error})
		return err
	}
	st.Status, st.Error = store.PITRReady, ""
	if closed, cerr := s.opts.Store.ClosePITRGaps(ctx, app.ID, store.PITRGapMountMissing, now); cerr != nil {
		return cerr
	} else if closed {
		needBase = true
		st.Warning = ""
	}
	if err := s.opts.Store.PutPITRState(ctx, st); err != nil {
		return err
	}
	if !wasReady {
		s.record(ctx, app, ActPrepared, map[string]any{"service": service})
	}
	if needBase || !wasReady {
		if err := s.base(ctx, app); err != nil {
			// The loop takes it again; the setup itself is done.
			s.wantBase(app.ID)
			s.opts.Logger.Warn("base backup after the preparation failed", "app", app.Slug, "error", err)
		}
	}
	return nil
}

// setUp runs with the service up. It reports whether a base backup is due
// because the server's major version changed.
func (s *Service) setUp(ctx context.Context, app store.App, st *store.PITRState, wasReady bool) (needBase bool, err error) {
	service := app.PITRService
	version, err := s.serverVersion(ctx, app, service)
	if err != nil {
		return false, err
	}
	if version < MinServerVersion {
		return false, fmt.Errorf("PostgreSQL %d.%d is too old: point-in-time recovery needs 14 or later", version/10000, version%10000)
	}
	dataDir, err := s.sql(ctx, app, service, "SHOW data_directory")
	if err != nil {
		return false, err
	}
	if err := s.checkVolume(ctx, app, service, dataDir); err != nil {
		return false, err
	}
	if _, err := s.sh(ctx, app, service, `test -d "$1"`, compose.WALMountTarget); err != nil {
		return false, ErrNotMounted
	}
	if _, err := s.run(ctx, append([]string{"compose", "-p", app.ProjectName(), "exec", "-T", "-u", "root", service},
		"sh", "-c", `chown postgres:postgres "$1" && chmod 700 "$1"`, "sh", compose.WALMountTarget)); err != nil {
		return false, fmt.Errorf("give the archive directory to the postgres user: %w", err)
	}

	mode, err := s.sql(ctx, app, service, "SHOW archive_mode")
	if err != nil {
		return false, err
	}
	command, err := s.sql(ctx, app, service, "SHOW archive_command")
	if err != nil {
		return false, err
	}
	timeout, err := s.sql(ctx, app, service, "SHOW archive_timeout")
	if err != nil {
		return false, err
	}
	reload := false
	if command != ArchiveCommand {
		if _, err := s.sql(ctx, app, service, "ALTER SYSTEM SET archive_command = '"+ArchiveCommand+"'"); err != nil {
			return false, err
		}
		reload = true
	}
	if timeout != ArchiveTimeout {
		if _, err := s.sql(ctx, app, service, "ALTER SYSTEM SET archive_timeout = '"+ArchiveTimeout+"'"); err != nil {
			return false, err
		}
		reload = true
	}
	if mode != "on" {
		if _, err := s.sql(ctx, app, service, "ALTER SYSTEM SET archive_mode = 'on'"); err != nil {
			return false, err
		}
		if err := s.restart(ctx, app, service); err != nil {
			return false, err
		}
		if mode, err = s.sql(ctx, app, service, "SHOW archive_mode"); err != nil {
			return false, err
		} else if mode != "on" {
			return false, fmt.Errorf("archive_mode is %q after the restart; the Compose file may set it on the command line", mode)
		}
	} else if reload {
		if _, err := s.sql(ctx, app, service, "SELECT pg_reload_conf()"); err != nil {
			return false, err
		}
	}

	if wasReady && st.ServerVersionNum/10000 != version/10000 {
		if err := s.versionChanged(ctx, app, st.ServerVersionNum, version); err != nil {
			return false, err
		}
		needBase = true
	}
	st.DataDirectory, st.ServerVersionNum = dataDir, version
	return needBase, nil
}

func (s *Service) serverVersion(ctx context.Context, app store.App, service string) (int, error) {
	out, err := s.sql(ctx, app, service, "SHOW server_version_num")
	if err != nil {
		return 0, err
	}
	version, err := strconv.Atoi(out)
	if err != nil {
		return 0, fmt.Errorf("server_version_num %q: %w", out, err)
	}
	return version, nil
}

// versionChanged records that nothing before now can be recovered with
// the new major version (ARCHITECTURE 10.9).
func (s *Service) versionChanged(ctx context.Context, app store.App, from, to int) error {
	now := s.opts.Clock.Now()
	s.opts.Logger.Info("PostgreSQL major version changed; earlier backups cannot be recovered with it",
		"app", app.Slug, "from", from, "to", to)
	if err := s.opts.Store.OpenPITRGap(ctx, app.ID, app.PITRService, store.PITRGapVersionChanged, now.Add(-backup.PITRWindow-24*time.Hour)); err != nil {
		return err
	}
	_, err := s.opts.Store.ClosePITRGaps(ctx, app.ID, store.PITRGapVersionChanged, now)
	return err
}

// checkVolume makes sure the data directory lies in a named volume of the
// container, which is what a recovery replaces.
func (s *Service) checkVolume(ctx context.Context, app store.App, service, dataDir string) error {
	id, err := s.run(ctx, []string{"compose", "-p", app.ProjectName(), "ps", "-q", service})
	if err != nil || id == "" {
		return fmt.Errorf("find the container of %s: %w", service, err)
	}
	out, err := s.run(ctx, []string{"inspect", "--format", "{{range .Mounts}}{{.Type}} {{.Destination}}\n{{end}}", strings.Fields(id)[0]})
	if err != nil {
		return fmt.Errorf("inspect the container of %s: %w", service, err)
	}
	best, bestType := "", ""
	for _, line := range strings.Split(out, "\n") {
		kind, dest, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		if (dataDir == dest || strings.HasPrefix(dataDir, strings.TrimSuffix(dest, "/")+"/")) && len(dest) > len(best) {
			best, bestType = dest, kind
		}
	}
	if bestType != "volume" {
		return fmt.Errorf("the data directory %s is not in a named volume; mount a named volume there in the Compose file", dataDir)
	}
	return nil
}

// restart restarts the database and waits until it accepts connections.
func (s *Service) restart(ctx context.Context, app store.App, service string) error {
	ctx, cancel := context.WithTimeout(ctx, s.opts.RestartTimeout)
	defer cancel()
	if _, err := s.run(ctx, []string{"compose", "-p", app.ProjectName(), "restart", service}); err != nil {
		return fmt.Errorf("restart %s: %w", service, err)
	}
	for {
		if _, err := s.sh(ctx, app, service, `pg_isready -q`); err == nil {
			if _, err := s.sql(ctx, app, service, "SELECT 1"); err == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s did not accept connections after the restart", service)
		case <-time.After(time.Second):
		}
	}
}

// Disable turns the archiving of service off (ARCHITECTURE 10.9): the
// settings are reset and the database restarted before a deployment
// removes the mount, so that no segment is dropped silently meanwhile.
func (s *Service) Disable(ctx context.Context, app store.App, service string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.opts.Engine.WithAppLock(ctx, app.ID, func(ctx context.Context) error {
		if app.DesiredState != store.DesiredRunning || app.SleepingSince != nil {
			return nil
		}
		for _, name := range []string{"archive_mode", "archive_command", "archive_timeout"} {
			if _, err := s.sql(ctx, app, service, "ALTER SYSTEM RESET "+name); err != nil {
				return err
			}
		}
		if err := s.restart(ctx, app, service); err != nil {
			return err
		}
		_, _ = s.run(ctx, append([]string{"compose", "-p", app.ProjectName(), "exec", "-T", "-u", "root", service},
			"sh", "-c", `rm -rf "$1"/* "$1"/.[!.]* 2>/dev/null; true`, "sh", compose.WALMountTarget))
		return nil
	})
	if derr := s.opts.Store.DeletePITRState(ctx, app.ID); derr != nil && err == nil {
		err = derr
	}
	return err
}

// --- base backups -------------------------------------------------------

// base takes a base backup of the app's service.
func (s *Service) base(ctx context.Context, app store.App) error {
	if s.opts.Repo == nil {
		return errors.New("backups are not configured")
	}
	service := app.PITRService
	return s.opts.Engine.WithServiceRunning(ctx, app.ID, service, func(ctx context.Context) error {
		timeline, err := s.sql(ctx, app, service, "SELECT timeline_id FROM pg_control_checkpoint()")
		if err != nil {
			return err
		}
		tl, err := strconv.Atoi(timeline)
		if err != nil {
			return fmt.Errorf("timeline %q: %w", timeline, err)
		}
		// The segment in use now: the backup starts in it or later, and
		// recovery needs it and every later one.
		segment, err := s.sql(ctx, app, service, "SELECT pg_walfile_name(pg_current_wal_lsn())")
		if err != nil {
			return err
		}
		version, err := s.serverVersion(ctx, app, service)
		if err != nil {
			return err
		}
		started := s.opts.Clock.Now()
		id, err := s.opts.Repo.BackupStream(ctx, backup.Stream{
			App: s.backupApp(app), Kind: backup.KindPITRBase, Target: service, FileName: "base.tar",
			Args: execArgs(app, service, "sh", "-c", baseBackupScript),
		})
		if err != nil {
			return err
		}
		_, err = s.opts.Store.AddPITRBase(ctx, store.PITRBase{
			AppID: app.ID, Service: service, SnapshotID: id, StartedAt: started, FinishedAt: s.opts.Clock.Now(),
			StartSegment: segment, Timeline: tl, ServerVersionNum: version,
		})
		if err == nil {
			s.opts.Logger.Info("base backup stored", "app", app.Slug, "snapshot", id)
		}
		return err
	})
}

// --- WAL ----------------------------------------------------------------

// walName matches what PostgreSQL archives: segments, timeline history
// files and backup history files. The temporary files of ArchiveCommand
// end in .tmp and are left alone.
var walName = regexp.MustCompile(`^([0-9A-F]{24}(\.partial|\.[0-9A-F]{8}\.backup)?|[0-9A-F]{8}\.history)$`)

type walFile struct {
	name     string
	size     int64
	modified time.Time
}

// listScript prints "<mtime> <size> <name>" for every file of the archive
// directory.
const listScript = `cd "$1" && for f in *; do [ -f "$f" ] && stat -c '%Y %s %n' -- "$f"; done; true`

func (s *Service) listWAL(ctx context.Context, app store.App, service string) ([]walFile, error) {
	out, err := s.sh(ctx, app, service, listScript, compose.WALMountTarget)
	if err != nil {
		return nil, fmt.Errorf("list the archive directory: %w", err)
	}
	var files []walFile
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !walName.MatchString(fields[2]) {
			continue
		}
		mtime, err1 := strconv.ParseInt(fields[0], 10, 64)
		size, err2 := strconv.ParseInt(fields[1], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		files = append(files, walFile{name: fields[2], size: size, modified: time.Unix(mtime, 0).UTC()})
	}
	slices.SortFunc(files, func(a, b walFile) int { return strings.Compare(a.name, b.name) })
	return files, nil
}

// Archive ships the WAL waiting in the app's archive directory to the
// repository and removes what was stored (ARCHITECTURE 10.9).
func (s *Service) Archive(ctx context.Context, app store.App) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.archive(ctx, app)
}

func (s *Service) archive(ctx context.Context, app store.App) error {
	service := app.PITRService
	st, err := s.opts.Store.GetPITRState(ctx, app.ID)
	if err != nil || st.Status != store.PITRReady || st.Service != service {
		return err
	}
	// A stopped or sleeping database writes no WAL.
	if app.DesiredState != store.DesiredRunning || app.SleepingSince != nil {
		return nil
	}
	now := s.opts.Clock.Now()
	if _, err := s.sh(ctx, app, service, `test -d "$1"`, compose.WALMountTarget); err != nil {
		st.Warning = "The running release does not mount the archive directory: nothing is archived until the app is deployed again."
		st.UpdatedAt = now
		if err := s.opts.Store.OpenPITRGap(ctx, app.ID, service, store.PITRGapMountMissing, now); err != nil {
			return err
		}
		return s.opts.Store.PutPITRState(ctx, st)
	}
	if closed, err := s.opts.Store.ClosePITRGaps(ctx, app.ID, store.PITRGapMountMissing, now); err != nil {
		return err
	} else if closed {
		st.Warning = ""
		s.wantBase(app.ID)
	}
	if version, err := s.serverVersion(ctx, app, service); err == nil && version/10000 != st.ServerVersionNum/10000 {
		if err := s.versionChanged(ctx, app, st.ServerVersionNum, version); err != nil {
			return err
		}
		st.ServerVersionNum = version
		s.wantBase(app.ID)
	}
	if out, err := s.sql(ctx, app, service, "SELECT failed_count FROM pg_stat_archiver"); err == nil {
		if failed, perr := strconv.ParseInt(out, 10, 64); perr == nil {
			if failed > st.ArchiverFailed {
				st.Warning = fmt.Sprintf("PostgreSQL failed to archive WAL %d times (pg_stat_archiver); see the logs of %s.", failed, service)
			}
			st.ArchiverFailed = failed
		}
	}

	files, err := s.listWAL(ctx, app, service)
	if err != nil {
		return err
	}
	st.UpdatedAt = now
	if len(files) == 0 {
		return s.opts.Store.PutPITRState(ctx, st)
	}
	names := make([]string, len(files))
	var newest time.Time
	for i, f := range files {
		names[i] = f.name
		if f.modified.After(newest) {
			newest = f.modified
		}
	}
	id, err := s.opts.Repo.BackupStream(ctx, backup.Stream{
		App: s.backupApp(app), Kind: backup.KindWAL, Target: service,
		FileName: fmt.Sprintf("wal-%s-%s.tar", files[0].name, files[len(files)-1].name),
		Args:     execArgs(app, service, append([]string{"tar", "-c", "-C", compose.WALMountTarget}, names...)...),
	})
	if err != nil {
		if !errors.Is(err, backup.ErrBusy) {
			if derr := s.dropIfFull(ctx, app, files); derr != nil {
				s.opts.Logger.Error("dropping WAL kept on the host failed", "app", app.Slug, "error", derr)
			}
		}
		_ = s.opts.Store.PutPITRState(ctx, st)
		return err
	}
	if err := s.opts.Store.AddPITRWALBatch(ctx, store.PITRWALBatch{
		AppID: app.ID, Service: service, SnapshotID: id,
		FirstSegment: files[0].name, LastSegment: files[len(files)-1].name, FileCount: len(files),
		LastModifiedAt: newest, CreatedAt: now,
	}); err != nil {
		return err
	}
	if err := s.remove(ctx, app, service, names); err != nil {
		return err
	}
	return s.opts.Store.PutPITRState(ctx, st)
}

func (s *Service) remove(ctx context.Context, app store.App, service string, names []string) error {
	_, err := s.sh(ctx, app, service, `cd "$1" && shift && rm -f "$@"`, append([]string{compose.WALMountTarget}, names...)...)
	if err != nil {
		return fmt.Errorf("remove archived WAL: %w", err)
	}
	return nil
}

// dropIfFull drops the oldest WAL waiting on the host when it outgrows its
// bound or the disk fills up, records the period as one that cannot be
// recovered to, and asks for a new base backup (ARCHITECTURE 10.9).
func (s *Service) dropIfFull(ctx context.Context, app store.App, files []walFile) error {
	var total int64
	for _, f := range files {
		total += f.size
	}
	full := s.opts.DiskFull != nil && s.opts.DiskFull()
	if total <= s.opts.MaxLocalBytes && !full {
		return nil
	}
	limit := s.opts.MaxLocalBytes / 2
	if full {
		limit = 0
	}
	var drop []string
	var from, to time.Time
	for _, f := range files {
		if total <= limit {
			break
		}
		drop = append(drop, f.name)
		total -= f.size
		if from.IsZero() || f.modified.Before(from) {
			from = f.modified
		}
		if f.modified.After(to) {
			to = f.modified
		}
	}
	if len(drop) == 0 {
		return nil
	}
	s.opts.Logger.Warn("dropping WAL that could not be shipped", "app", app.Slug, "files", len(drop))
	if err := s.remove(ctx, app, app.PITRService, drop); err != nil {
		return err
	}
	if err := s.opts.Store.OpenPITRGap(ctx, app.ID, app.PITRService, store.PITRGapDropped, from); err != nil {
		return err
	}
	if _, err := s.opts.Store.ClosePITRGaps(ctx, app.ID, store.PITRGapDropped, to); err != nil {
		return err
	}
	s.wantBase(app.ID)
	return nil
}

// --- activity -----------------------------------------------------------

func (s *Service) record(ctx context.Context, app store.App, action string, details map[string]any) {
	if _, err := s.opts.Store.RecordActivity(ctx, app.ID, action, details, s.opts.Clock.Now()); err != nil {
		s.opts.Logger.Error("recording activity failed", "app", app.Slug, "action", action, "error", err)
	}
}
