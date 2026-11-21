package api

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// BackupSnapshot is one restic snapshot of an app.
type BackupSnapshot struct {
	ID     string    `json:"id"`
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`   // volume | dump
	Target string    `json:"target"` // volume name or dump hook service
	// SizeBytes is the amount of data restic processed, when it recorded it.
	SizeBytes *int64 `json:"size_bytes"`
}

var (
	// ErrBackupBusy is returned when a backup or restore is already running.
	ErrBackupBusy = errors.New("a backup is already running")
	// ErrSnapshotNotFound is returned for snapshots that do not exist, do
	// not belong to the app, or are of the wrong kind for the operation.
	ErrSnapshotNotFound = errors.New("snapshot not found")
)

// Backups runs and inspects backups.
type Backups interface {
	// Running reports whether a run is in progress.
	Running() bool
	// NextRun is when the next scheduled backup starts.
	NextRun() time.Time
	// StartManual begins a full backup in the background and returns its record.
	StartManual(ctx context.Context) (store.BackupRun, error)
	Snapshots(ctx context.Context, app store.App) ([]BackupSnapshot, error)
	// RestoreVolume restores a volume snapshot in place (the app is stopped
	// and started around it).
	RestoreVolume(ctx context.Context, app store.App, snapshotID string) error
	// OpenDump prepares the download of a dump snapshot.
	OpenDump(ctx context.Context, app store.App, snapshotID string) (filename string, write func(io.Writer) error, err error)
}

// backupWarningAfter is how long without a successful backup raises the warning.
const backupWarningAfter = 24 * time.Hour

var (
	snapshotIDPattern = regexp.MustCompile(`^[0-9a-f]{8,64}$`)
	logServicePattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
)

type backupRunJSON struct {
	ID         int64      `json:"id"`
	Trigger    string     `json:"trigger"`
	Status     string     `json:"status"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Error      string     `json:"error"`
}

func backupRunToJSON(r store.BackupRun) backupRunJSON {
	return backupRunJSON{ID: r.ID, Trigger: r.Trigger, Status: r.Status,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, Error: r.Error}
}

type backupTargetJSON struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

