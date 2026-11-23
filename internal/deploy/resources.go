package deploy

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// ContainerUsage is what one running container uses.
type ContainerUsage struct {
	Name string `json:"name"`
	// Project and Service are the Compose labels; empty when the container
	// was not created by Compose.
	Project string `json:"project"`
	Service string `json:"service"`
	// Color is the colour of a copy of a blue/green app's public service,
	// which is shown as the public service (ARCHITECTURE 7.2); "" otherwise.
	Color       string `json:"color,omitempty"`
	MemoryBytes int64  `json:"memory_bytes"`
	// MemoryLimitBytes is nil when the container has no memory limit.
	MemoryLimitBytes *int64 `json:"memory_limit_bytes"`
	// CPUPercent is as docker stats reports it: one core is 100%, so it
	// ranges up to CPUCount × 100.
	CPUPercent float64 `json:"cpu_percent"`
}

// ResourceGroup totals the running containers of one app, or (App nil)
// those of no koroku app.
type ResourceGroup struct {
	App         *string `json:"app"`
	MemoryBytes int64   `json:"memory_bytes"`
	// MemoryLimitBytes is the sum of the containers' limits; nil when any
	// container has none.
	MemoryLimitBytes *int64           `json:"memory_limit_bytes"`
	CPUPercent       float64          `json:"cpu_percent"`
	Containers       []ContainerUsage `json:"containers"`
}

// ContainerResources breaks down what Docker containers use, by app
// (ARCHITECTURE 9.3).
type ContainerResources struct {
	CPUCount         int             `json:"cpu_count"`
	MemoryTotalBytes int64           `json:"memory_total_bytes"`
	Groups           []ResourceGroup `json:"groups"`
}

// containerInspectFormat prints, per container, the labels, the memory
// limit and the restart count that `docker stats` does not report, as
// tab-separated fields (strings in JSON).
const containerInspectFormat = `{{json .Id}}	{{json .Name}}	{{json (index .Config.Labels "com.docker.compose.project")}}	{{json (index .Config.Labels "com.docker.compose.service")}}	{{.HostConfig.Memory}}	{{.RestartCount}}`

// ContainerResources reports the memory and CPU use of every running
// container, grouped by koroku app. `docker stats` samples twice to compute
// CPU use, so this takes a couple of seconds.
func (e *Engine) ContainerResources(ctx context.Context) (ContainerResources, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	var stats, info []byte
	var statsErr, infoErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		stats, statsErr = e.retryingDocker(ctx, "stats", "--no-stream", "--no-trunc", "--format", "json")
	}()
	go func() {
		defer wg.Done()
		info, infoErr = runner.Output(ctx, e.opts.Runner, runner.Cmd{
			Name: "docker", Args: []string{"info", "--format", "{{.NCPU}} {{.MemTotal}}"},
		})
		if infoErr != nil {
			infoErr = fmt.Errorf("docker info: %w", infoErr)
		}
	}()
	wg.Wait()
	if err := errors.Join(statsErr, infoErr); err != nil {
		return ContainerResources{}, err
	}

	var result ContainerResources
	if _, err := fmt.Sscan(string(info), &result.CPUCount, &result.MemoryTotalBytes); err != nil || result.CPUCount <= 0 {
		return ContainerResources{}, errors.New("parse docker info output")
	}
	rows, err := parseStats(stats)
	if err != nil {
		return ContainerResources{}, err
	}
	result.Groups = []ResourceGroup{}
	if len(rows) == 0 {
		return result, nil
	}

	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	inspected, err := e.inspectContainers(ctx, ids)
	if err != nil {
		return ContainerResources{}, err
	}
	apps, err := e.opts.Store.ListApps(ctx)
	if err != nil {
		return ContainerResources{}, fmt.Errorf("list apps: %w", err)
	}
	slugs := map[string]string{}
	byProject := map[string]store.App{}
	for _, app := range apps {
		slugs[app.ProjectName()] = app.Slug
		byProject[app.ProjectName()] = app
	}

	var containers []ContainerUsage
	for _, row := range rows {
		meta, ok := inspected[row.ID]
		if !ok {
			continue // removed since docker stats ran
		}
		usage := ContainerUsage{
			Name:        meta.name,
			Project:     meta.project,
			Service:     meta.service,
			MemoryBytes: row.memory,
			CPUPercent:  roundCPU(row.cpu),
		}
		if meta.limit > 0 {
			limit := meta.limit
			usage.MemoryLimitBytes = &limit
		}
		if app, ok := byProject[meta.project]; ok {
			usage.Service, usage.Color = app.PublicServiceOf(meta.service)
		}
		containers = append(containers, usage)
	}
	result.Groups = groupByApp(containers, slugs)
	return result, nil
}

