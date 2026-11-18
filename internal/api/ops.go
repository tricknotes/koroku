package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// AppOperator runs manual operations on an app and on the host.
type AppOperator interface {
	Start(ctx context.Context, app store.App) error
	Stop(ctx context.Context, app store.App) error
	Restart(ctx context.Context, app store.App) error
	Delete(ctx context.Context, app store.App, removeVolumes bool) error
	Services(ctx context.Context, app store.App) ([]deploy.ServiceStatus, error)
	Prune(ctx context.Context, images, builder bool) (deploy.PruneResult, error)
	DockerDiskUsage(ctx context.Context) ([]deploy.DiskUsageItem, error)
}

// writeOpError maps operation errors; it reports whether one was written.
func (s *Server) writeOpError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, deploy.ErrBusy):
		writeError(w, http.StatusConflict, "busy", "the app is being deployed or another operation is in progress")
	case errors.Is(err, deploy.ErrNotDeployed):
		writeError(w, http.StatusConflict, "not_deployed", "the app has not been deployed yet")
	default:
		s.internalError(w, r, err)
	}
	return true
}

func (s *Server) handleAppOperation(op func(AppOperator, context.Context, store.App) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app, ok := s.loadApp(w, r)
		if !ok {
			return
		}
		if s.writeOpError(w, r, op(s.ops, r.Context(), app)) {
			return
		}
		updated, err := s.store.GetAppByID(r.Context(), app.ID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		j, err := s.appToJSON(r, updated)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, j)
	}
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	removeVolumes := false
	switch r.URL.Query().Get("volumes") {
	case "", "false":
	case "true":
		removeVolumes = true
	default:
		// Deleting data must never hinge on a typo.
		writeError(w, http.StatusBadRequest, "invalid_volumes", "volumes must be true or false")
		return
	}
	if s.writeOpError(w, r, s.ops.Delete(r.Context(), app, removeVolumes)) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListServices(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	services, err := s.ops.Services(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if services == nil {
		services = []deploy.ServiceStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"services": services})
}

func (s *Server) handlePrune(w http.ResponseWriter, r *http.Request) {
	in := struct {
		Images  *bool `json:"images"`
		Builder *bool `json:"builder"`
	}{}
	if r.ContentLength != 0 && !decodeJSON(w, r, &in) {
		return
	}
	images := in.Images == nil || *in.Images
	builder := in.Builder == nil || *in.Builder
	result, err := s.ops.Prune(r.Context(), images, builder)
	if err != nil {
		s.log.Error("prune failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: errorDetail{
			Code: "prune_failed", Message: "a cleanup command failed", Details: map[string]any{"output": result.Output},
		}})
		return
	}
	writeJSON(w, http.StatusOK, result)
}
