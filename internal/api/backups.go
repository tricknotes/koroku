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
