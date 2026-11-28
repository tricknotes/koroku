package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/pitr"
	"github.com/tricknotes/koroku/internal/store"
)

// PITR is the point-in-time recovery of the apps (ARCHITECTURE 10.9).
type PITR interface {
	Status(ctx context.Context, app store.App) (pitr.Status, error)
	// Disable turns the archiving of service off.
	Disable(ctx context.Context, app store.App, service string) error
}

type pitrGapJSON struct {
	From   time.Time  `json:"from"`
	To     *time.Time `json:"to"`
	Reason string     `json:"reason"`
}

type pitrStatusJSON struct {
	Service       string        `json:"service"`
	State         string        `json:"state"`
	Error         string        `json:"error"`
	Warning       string        `json:"warning"`
	EarliestAt    *time.Time    `json:"earliest_at"`
	ArchivedUntil *time.Time    `json:"archived_until"`
	Gaps          []pitrGapJSON `json:"gaps"`
}

func (s *Server) handlePITRStatus(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	out := pitrStatusJSON{Service: app.PITRService, State: pitr.StateOff, Gaps: []pitrGapJSON{}}
	if s.pitr == nil {
		if app.PITRService != "" {
			out.State = pitr.StateUnavailable
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	st, err := s.pitr.Status(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out.State, out.Error, out.Warning = st.State, st.Error, st.Warning
	out.EarliestAt, out.ArchivedUntil = st.EarliestAt, st.ArchivedUntil
	for _, g := range st.Gaps {
		out.Gaps = append(out.Gaps, pitrGapJSON{From: g.StartedAt, To: g.EndedAt, Reason: g.Reason})
	}
	writeJSON(w, http.StatusOK, out)
}

// PITRRestorer starts restores to a point in time (ARCHITECTURE 10.9).
type PITRRestorer interface {
	StartPITRRestore(ctx context.Context, app store.App, target time.Time) (store.PITRRestore, error)
	PITRRestoreLogPath(slug string, id int64) string
}

type pitrRestoreJSON struct {
	ID             int64      `json:"id"`
	Service        string     `json:"service"`
	TargetTime     time.Time  `json:"target_time"`
	BaseSnapshotID string     `json:"base_snapshot_id"`
	TrashPath      string     `json:"trash_path"`
	Status         string     `json:"status"`
	Error          string     `json:"error"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at"`
	Actor          *actorJSON `json:"actor"`
}

func pitrRestoreToJSON(r store.PITRRestore) pitrRestoreJSON {
	return pitrRestoreJSON{ID: r.ID, Service: r.Service, TargetTime: r.TargetTime, BaseSnapshotID: r.BaseSnapshotID,
		TrashPath: r.TrashPath, Status: r.Status, Error: r.Error, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		Actor: actorToJSON(r.Actor)}
}

func (s *Server) handleListPITRRestores(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	list, err := s.store.ListPITRRestores(r.Context(), app.ID, 20)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]pitrRestoreJSON, 0, len(list))
	for _, item := range list {
		out = append(out, pitrRestoreToJSON(item))
	}
	writeJSON(w, http.StatusOK, map[string]any{"restores": out})
}

// loadPITRRestore resolves {id} among the restores of the app.
func (s *Server) loadPITRRestore(w http.ResponseWriter, r *http.Request) (store.PITRRestore, store.App, bool) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return store.PITRRestore{}, store.App{}, false
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var rec store.PITRRestore
	if err == nil {
		rec, err = s.store.GetPITRRestore(r.Context(), id)
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) && !errors.Is(err, strconv.ErrSyntax) {
		s.internalError(w, r, err)
		return store.PITRRestore{}, store.App{}, false
	}
	if err != nil || rec.AppID != app.ID {
		writeError(w, http.StatusNotFound, "pitr_restore_not_found", "no such restore of this app")
		return store.PITRRestore{}, store.App{}, false
	}
	return rec, app, true
}

func (s *Server) handleGetPITRRestore(w http.ResponseWriter, r *http.Request) {
	rec, _, ok := s.loadPITRRestore(w, r)
	if ok {
		writeJSON(w, http.StatusOK, pitrRestoreToJSON(rec))
	}
}

func (s *Server) handlePITRRestoreLogs(w http.ResponseWriter, r *http.Request) {
	rec, app, ok := s.loadPITRRestore(w, r)
	if !ok {
		return
	}
	if s.pitrRestorer == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "point-in-time restores are not available")
		return
	}
	s.serveJobLog(w, r, s.pitrRestorer.PITRRestoreLogPath(app.Slug, rec.ID), func(ctx context.Context) (string, bool, error) {
		current, err := s.store.GetPITRRestore(ctx, rec.ID)
		return current.Status, current.Status != store.CopyRunning, err
	})
}

func (s *Server) handleStartPITRRestore(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	var in struct {
		TargetTime time.Time `json:"target_time"`
		Confirm    string    `json:"confirm"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.TargetTime.IsZero() {
		writeValidationError(w, fieldErrors{"target_time": "is required (RFC 3339)"})
		return
	}
	if app.Environment == store.EnvironmentProduction && in.Confirm != app.Slug {
		writeError(w, http.StatusUnprocessableEntity, "confirmation_mismatch", "type the slug of the app to confirm")
		return
	}
	if !s.requireRecentAuth(w, r) {
		return
	}
	if s.pitrRestorer == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "point-in-time restores are not available")
		return
	}
	rec, err := s.pitrRestorer.StartPITRRestore(r.Context(), app, in.TargetTime)
	refusals := map[error]struct {
		status        int
		code, message string
	}{
		deploy.ErrBusy:                 {http.StatusConflict, "busy", "the app is being deployed or another operation is in progress"},
		deploy.ErrNotDeployed:          {http.StatusConflict, "not_deployed", "the app has not been deployed yet"},
		deploy.ErrPITROff:              {http.StatusUnprocessableEntity, "pitr_off", "point-in-time recovery is not set up for this app"},
		deploy.ErrPITRNotReady:         {http.StatusConflict, "pitr_not_ready", "point-in-time recovery of this app is not ready"},
		deploy.ErrBackupsNotConfigured: {http.StatusServiceUnavailable, "not_configured", "backups are not configured"},
		pitr.ErrFutureTarget:           {http.StatusUnprocessableEntity, "target_in_future", "the time is in the future"},
		pitr.ErrNoBase:                 {http.StatusUnprocessableEntity, "target_out_of_range", "no base backup reaches back to that time"},
		pitr.ErrInGap:                  {http.StatusUnprocessableEntity, "target_in_gap", "the time falls in a period that cannot be recovered to"},
	}
	if err != nil {
		for target, ref := range refusals {
			if errors.Is(err, target) {
				writeError(w, ref.status, ref.code, ref.message)
				return
			}
		}
		s.internalError(w, r, err)
		return
	}
	s.recordActivity(r, app.ID, actPITRRestored, map[string]any{"restore_id": rec.ID, "target_time": rec.TargetTime})
	writeJSON(w, http.StatusAccepted, pitrRestoreToJSON(rec))
}
