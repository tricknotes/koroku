package api

import (
	"net/http"

	"github.com/tricknotes/koroku/internal/system"
)

type systemResponse struct {
	Version    string `json:"version"`
	BaseDomain string `json:"base_domain"`
	system.Info
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, systemResponse{
		Version:    s.version,
		BaseDomain: s.cfg.BaseDomain,
		Info:       s.system.Info(r.Context()),
	})
}
