// Package routing keeps the reverse proxy in line with the database: it
// builds the Caddy configuration from apps and domains, loads it, and
// verifies custom domains.
package routing

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"sync"

	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/store"
)

// Settings are the configuration values that shape the proxy config.
type Settings struct {
	BaseDomain      string
	ConsoleHost     string
	ConsoleUpstream string
	AdminListen     string
	ACMECA          string
	ACMEEmail       string
	AccessLogDir    string
}

// Router regenerates the whole Caddy configuration from the database and
// loads it. It never applies partial updates.
type Router struct {
	Store    *store.Store
	Loader   caddy.Loader
	Settings Settings
	Logger   *slog.Logger

	mu     sync.Mutex
	loaded bool
	last   [sha256.Size]byte
}

// Build renders the Caddy configuration for the current database content.
func (r *Router) Build(ctx context.Context) ([]byte, error) {
	apps, err := r.Store.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	active, err := r.Store.ListActiveDomains(ctx)
	if err != nil {
		return nil, err
	}
	byApp := map[int64][]store.Domain{}
	for _, d := range active {
		byApp[d.AppID] = append(byApp[d.AppID], d)
	}

	in := caddy.Input{
		ConsoleHost:     r.Settings.ConsoleHost,
		ConsoleUpstream: r.Settings.ConsoleUpstream,
		AdminListen:     r.Settings.AdminListen,
		ACMECA:          r.Settings.ACMECA,
		ACMEEmail:       r.Settings.ACMEEmail,
		AccessLogDir:    r.Settings.AccessLogDir,
	}
	for _, app := range apps {
		route := caddy.App{
			Slug:      app.Slug,
			HostPort:  app.HostPort,
			Hostnames: []string{DefaultHostname(app.Slug, r.Settings.BaseDomain)},
			// An app that was never deployed has nothing to proxy to; it
			// gets the same fixed 503 page as a stopped one.
			Stopped: app.DesiredState != store.DesiredRunning || app.CurrentDeploymentID == nil,
		}
		for _, d := range byApp[app.ID] {
			route.Hostnames = append(route.Hostnames, d.Hostname)
			if d.IsPrimary {
				route.Primary = d.Hostname
			}
		}
		in.Apps = append(in.Apps, route)
	}
	config, err := caddy.Generate(in)
	if err != nil {
		return nil, fmt.Errorf("generate caddy config: %w", err)
	}
	return config, nil
}

// DefaultHostname is the automatically assigned hostname of an app.
func DefaultHostname(slug, baseDomain string) string { return slug + "." + baseDomain }

// Sync loads the configuration when it differs from the last one loaded by
// this process. Ordinary redeploys therefore do not reload the proxy.
func (r *Router) Sync(ctx context.Context) error { return r.sync(ctx, false) }

// ForceSync loads the configuration even when it looks unchanged. It runs at
// startup, when what Caddy currently holds is unknown.
func (r *Router) ForceSync(ctx context.Context) error { return r.sync(ctx, true) }

func (r *Router) sync(ctx context.Context, force bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	config, err := r.Build(ctx)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(config)
	if !force && r.loaded && sum == r.last {
		return nil
	}
	if err := r.Loader.Load(ctx, config); err != nil {
		// Whatever Caddy holds now is unknown; load again next time.
		r.loaded = false
		return fmt.Errorf("load caddy config: %w", err)
	}
	r.loaded, r.last = true, sum
	if r.Logger != nil {
		r.Logger.Info("caddy configuration loaded", "bytes", len(config))
	}
	return nil
}
