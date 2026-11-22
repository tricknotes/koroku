package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
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
	// IdleSleep says whether apps can sleep while idle on this host
	// (ARCHITECTURE 8.4).
	IdleSleep idleSleepStatusJSON `json:"idle_sleep"`
	// MemoryReservation totals what production apps reserve (ARCHITECTURE
	// 6.2); nil for members, as it names other apps.
	MemoryReservation *memoryReservationJSON `json:"memory_reservation"`
}

type idleSleepStatusJSON struct {
	// Available is false without per-app access logs: requests cannot
	// be counted then.
	Available bool `json:"available"`
}

// idleSleepAvailable reports whether requests to the apps can be counted:
// Caddy routes them and writes a per-app access log (ARCHITECTURE 8.4).
func (s *Server) idleSleepAvailable() bool {
	return s.cfg.Caddy.Enabled && s.cfg.Caddy.AccessLogDir != ""
}

// reservationMarginBytes is left free on top of the reservations for the
// host itself, builds and staging apps before the system screen warns.
const reservationMarginBytes = 1 << 30

type memoryReservationJSON struct {
	TotalBytes  int64 `json:"total_bytes"`
	MarginBytes int64 `json:"margin_bytes"`
	// Warning is set when the reservations plus the margin exceed the
	// host's memory.
	Warning bool                 `json:"warning"`
	Apps    []appReservationJSON `json:"apps"`
}

type appReservationJSON struct {
	Slug        string           `json:"slug"`
	Environment string           `json:"environment"`
	TotalBytes  int64            `json:"total_bytes"`
	Services    map[string]int64 `json:"services"`
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
		IdleSleep: idleSleepStatusJSON{Available: s.idleSleepAvailable()},
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
	} else {
		reservation, err := s.memoryReservation(r.Context(), resp.Memory)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		resp.MemoryReservation = &reservation
	}
	writeJSON(w, http.StatusOK, resp)
}

// memoryReservation sums what the running releases of the apps that are
// not stopped reserve. An app waiting to be started again reserves the
// same once it runs, so only a stopped app is left out.
func (s *Server) memoryReservation(ctx context.Context, host *system.Memory) (memoryReservationJSON, error) {
	out := memoryReservationJSON{MarginBytes: reservationMarginBytes, Apps: []appReservationJSON{}}
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return out, err
	}
	var ids []int64
	for _, app := range apps {
		if app.CurrentDeploymentID != nil && app.DesiredState != store.DesiredStopped {
			ids = append(ids, *app.CurrentDeploymentID)
		}
	}
	recorded, err := s.store.DeploymentMemoryReservations(ctx, ids...)
	if err != nil {
		return out, err
	}
	for _, app := range apps {
		if app.CurrentDeploymentID == nil || app.DesiredState == store.DesiredStopped {
			continue
		}
		services := recorded[*app.CurrentDeploymentID]
		var total int64
		for _, bytes := range services {
			total += bytes
		}
		if total == 0 {
			continue
		}
		out.TotalBytes += total
		out.Apps = append(out.Apps, appReservationJSON{
			Slug: app.Slug, Environment: app.Environment, TotalBytes: total, Services: services,
		})
	}
	slices.SortStableFunc(out.Apps, func(a, b appReservationJSON) int {
		return cmp.Or(cmp.Compare(b.TotalBytes, a.TotalBytes), cmp.Compare(a.Slug, b.Slug))
	})
	if host != nil && host.TotalBytes > 0 {
		out.Warning = uint64(out.TotalBytes+out.MarginBytes) > host.TotalBytes
	}
	return out, nil
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
