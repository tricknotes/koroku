package deploy

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// Memory history (ARCHITECTURE 9.5): how often memory is sampled, how long
// the buckets are kept and how often old ones are removed.
const (
	MemorySampleInterval = time.Minute
	MemoryRetention      = 14 * 24 * time.Hour
	memoryPurgeInterval  = time.Hour
)

// MemorySampler records the memory use of every running service of the
// koroku apps once a minute. It is not safe for concurrent use: Run calls
// Sample from one goroutine.
type MemorySampler struct {
	engine *Engine
	// restarts is the RestartCount of each container at the previous
	// sampling, to count the restarts in between.
	restarts  map[string]int
	lastPurge time.Time
	failing   bool
}

// NewMemorySampler returns a sampler that reads Docker through the engine's
// runner and writes to its store.
func (e *Engine) NewMemorySampler() *MemorySampler {
	return &MemorySampler{engine: e, restarts: map[string]int{}}
}

// Run samples every MemorySampleInterval until ctx is canceled. Failures
// are logged once until a sampling succeeds again.
func (m *MemorySampler) Run(ctx context.Context) {
	ticker := time.NewTicker(MemorySampleInterval)
	defer ticker.Stop()
	for {
		err := m.Sample(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil && !m.failing:
			m.engine.opts.Logger.Warn("sampling memory use failed; retrying every minute", "error", err)
			m.failing = true
		case err == nil && m.failing:
			m.engine.opts.Logger.Info("sampling memory use works again")
			m.failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sample takes one sampling: `docker stats` for the running containers,
// `docker container inspect` for their Compose labels, limits and restart
// counts, then one write of the services of koroku apps. Once an hour it
// also removes buckets past the retention.
func (m *MemorySampler) Sample(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	e := m.engine
	now := e.opts.Clock.Now()
	if m.lastPurge.IsZero() || now.Sub(m.lastPurge) >= memoryPurgeInterval {
		if _, err := e.opts.Store.DeleteMemoryUsageBefore(ctx, now.Add(-MemoryRetention)); err != nil {
			return err
		}
		m.lastPurge = now
	}

	out, err := e.retryingDocker(ctx, "stats", "--no-stream", "--no-trunc", "--format", "json")
	if err != nil {
		return err
	}
	rows, err := parseStats(out)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		clear(m.restarts)
		return nil
	}
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	inspected, err := e.inspectContainers(ctx, ids)
	if err != nil {
		return err
	}
	apps, err := e.opts.Store.ListApps(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	appIDs := map[string]int64{}
	for _, app := range apps {
		appIDs[app.ProjectName()] = app.ID
	}

	type key struct {
		app     int64
		service string
	}
	byService := map[key]*store.MemorySample{}
	var order []key
	seen := map[string]int{}
	for _, row := range rows {
		meta, ok := inspected[row.ID]
		if !ok {
			continue // removed since docker stats ran
		}
		seen[row.ID] = meta.restarts
		appID, ok := appIDs[meta.project]
		if !ok || meta.service == "" {
			continue
		}
		restarts := 0
		if prev, ok := m.restarts[row.ID]; ok && meta.restarts > prev {
			restarts = meta.restarts - prev
		}
		k := key{appID, meta.service}
		sample := byService[k]
		if sample == nil {
			sample = &store.MemorySample{AppID: appID, Service: meta.service, Bytes: -1}
			byService[k] = sample
			order = append(order, k)
		}
		sample.Restarts += restarts
		// With several containers of one service (replicas), the busiest
		// one counts: the limit applies to each container.
		if row.memory > sample.Bytes {
			sample.Bytes = row.memory
			sample.LimitBytes = nil
			if meta.limit > 0 {
				limit := meta.limit
				sample.LimitBytes = &limit
			}
		}
	}
	m.restarts = seen

	slices.SortFunc(order, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.app, b.app), strings.Compare(a.service, b.service))
	})
	samples := make([]store.MemorySample, 0, len(order))
	for _, k := range order {
		samples = append(samples, *byService[k])
	}
	return e.opts.Store.RecordMemorySamples(ctx, now, samples)
}