type statsRow struct {
	ID     string
	memory int64
	cpu    float64 // percent of one core
}

// parseStats reads `docker stats --no-stream --no-trunc --format json`: one
// JSON object per line. Figures Docker cannot report yet (a container that
// is just starting prints "--") count as 0.
func parseStats(out []byte) ([]statsRow, error) {
	var rows []statsRow
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var raw struct{ ID, CPUPerc, MemUsage string }
		if err := json.Unmarshal(line, &raw); err != nil {
			return nil, errors.New("parse docker stats output")
		}
		if raw.ID == "" {
			continue
		}
		used, _, _ := strings.Cut(raw.MemUsage, " / ")
		cpu, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(raw.CPUPerc, "%")), 64)
		if err != nil || cpu < 0 || math.IsNaN(cpu) || math.IsInf(cpu, 0) {
			cpu = 0
		}
		rows = append(rows, statsRow{ID: raw.ID, memory: parseDockerSize(used), cpu: cpu})
	}
	return rows, nil
}

type containerMeta struct {
	name, project, service string
	limit                  int64
	// restarts is Docker's RestartCount: restarts by the restart policy.
	restarts int
}

// inspectContainers reads the Compose labels and memory limits of the
// given containers. A container removed meanwhile makes docker fail with
// "No such container" after printing the others; those are used.
func (e *Engine) inspectContainers(ctx context.Context, ids []string) (map[string]containerMeta, error) {
	args := append([]string{"container", "inspect", "--format", containerInspectFormat}, ids...)
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: args})
	if err != nil && !strings.Contains(err.Error(), "No such container") {
		return nil, fmt.Errorf("docker container inspect: %w", err)
	}
	return parseContainerInspect(out)
}

func parseContainerInspect(out []byte) (map[string]containerMeta, error) {
	result := map[string]containerMeta{}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		fields := strings.Split(string(line), "\t")
		if len(fields) != 6 {
			return nil, errors.New("parse docker container inspect output")
		}
		var id, name, project, service string
		for i, dst := range []*string{&id, &name, &project, &service} {
			if err := json.Unmarshal([]byte(fields[i]), dst); err != nil {
				return nil, errors.New("parse docker container inspect output")
			}
		}
		limit, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			return nil, errors.New("parse docker container inspect output")
		}
		restarts, err := strconv.Atoi(fields[5])
		if err != nil {
			return nil, errors.New("parse docker container inspect output")
		}
		result[id] = containerMeta{
			name:     strings.TrimPrefix(name, "/"),
			project:  project,
			service:  service,
			limit:    limit,
			restarts: restarts,
		}
	}
	return result, nil
}

// groupByApp totals containers per app (slugs maps a Compose project name
// to the app's slug). Apps come first, most memory first; the containers
// of no app are one group at the end.
func groupByApp(containers []ContainerUsage, slugs map[string]string) []ResourceGroup {
	byApp := map[string]*ResourceGroup{}
	var apps []*ResourceGroup
	other := &ResourceGroup{}
	for _, c := range containers {
		group := other
		if slug, ok := slugs[c.Project]; ok {
			group = byApp[slug]
			if group == nil {
				group = &ResourceGroup{App: &slug}
				byApp[slug] = group
				apps = append(apps, group)
			}
		}
		group.Containers = append(group.Containers, c)
	}
	if len(other.Containers) > 0 {
		apps = append(apps, other)
	}

	groups := make([]ResourceGroup, 0, len(apps))
	for _, g := range apps {
		var limit int64
		limited := true
		var cpu float64
		for _, c := range g.Containers {
			g.MemoryBytes += c.MemoryBytes
			cpu += c.CPUPercent
			if c.MemoryLimitBytes == nil {
				limited = false
			} else {
				limit += *c.MemoryLimitBytes
			}
		}
		g.CPUPercent = roundCPU(cpu)
		if limited {
			g.MemoryLimitBytes = &limit
		}
		slices.SortStableFunc(g.Containers, func(a, b ContainerUsage) int {
			return cmp.Or(cmp.Compare(b.MemoryBytes, a.MemoryBytes), cmp.Compare(a.Name, b.Name))
		})
		groups = append(groups, *g)
	}
	slices.SortStableFunc(groups, func(a, b ResourceGroup) int {
		// The group of no app stays last.
		if (a.App == nil) != (b.App == nil) {
			if a.App == nil {
				return 1
			}
			return -1
		}
		if a.App == nil {
			return 0
		}
		return cmp.Or(cmp.Compare(b.MemoryBytes, a.MemoryBytes), cmp.Compare(*a.App, *b.App))
	})
	return groups
}

// roundCPU keeps two decimals, as stats prints; sums would add float noise.
func roundCPU(percent float64) float64 {
	return math.Round(percent*100) / 100
}
