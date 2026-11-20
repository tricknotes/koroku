package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// DBCopier copies a database into a staging app (ARCHITECTURE 10.5).
type DBCopier interface {
	StartCopy(ctx context.Context, req deploy.CopyRequest) (store.DBCopy, error)
	CopyLogPath(slug string, id int64) string
}

type dbCopyJSON struct {
	ID               int64      `json:"id"`
	TargetApp        string     `json:"target_app"`
	SourceApp        string     `json:"source_app"`
	SourceKind       string     `json:"source_kind"`
	Status           string     `json:"status"`
	Error            string     `json:"error"`
	Bytes            int64      `json:"bytes"`
	BackupSnapshotID string     `json:"backup_snapshot_id"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at"`
}

func dbCopyToJSON(c store.DBCopy, target string) dbCopyJSON {
	return dbCopyJSON{
		ID: c.ID, TargetApp: target, SourceApp: c.SourceSlug, SourceKind: c.SourceKind,
		Status: c.Status, Error: c.Error, Bytes: c.Bytes, BackupSnapshotID: c.BackupSnapshotID,
		StartedAt: c.StartedAt, FinishedAt: c.FinishedAt,
	}
}

func (s *Server) handleListDBCopies(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 100")
			return
		}
		limit = n
	}
	copies, err := s.store.ListDBCopies(r.Context(), app.ID, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]dbCopyJSON, 0, len(copies))
	for _, c := range copies {
		out = append(out, dbCopyToJSON(c, app.Slug))
	}
	writeJSON(w, http.StatusOK, map[string]any{"copies": out})
}

func (s *Server) handleStartDBCopy(w http.ResponseWriter, r *http.Request) {
	target, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	var in struct {
		Source     string `json:"source"`
		SourceKind string `json:"source_kind"`
		Confirm    string `json:"confirm"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.SourceKind == "" {
		in.SourceKind = store.CopySourceLive
	}
	errs := fieldErrors{}
	if in.SourceKind != store.CopySourceLive && in.SourceKind != store.CopySourceSnapshot {
		errs.add("source_kind", "must be live or snapshot")
	}
	if strings.TrimSpace(in.Source) == "" {
		errs.add("source", "is required")
	} else if in.Source == target.Slug {
		errs.add("source", "must be another app")
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	// The cheap refusals come before the login check, so that the console
	// can say what is wrong without sending the user through a login.
	if target.Environment != store.EnvironmentStaging {
		writeError(w, http.StatusUnprocessableEntity, "target_not_staging", "data can only be copied into a staging app")
		return
	}
	if in.Confirm != target.Slug {
		writeError(w, http.StatusUnprocessableEntity, "confirmation_mismatch", "type the slug of the target app to confirm")
		return
	}
	if !s.requireRecentAuth(w, r) {
		return
	}
	source, err := s.store.GetApp(r.Context(), in.Source)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "source_not_found", "no such source app")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if s.copier == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "database copies are not available")
		return
	}
	c, err := s.copier.StartCopy(r.Context(), deploy.CopyRequest{Source: source, Target: target, Kind: in.SourceKind})
	if s.writeCopyError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, dbCopyToJSON(c, target.Slug))
}

// writeCopyError maps the refusals of StartCopy; it reports whether one
// was written.
func (s *Server) writeCopyError(w http.ResponseWriter, r *http.Request, err error) bool {
	type refusal struct {
		status  int
		code    string
		message string
	}
	refusals := map[error]refusal{
		deploy.ErrBusy:                 {http.StatusConflict, "busy", "the target or the source app is being deployed or another operation is in progress"},
		deploy.ErrNotDeployed:          {http.StatusConflict, "not_deployed", "the target app has not been deployed yet"},
		deploy.ErrTargetNotRunning:     {http.StatusConflict, "target_not_running", "the target app is stopped"},
		deploy.ErrSourceNotRunning:     {http.StatusConflict, "source_not_running", "the source app is not running"},
		deploy.ErrNoDumpSnapshot:       {http.StatusConflict, "no_dump_snapshot", "the source app has no dump snapshot"},
		deploy.ErrTargetNotStaging:     {http.StatusUnprocessableEntity, "target_not_staging", "data can only be copied into a staging app"},
		deploy.ErrSameApp:              {http.StatusUnprocessableEntity, "validation_failed", "the source must be another app"},
		deploy.ErrNoDumpHook:           {http.StatusUnprocessableEntity, "source_dump_hook_missing", "the source app has no dump hook"},
		deploy.ErrNoLoadHook:           {http.StatusUnprocessableEntity, "target_load_hook_missing", "the target app has no load hook"},
		deploy.ErrBackupsNotConfigured: {http.StatusServiceUnavailable, "not_configured", "backups are not configured"},
	}
	if err == nil {
		return false
	}
	for target, ref := range refusals {
		if errors.Is(err, target) {
			writeError(w, ref.status, ref.code, ref.message)
			return true
		}
	}
	s.internalError(w, r, err)
	return true
}

// loadDBCopy resolves the {id} path value together with its target app.
func (s *Server) loadDBCopy(w http.ResponseWriter, r *http.Request) (store.DBCopy, store.App, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusNotFound, "copy_not_found", "no such database copy")
		return store.DBCopy{}, store.App{}, false
	}
	c, err := s.store.GetDBCopy(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "copy_not_found", "no such database copy")
		return store.DBCopy{}, store.App{}, false
	}
	if err != nil {
		s.internalError(w, r, err)
		return store.DBCopy{}, store.App{}, false
	}
	app, err := s.store.GetAppByID(r.Context(), c.TargetAppID)
	if err != nil {
		s.internalError(w, r, err)
		return store.DBCopy{}, store.App{}, false
	}
	return c, app, true
}

func (s *Server) handleGetDBCopy(w http.ResponseWriter, r *http.Request) {
	c, app, ok := s.loadDBCopy(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, dbCopyToJSON(c, app.Slug))
}

// handleDBCopyLogs streams the log of a copy, like handleDeploymentLogs.
func (s *Server) handleDBCopyLogs(w http.ResponseWriter, r *http.Request) {
	c, app, ok := s.loadDBCopy(w, r)
	if !ok {
		return
	}
	if s.copier == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "database copies are not available")
		return
	}
	serveJobLog(w, r, s.copier.CopyLogPath(app.Slug, c.ID), func(ctx context.Context) (string, bool, error) {
		current, err := s.store.GetDBCopy(ctx, c.ID)
		return current.Status, current.Status != store.CopyRunning, err
	})
}
