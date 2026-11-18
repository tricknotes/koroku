package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

var (
	// ErrBusy is returned when the app is being deployed or another
	// operation on it is in progress.
	ErrBusy = errors.New("the app is busy with a deployment or another operation")
	// ErrNotDeployed is returned when an app has no successful deployment
	// to start.
	ErrNotDeployed = errors.New("the app has not been deployed yet")
)

// opTimeout bounds stop, restart and delete. Start uses WaitTimeout.
const opTimeout = 3 * time.Minute

// appLocks hands out one mutex per app so that a deployment and a manual
// operation on the same app never run docker compose at the same time.
type appLocks struct {
	mu    sync.Mutex
	locks map[int64]*sync.Mutex
}

func (l *appLocks) get(appID int64) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.locks == nil {
		l.locks = map[int64]*sync.Mutex{}
	}
	m, ok := l.locks[appID]
	if !ok {
		m = &sync.Mutex{}
		l.locks[appID] = m
	}
	return m
}

// beginOp reserves the app for a manual operation. It fails with ErrBusy
// when a deployment is queued or running, or another operation holds the app.
func (e *Engine) beginOp(ctx context.Context, app store.App) (release func(), err error) {
	lock := e.locks.get(app.ID)
	if !lock.TryLock() {
		return nil, ErrBusy
	}
	active, err := e.opts.Store.HasActiveDeployment(ctx, app.ID)
	if err != nil {
		lock.Unlock()
		return nil, err
	}
	if active {
		lock.Unlock()
		return nil, ErrBusy
	}
	return lock.Unlock, nil
}

// currentInvocation returns the compose call for the app's current release,
// or ok=false when its files are no longer on disk.
func (e *Engine) currentInvocation(app store.App) (inv compose.Invocation, dir string, ok bool) {
	if app.CurrentDeploymentID == nil {
		return compose.Invocation{}, "", false
	}
	id := *app.CurrentDeploymentID
	meta, err := e.readMeta(app.Slug, id)
	if err != nil {
		return compose.Invocation{}, "", false
	}
	dir = e.ReleaseDir(app.Slug, id)
	if _, err := os.Stat(filepath.Join(dir, compose.OverrideFileName)); err != nil {
		return compose.Invocation{}, "", false
	}
	return e.invocation(app, id, meta.ComposeFile, true), dir, true
}

func (e *Engine) projectOnly(app store.App) compose.Invocation {
	return compose.Invocation{ProjectName: app.ProjectName()}
}

// runCompose runs a compose command outside of a deployment. Output is kept
// only to explain a failure.
func (e *Engine) runCompose(ctx context.Context, inv compose.Invocation, dir string, sub ...string) error {
	var output bytes.Buffer
	err := e.opts.Runner.Run(ctx, runner.Cmd{
		Name: "docker", Args: inv.Args(sub...), Dir: dir, Stdout: &output, Stderr: &output,
	})
	if err != nil {
		return fmt.Errorf("docker compose %s: %w: %s", sub[0], err, lastOutputLine(output.String()))
	}
	return nil
}

func lastOutputLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if len(last) > 300 {
		last = last[:300]
	}
	return last
}

func (e *Engine) syncRoutes(ctx context.Context, reason string) {
	if e.opts.Router == nil {
		return
	}
	if err := e.opts.Router.Sync(ctx); err != nil {
		e.opts.Logger.Error("syncing routes failed", "reason", reason, "error", err)
	}
}

// Stop stops the app's containers and marks it stopped. Its hostnames then
// answer with the fixed 503 page.
func (e *Engine) Stop(ctx context.Context, app store.App) error {
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := e.runCompose(ctx, e.projectOnly(app), "", "stop"); err != nil {
		return err
	}
	if err := e.opts.Store.SetAppDesiredState(ctx, app.ID, store.DesiredStopped, e.opts.Clock.Now()); err != nil {
		return err
	}
	e.syncRoutes(ctx, "app stopped")
	return nil
}

// Start brings the app's current release up and marks it running.
func (e *Engine) Start(ctx context.Context, app store.App) error {
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return err
	}
	defer release()
	if app.CurrentDeploymentID == nil {
		return ErrNotDeployed
	}
	ctx, cancel := context.WithTimeout(ctx, e.opts.WaitTimeout+time.Minute)
	defer cancel()
	if inv, dir, ok := e.currentInvocation(app); ok {
		wait := strconv.Itoa(int(e.opts.WaitTimeout / time.Second))
		err = e.runCompose(ctx, inv, dir, "up", "-d", "--remove-orphans", "--wait", "--wait-timeout", wait)
	} else {
		// The release files are gone; the stopped containers still exist.
		err = e.runCompose(ctx, e.projectOnly(app), "", "start")
	}
	if err != nil {
		return err
	}
	if err := e.opts.Store.SetAppDesiredState(ctx, app.ID, store.DesiredRunning, e.opts.Clock.Now()); err != nil {
		return err
	}
	e.syncRoutes(ctx, "app started")
	return nil
}