// appBackupRunJSON is the latest run that concerns one app, reduced to that
// app's targets.
type appBackupRunJSON struct {
	ID         int64      `json:"id"`
	Trigger    string     `json:"trigger"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	// Status is the app's: failed when one of its targets failed or the run
	// failed before backing anything up, warning when one was stored with
	// unreadable files.
	Status string `json:"status"`
	// WholeRun is true when the run failed before backing anything up; it
	// concerns every app.
	WholeRun bool `json:"whole_run"`
	// Error is the run's message when WholeRun; only the owner gets it.
	Error   string             `json:"error"`
	Targets []backupTargetJSON `json:"targets"`
}

// appBackupStatus reduces an outcome to what the app's screens show.
func appBackupStatus(r *http.Request, o *store.BackupOutcome) *appBackupRunJSON {
	if o == nil {
		return nil
	}
	out := &appBackupRunJSON{ID: o.Run.ID, Trigger: o.Run.Trigger, StartedAt: o.Run.StartedAt,
		FinishedAt: o.Run.FinishedAt, Status: store.BackupTargetSucceeded, Targets: []backupTargetJSON{}}
	if len(o.Targets) == 0 && o.Run.Status == store.BackupFailed {
		out.Status, out.WholeRun = store.BackupTargetFailed, true
		// The run's message may name other apps; members get the fact only.
		if isOwner(r) {
			out.Error = o.Run.Error
		}
	}
	for _, t := range o.Targets {
		out.Targets = append(out.Targets, backupTargetJSON{Kind: t.Kind, Target: t.Target, Status: t.Status, Error: t.Error})
		switch {
		case t.Status == store.BackupTargetFailed:
			out.Status = store.BackupTargetFailed
		case t.Status == store.BackupTargetWarning && out.Status != store.BackupTargetFailed:
			out.Status = store.BackupTargetWarning
		}
	}
	return out
}

// handleAppBackupStatus reports how the backups of one app are doing: the
// latest run that concerns it, with its own targets only, and when all of
// them were last stored (ARCHITECTURE 10.2).
func (s *Server) handleAppBackupStatus(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	outcome, err := s.store.LatestAppBackupOutcome(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	last, err := s.store.LastAppBackupSuccess(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	configured := s.backups != nil
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":      configured,
		"last_success_at": last,
		// Only once the app has been part of a run: a new app has nothing
		// to be late with.
		"warning":  configured && outcome != nil && (last == nil || s.clock.Now().Sub(*last) > backupWarningAfter),
		"last_run": appBackupStatus(r, outcome),
	})
}

type backupProblemJSON struct {
	App      string `json:"app"`
	Status   string `json:"status"` // failed | warning
	WholeRun bool   `json:"whole_run"`
}

// backupProblems lists the apps the user can see whose latest run failed or
// stored a volume with unreadable files.
func (s *Server) backupProblems(r *http.Request) ([]backupProblemJSON, error) {
	apps, err := s.store.ListApps(r.Context())
	if err != nil {
		return nil, err
	}
	var roles map[int64]string
	if a, _ := actorFrom(r); !a.Owner {
		if roles, err = s.store.RolesOf(r.Context(), a.UserID); err != nil {
			return nil, err
		}
	}
	out := []backupProblemJSON{}
	for _, app := range apps {
		if roles != nil && roles[app.ID] == "" {
			continue
		}
		outcome, err := s.store.LatestAppBackupOutcome(r.Context(), app.ID)
		if err != nil {
			return nil, err
		}
		if st := appBackupStatus(r, outcome); st != nil && st.Status != store.BackupTargetSucceeded {
			out = append(out, backupProblemJSON{App: app.Slug, Status: st.Status, WholeRun: st.WholeRun})
		}
	}
	return out, nil
}

// handleBackupStatus reports the overall state. It answers even when
// backups are not configured so the system screen can say so.
func (s *Server) handleBackupStatus(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListBackupRuns(r.Context(), 20)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	last, err := s.store.LastSuccessfulBackup(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]backupRunJSON, 0, len(runs))
	// The run history is the host's, and its errors may name other apps;
	// members see only the overall state.
	for _, run := range runs {
		if !isOwner(r) {
			break
		}
		out = append(out, backupRunToJSON(run))
	}
	configured := s.backups != nil
	problems := []backupProblemJSON{}
	systemFailed := false
	if configured {
		if problems, err = s.backupProblems(r); err != nil {
			s.internalError(w, r, err)
			return
		}
		// The system's backup is the owner's business.
		if isOwner(r) {
			sys, err := s.store.LatestSystemBackupOutcome(r.Context())
			if err != nil {
				s.internalError(w, r, err)
				return
			}
			if st := appBackupStatus(r, sys); st != nil && st.Status == store.BackupTargetFailed {
				systemFailed = true
			}
		}
	}
	var nextRun *time.Time
	if configured {
		if t := s.backups.NextRun(); !t.IsZero() {
			nextRun = &t
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"next_run_at":     nextRun,
		"configured":      configured,
		"running":         configured && s.backups.Running(),
		"last_success_at": last,
		// No success within a day (or never) is worth a visible warning.
		"warning": configured && (last == nil || s.clock.Now().Sub(*last) > backupWarningAfter),
		"runs":    out,
		// The apps whose latest backup did not fully succeed, and whether
		// the system's did not (ARCHITECTURE 10.2).
		"problems":      problems,
		"system_failed": systemFailed,
	})
}

func (s *Server) backupService(w http.ResponseWriter) (Backups, bool) {
	if s.backups == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "backups are not configured")
		return nil, false
	}
	return s.backups, true
}

func (s *Server) handleStartBackup(w http.ResponseWriter, r *http.Request) {
	backups, ok := s.backupService(w)
	if !ok {
		return
	}
	run, err := backups.StartManual(r.Context())
	if errors.Is(err, ErrBackupBusy) {
		writeError(w, http.StatusConflict, "busy", "a backup is already running")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, backupRunToJSON(run))
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	backups, ok := s.backupService(w)
	if !ok {
		return
	}
	snapshots, err := backups.Snapshots(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if snapshots == nil {
		snapshots = []BackupSnapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snapshots})
}

func (s *Server) writeBackupError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, ErrBackupBusy):
		writeError(w, http.StatusConflict, "busy", "a backup or restore is already running")
	case errors.Is(err, ErrSnapshotNotFound):
		writeError(w, http.StatusNotFound, "snapshot_not_found", "no such snapshot for this app")
	default:
		if !s.writeOpError(w, r, err) {
			s.internalError(w, r, err)
		}
	}
}

func (s *Server) handleRestoreVolume(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok || !s.requireRecentAuth(w, r) {
		return
	}
	backups, ok := s.backupService(w)
	if !ok {
		return
	}
	var in struct {
		SnapshotID string `json:"snapshot_id"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !snapshotIDPattern.MatchString(in.SnapshotID) {
		writeValidationError(w, fieldErrors{"snapshot_id": "is not a snapshot ID"})
		return
	}
	// A restore replaces data and must not be cut short by the client
	// going away halfway through.
	ctx := context.WithoutCancel(r.Context())
	if err := backups.RestoreVolume(ctx, app, in.SnapshotID); err != nil {
		s.writeBackupError(w, r, err)
		return
	}
	s.recordActivity(r.WithContext(ctx), app.ID, actVolumeRestored, map[string]any{"snapshot_id": in.SnapshotID})
	writeJSON(w, http.StatusOK, map[string]any{"restored": true})
}

func (s *Server) handleDownloadDump(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok || !s.requireRecentAuth(w, r) {
		return
	}
	backups, ok := s.backupService(w)
	if !ok {
		return
	}
	id := r.PathValue("snapshot")
	if !snapshotIDPattern.MatchString(id) {
		writeError(w, http.StatusNotFound, "snapshot_not_found", "no such snapshot for this app")
		return
	}
	filename, write, err := backups.OpenDump(r.Context(), app, id)
	if err != nil {
		s.writeBackupError(w, r, err)
		return
	}
	// Recorded when the download starts: the data is on its way out
	// whether or not the browser keeps the whole file.
	s.recordActivity(r, app.ID, actDumpDownloaded, map[string]any{"snapshot_id": id})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "no-store")
	if err := write(w); err != nil {
		// Headers are gone; all that is left is to cut the response short
		// so the browser does not keep a truncated file as complete.
		s.log.Error("streaming a dump failed", "app", app.Slug, "error", err)
		panic(http.ErrAbortHandler)
	}
}
