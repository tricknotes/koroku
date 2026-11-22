package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

// ComposeInspector reads an app's Compose setup for the read-only
// configuration view (ARCHITECTURE 9.10). Every value it returns is
// masked; nothing it does changes a container or a file.
type ComposeInspector interface {
	// ComposeView returns the override and merged configuration of the
	// release of deployment id, or of the current release when id is 0.
	ComposeView(ctx context.Context, app store.App, id int64) (deploy.ComposeView, error)
	// Containers lists the app's containers compared with the current
	// release.
	Containers(ctx context.Context, app store.App) (deploy.ContainerView, error)
}

func (s *Server) handleComposeView(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	if s.compose == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "the Compose view is not available")
		return
	}
	var id int64
	if raw := r.URL.Query().Get("deployment"); raw != "" {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_deployment", "deployment must be a deployment ID")
			return
		}
		id = n
	}
	view, err := s.compose.ComposeView(r.Context(), app, id)
	switch {
	case errors.Is(err, deploy.ErrNotDeployed):
		writeError(w, http.StatusConflict, "not_deployed", "the app has no deployed release whose files are kept")
	case errors.Is(err, deploy.ErrReleaseUnavailable):
		writeError(w, http.StatusConflict, "release_unavailable", "the files of this release are not kept any more")
	case err != nil:
		s.internalError(w, r, err)
	default:
		if view.Releases == nil {
			view.Releases = []deploy.ComposeRelease{}
		}
		writeJSON(w, http.StatusOK, view)
	}
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	if s.compose == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "the Compose view is not available")
		return
	}
	view, err := s.compose.Containers(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if view.Containers == nil {
		view.Containers = []deploy.ContainerInfo{}
	}
	writeJSON(w, http.StatusOK, view)
}
