package api

import (
	"context"
	"net/http"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/system"
)

// RouteStatus reports the last attempt to load routes into the proxy.
type RouteStatus struct {
	LastLoadedAt *time.Time
	LastError    string
}

// LogArchiveStatus reports the state of the log archiver.
type LogArchiveStatus struct {
	Configured    bool       `json:"configured"`
	LastSuccessAt *time.Time `json:"last_success_at"`
	LastError     string     `json:"last_error"`
	// NextRunAt is when the next scheduled archive run starts; nil when
	// archiving is not configured.
	NextRunAt *time.Time `json:"next_run_at"`
}

// HostInspector reports Docker disk usage.
type HostInspector interface {
	DockerDiskUsage(ctx context.Context) ([]deploy.DiskUsageItem, error)
}

type caddyStatusJSON struct {
	Enabled      bool       `json:"enabled"`
	LastLoadedAt *time.Time `json:"last_loaded_at"`
	LastError    string     `json:"last_error"`
}

type systemResponse struct {
	Version    string `json:"version"`
	BaseDomain string `json:"base_domain"`
	system.Info
	DiskLimitPercent int              `json:"disk_limit_percent"`
	LogArchive       LogArchiveStatus `json:"log_archive"`
	Caddy            caddyStatusJSON  `json:"caddy"`
	// Webhook is where the GitHub App delivers pushes for automatic
	// deployment (ARCHITECTURE 5.2).
	Webhook webhookStatusJSON `json:"webhook"`
}

type webhookStatusJSON struct {
	URL string `json:"url"`
	// SecretConfigured says whether [github] webhook_secret is set; without
	// it every delivery is refused.
	SecretConfigured bool `json:"secret_configured"`
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request) {
	resp := systemResponse{
		Version:          s.version,
		BaseDomain:       s.cfg.BaseDomain,
		Info:             s.system.Info(r.Context()),
		DiskLimitPercent: s.cfg.Deploy.DiskUsageLimitPercent,
		Caddy:            caddyStatusJSON{Enabled: s.cfg.Caddy.Enabled},
		Webhook: webhookStatusJSON{
			URL:              s.cfg.ConsoleURL + WebhookPath,
			SecretConfigured: s.cfg.GitHub.WebhookSecret != "",
		},
	}
	if s.logs != nil {
		resp.LogArchive = s.logs.ArchiveStatus()
	}
	if s.routeStatus != nil {
		status := s.routeStatus()
		resp.Caddy.LastLoadedAt, resp.Caddy.LastError = status.LastLoadedAt, status.LastError
	}
	if !isOwner(r) {
		// The error texts may name other apps' hosts and objects.
		resp.LogArchive.LastError, resp.Caddy.LastError = "", ""
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDockerUsage(w http.ResponseWriter, r *http.Request) {
	items, err := s.ops.DockerDiskUsage(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if items == nil {
		items = []deploy.DiskUsageItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) handleContainerResources(w http.ResponseWriter, r *http.Request) {
	resources, err := s.ops.ContainerResources(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if resources.Groups == nil {
		resources.Groups = []deploy.ResourceGroup{}
	}
	writeJSON(w, http.StatusOK, resources)
}
