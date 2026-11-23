package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/tricknotes/koroku/internal/api"
	"github.com/tricknotes/koroku/internal/logs"
	"github.com/tricknotes/koroku/internal/objstore"
	"github.com/tricknotes/koroku/internal/store"
)

// archiveURLLifetime is how long a presigned download link stays valid.
const archiveURLLifetime = 15 * time.Minute

// appLogs adapts the log collector and the archiver to the API. The archiver
// and the object store are nil when object storage is not configured.
type appLogs struct {
	collector *logs.Collector
	archiver  *logs.Archiver
	objects   objstore.Store
}

func (a *appLogs) Subscribe(slug string) (<-chan api.LogLine, func()) {
	source, cancel := a.collector.Subscribe(slug)
	out := make(chan api.LogLine, 64)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for {
			select {
			case <-done:
				return
			case line, ok := <-source:
				if !ok {
					return
				}
				select {
				case out <- toAPILine(line):
				case <-done:
					return
				}
			}
		}
	}()
	var once bool
	return out, func() {
		if !once {
			once = true
			close(done)
			cancel()
		}
	}
}

func toAPILine(line logs.Line) api.LogLine {
	return api.LogLine{Service: line.Service, Time: line.Time, Line: line.Text}
}

func toAPILines(lines []logs.Line) []api.LogLine {
	out := make([]api.LogLine, len(lines))
	for i, line := range lines {
		out[i] = toAPILine(line)
	}
	return out
}

func (a *appLogs) Tail(slug, service string, n int) ([]api.LogLine, error) {
	lines, err := a.collector.Tail(slug, service, n)
	if err != nil {
		return nil, err
	}
	return toAPILines(lines), nil
}

func (a *appLogs) Search(ctx context.Context, slug string, q api.LogQuery) ([]api.LogLine, bool, error) {
	result, err := a.collector.Search(ctx, slug, logs.Query{
		Service: q.Service, From: q.From, To: q.To, Contains: q.Contains, Limit: q.Limit,
	})
	if err != nil {
		return nil, false, err
	}
	return toAPILines(result.Lines), result.Truncated, nil
}

func (a *appLogs) ListArchives(ctx context.Context, slug, service string) ([]api.LogArchive, error) {
	if a.archiver == nil {
		return nil, api.ErrNotConfigured
	}
	archives, err := a.archiver.List(ctx, slug, service)
	if err != nil {
		return nil, err
	}
	out := make([]api.LogArchive, 0, len(archives))
	for _, archive := range archives {
		out = append(out, api.LogArchive{Key: archive.Key, Service: archive.Service, Hour: archive.Hour, Size: archive.Size})
	}
	return out, nil
}

func (a *appLogs) ArchiveURL(_ context.Context, slug, key string) (string, error) {
	if a.objects == nil {
		return "", api.ErrNotConfigured
	}
	// Only keys under this app's log prefix may be signed: the same bucket
	// also holds the backups.
	if err := logs.ValidateArchiveKey(slug, key); err != nil {
		return "", fmt.Errorf("%w: %w", api.ErrInvalidArchiveKey, err)
	}
	return a.objects.PresignGet(key, archiveURLLifetime)
}

func (a *appLogs) ArchiveStatus() api.LogArchiveStatus {
	if a.archiver == nil {
		return api.LogArchiveStatus{}
	}
	status := a.archiver.Status()
	next := a.archiver.NextRun()
	out := api.LogArchiveStatus{Configured: true, LastError: status.LastError, NextRunAt: &next}
	if !status.LastSuccess.IsZero() {
		last := status.LastSuccess
		out.LastSuccessAt = &last
	}
	return out
}

// collectedApps lists the apps whose logs are collected: every app, with the
// access log Caddy writes for it when access logs are enabled.
func collectedApps(st *store.Store, accessLogDir string) func(ctx context.Context) ([]logs.App, error) {
	return func(ctx context.Context) ([]logs.App, error) {
		apps, err := st.ListApps(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]logs.App, 0, len(apps))
		for _, app := range apps {
			entry := logs.App{Slug: app.Slug, Project: app.ProjectName()}
			if app.UsesColors() {
				entry.PublicService = app.WebService
			}
			if accessLogDir != "" {
				entry.AccessLogPath = filepath.Join(accessLogDir, app.Slug+".log")
			}
			out = append(out, entry)
		}
		return out, nil
	}
}