// Restart restarts the app's containers. A stopped app is started.
func (e *Engine) Restart(ctx context.Context, app store.App) error {
	if app.CurrentDeploymentID == nil {
		return ErrNotDeployed
	}
	if app.DesiredState != store.DesiredRunning {
		return e.Start(ctx, app)
	}
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	return e.runCompose(ctx, e.projectOnly(app), "", "restart")
}

// Delete removes the app: containers, networks, optionally named volumes,
// the images built for its releases, its files and its database rows.
func (e *Engine) Delete(ctx context.Context, app store.App, removeVolumes bool) error {
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	down := []string{"down", "--remove-orphans"}
	if removeVolumes {
		down = append(down, "--volumes")
	}
	// With the release files Compose also knows the declared volumes.
	inv, dir, ok := e.currentInvocation(app)
	if !ok {
		inv, dir = e.projectOnly(app), ""
	}
	if err := e.runCompose(ctx, inv, dir, down...); err != nil {
		return err
	}
	if removeVolumes {
		if err := e.removeProjectVolumes(ctx, app); err != nil {
			return err
		}
	}

	// Images of every release still on disk.
	stateDir := filepath.Join(e.appDir(app.Slug), "release-state")
	if entries, err := os.ReadDir(stateDir); err == nil {
		var images []string
		for _, entry := range entries {
			if id, err := strconv.ParseInt(entry.Name(), 10, 64); err == nil {
				if meta, err := e.readMeta(app.Slug, id); err == nil {
					images = append(images, meta.Images...)
				}
			}
		}
		if len(images) > 0 {
			args := append([]string{"image", "rm"}, images...)
			if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args}); err != nil {
				e.opts.Logger.Warn("removing the app's images failed", "app", app.Slug, "error", err)
			}
		}
	}

	if err := e.opts.Store.DeleteApp(ctx, app.ID); err != nil {
		return err
	}
	if err := os.RemoveAll(e.appDir(app.Slug)); err != nil {
		e.opts.Logger.Warn("removing the app directory failed", "app", app.Slug, "error", err)
	}
	e.syncRoutes(ctx, "app deleted")
	return nil
}

// removeProjectVolumes removes volumes that still carry the project label
// (down --volumes only removes the ones it can derive from its input).
func (e *Engine) removeProjectVolumes(ctx context.Context, app store.App) error {
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: []string{
		"volume", "ls", "--quiet", "--filter", "label=com.docker.compose.project=" + app.ProjectName(),
	}})
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	names := strings.Fields(string(out))
	if len(names) == 0 {
		return nil
	}
	args := append([]string{"volume", "rm"}, names...)
	if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args}); err != nil {
		return fmt.Errorf("remove volumes: %w", err)
	}
	return nil
}

// ServiceStatus describes one container of the app.
type ServiceStatus struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Status string `json:"status"`
	Health string `json:"health"`
	// MemoryBytes and MemoryLimitBytes are nil when unknown (the container
	// is not running, or docker stats failed).
	MemoryBytes      *int64 `json:"memory_bytes"`
	MemoryLimitBytes *int64 `json:"memory_limit_bytes"`

	container string
}

// Services lists the app's containers (`docker compose ps --all`).
func (e *Engine) Services(ctx context.Context, app store.App) ([]ServiceStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{
		Name: "docker", Args: e.projectOnly(app).Args("ps", "--all", "--format", "json"),
	})
	if err != nil {
		return nil, fmt.Errorf("docker compose ps: %w", err)
	}
	services, err := parseComposePS(out)
	if err != nil {
		return nil, err
	}
	var running []string
	for _, svc := range services {
		if svc.State == "running" && svc.container != "" {
			running = append(running, svc.container)
		}
	}
	usage := e.memoryUsage(ctx, running)
	for i := range services {
		if u, ok := usage[services[i].container]; ok {
			used, limit := u[0], u[1]
			services[i].MemoryBytes, services[i].MemoryLimitBytes = &used, &limit
		}
	}
	return services, nil
}

type composePSEntry struct {
	Name    string `json:"Name"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Status  string `json:"Status"`
	Health  string `json:"Health"`
}

// parseComposePS accepts both output shapes of `ps --format json`: one JSON
// object per line (current Compose) and a single JSON array (older releases).
func parseComposePS(out []byte) ([]ServiceStatus, error) {
	trimmed := bytes.TrimSpace(out)
	var entries []composePSEntry
	switch {
	case len(trimmed) == 0:
	case trimmed[0] == '[':
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, errors.New("parse docker compose ps output")
		}
	default:
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var entry composePSEntry
			if err := json.Unmarshal(line, &entry); err != nil {
				return nil, errors.New("parse docker compose ps output")
			}
			entries = append(entries, entry)
		}
	}
	services := make([]ServiceStatus, 0, len(entries))
	for _, entry := range entries {
		services = append(services, ServiceStatus{
			Name: entry.Service, State: entry.State, Status: entry.Status, Health: entry.Health,
			container: entry.Name,
		})
	}
	sort.SliceStable(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services, nil
}

// PruneResult is the outcome of a manual cleanup.
type PruneResult struct {
	Output string `json:"output"`
}

// Prune removes dangling images and build cache older than a week
// (ARCHITECTURE chapter 10). It never touches volumes or tagged images.
func (e *Engine) Prune(ctx context.Context, images, builder bool) (PruneResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var output bytes.Buffer
	run := func(args ...string) error {
		fmt.Fprintf(&output, "$ docker %s\n", strings.Join(args, " "))
		return e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args, Stdout: &output, Stderr: &output})
	}
	var errs []error
	if images {
		if err := run("image", "prune", "-f"); err != nil {
			errs = append(errs, fmt.Errorf("image prune: %w", err))
		}
	}
	if builder {
		if err := run("builder", "prune", "-f", "--filter", "until=168h"); err != nil {
			errs = append(errs, fmt.Errorf("builder prune: %w", err))
		}
	}
	text := output.String()
	if len(text) > 8192 {
		text = "...\n" + text[len(text)-8192:]
	}
	return PruneResult{Output: text}, errors.Join(errs...)
}

// RunMaintenance prunes old build cache once a day until ctx is canceled.
// The filter keeps anything used within the last week, so a daily run has
// the effect of the weekly cleanup the design asks for and survives restarts.
func (e *Engine) RunMaintenance(ctx context.Context) {
	timer := time.NewTimer(10 * time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := e.Prune(ctx, false, true); err != nil && ctx.Err() == nil {
			e.opts.Logger.Warn("pruning the build cache failed", "error", err)
		}
		timer.Reset(24 * time.Hour)
	}
}

// DiskUsageItem is one row of `docker system df`.
type DiskUsageItem struct {
	Type             string `json:"type"` // images | containers | volumes | build_cache
	TotalBytes       int64  `json:"total_bytes"`
	ReclaimableBytes int64  `json:"reclaimable_bytes"`
	Count            int    `json:"count"`
}

// DockerDiskUsage reports how much disk Docker uses (`docker system df`).
// The command walks every image and volume, so it can take seconds.
func (e *Engine) DockerDiskUsage(ctx context.Context) ([]DiskUsageItem, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{
		Name: "docker", Args: []string{"system", "df", "--format", "json"},
	})
	if err != nil {
		return nil, fmt.Errorf("docker system df: %w", err)
	}
	return parseSystemDF(out)
}

var diskUsageTypes = map[string]string{
	"Images":        "images",
	"Containers":    "containers",
	"Local Volumes": "volumes",
	"Build Cache":   "build_cache",
}

func parseSystemDF(out []byte) ([]DiskUsageItem, error) {
	items := []DiskUsageItem{}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row struct {
			Type, Size, Reclaimable, TotalCount string
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, errors.New("parse docker system df output")
		}
		kind, ok := diskUsageTypes[row.Type]
		if !ok {
			continue
		}
		count, _ := strconv.Atoi(row.TotalCount)
		// Reclaimable looks like "28.32GB (69%)".
		reclaimable, _, _ := strings.Cut(row.Reclaimable, " ")
		items = append(items, DiskUsageItem{
			Type:             kind,
			TotalBytes:       parseDockerSize(row.Size),
			ReclaimableBytes: parseDockerSize(reclaimable),
			Count:            count,
		})
	}
	return items, nil
}

// parseDockerSize converts sizes as the Docker CLI prints them ("8.998MB",
// "26.02MiB", "0B") to bytes. Unparseable input yields 0.
func parseDockerSize(s string) int64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		factor float64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12},
		{"B", 1},
	}
	for _, u := range units {
		if number, ok := strings.CutSuffix(s, u.suffix); ok {
			value, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
			if err != nil || value < 0 {
				return 0
			}
			return int64(value * u.factor)
		}
	}
	return 0
}

// memoryUsage reads the current memory use and limit of containers with
// `docker stats`. Containers that are not running are absent from the result.
func (e *Engine) memoryUsage(ctx context.Context, containers []string) map[string][2]int64 {
	usage := map[string][2]int64{}
	if len(containers) == 0 {
		return usage
	}
	args := append([]string{"stats", "--no-stream", "--format", "json"}, containers...)
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: args})
	if err != nil {
		// Memory figures are a nicety; the listing works without them.
		return usage
	}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		var row struct{ Name, MemUsage string }
		if json.Unmarshal(bytes.TrimSpace(line), &row) != nil || row.Name == "" {
			continue
		}
		used, limit, ok := strings.Cut(row.MemUsage, " / ")
		if !ok {
			continue
		}
		usage[row.Name] = [2]int64{parseDockerSize(used), parseDockerSize(limit)}
	}
	return usage
}
