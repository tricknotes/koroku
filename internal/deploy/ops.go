package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
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

// opTimeout bounds the operations that stop containers: stop, restart,
// delete (down) and the stops of sleep and scheduled tasks. Docker waits
// for each container's stop_grace_period, up to compose.MaxStopGracePeriod
// in total (ARCHITECTURE 6). Start uses upTimeout.
const opTimeout = compose.MaxStopGracePeriod + 3*time.Minute

// upTimeout bounds an `up` outside of a deployment's own job (start, the
// automatic rollback): stopping the containers it recreates, then
// --wait-timeout, which only covers the wait after they started.
func (e *Engine) upTimeout() time.Duration {
	return e.opts.WaitTimeout + compose.MaxStopGracePeriod + time.Minute
}

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
		Name: "docker", Args: inv.Args(sub...), Env: inv.Env(), CleanEnv: true, Dir: dir, Stdout: &output, Stderr: &output,
	})
	if err != nil {
		err = fmt.Errorf("docker compose %s: %w: %s", sub[0], err, lastOutputLine(output.String()))
		if sub[0] == "up" && ctx.Err() == nil && outputHasRegistryAuthFailure(output.String()) {
			return &RegistryAuthError{Configured: inv.DockerConfig != "", Err: err}
		}
		return err
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
	ctx, cancel := context.WithTimeout(ctx, e.upTimeout())
	defer cancel()
	if err := e.upCurrent(ctx, app); err != nil {
		return err
	}
	if err := e.opts.Store.SetAppDesiredState(ctx, app.ID, store.DesiredRunning, e.opts.Clock.Now()); err != nil {
		return err
	}
	e.syncRoutes(ctx, "app started")
	return nil
}

// upCurrent brings the app's current release up with its files, checked
// again first, or starts the existing containers when the files are gone.
//
// With services, only those are brought up, without their dependencies
// and without removing the others.
func (e *Engine) upCurrent(ctx context.Context, app store.App, services ...string) error {
	// A blue/green app runs its public service in its colour.
	services = slices.Clone(services)
	for i, name := range services {
		services[i] = app.LiveService(name)
	}
	inv, dir, ok := e.currentInvocation(app)
	if !ok {
		// The release files are gone; the stopped containers still exist.
		return e.runCompose(ctx, e.projectOnly(app), "", append([]string{"start"}, services...)...)
	}
	// The release files are reused: check them again first.
	meta, err := e.readMeta(app.Slug, *app.CurrentDeploymentID)
	if err != nil {
		return err
	}
	// A missing image is pulled with the app's own registry login.
	if inv.DockerConfig, err = e.registryConfig(ctx, app, secrets.NewMasker()); err != nil {
		return err
	}
	if err := e.revalidate(ctx, app, *app.CurrentDeploymentID, meta.ComposeFile, inv.DockerConfig); err != nil {
		return fmt.Errorf("the release cannot be started as it is (deploy the app again): %w", err)
	}
	if len(services) == 0 {
		// The whole release, in the app's colour when it has the
		// blue/green form (ARCHITECTURE 7.2).
		return e.upRelease(ctx, app, *app.CurrentDeploymentID, inv.DockerConfig, e.runCompose)
	}
	return e.runCompose(ctx, inv, dir, e.upArgs(services...)...)
}

// waitSeconds is WaitTimeout as the --wait-timeout of docker compose.
func (e *Engine) waitSeconds() string { return strconv.Itoa(int(e.opts.WaitTimeout / time.Second)) }

// Restart restarts the app's containers. A stopped app is started, and a
// sleeping one woken (ARCHITECTURE 8.4).
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
	current, err := e.opts.Store.GetAppByID(ctx, app.ID)
	if err != nil {
		return err
	}
	if current.SleepingSince != nil {
		ctx, cancel := context.WithTimeout(ctx, e.wakeTimeout())
		defer cancel()
		_, err := e.wakeLocked(ctx, app.ID, nil)
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	// Named, so that only the colour that serves restarts.
	return e.runCompose(ctx, e.projectOnly(app), "", append([]string{"restart"}, e.liveServices(current)...)...)
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

	// Images of every release still on disk. Pulled images go only when
	// no other app uses them.
	stateDir := filepath.Join(e.appDir(app.Slug), "release-state")
	if entries, err := os.ReadDir(stateDir); err == nil {
		var images, pulled []string
		for _, entry := range entries {
			if id, err := strconv.ParseInt(entry.Name(), 10, 64); err == nil {
				if meta, err := e.readMeta(app.Slug, id); err == nil {
					images = append(images, meta.Images...)
					pulled = append(pulled, meta.PulledImages...)
				}
			}
		}
		if len(images) > 0 {
			args := append([]string{"image", "rm"}, images...)
			if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args}); err != nil {
				e.opts.Logger.Warn("removing the app's images failed", "app", app.Slug, "error", err)
			}
		}
		e.removeUnusedPulledImages(ctx, pulled, app.Slug)
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
	// CPUPercent is as docker stats reports it (one core is 100%); nil
	// when unknown, like the memory figures.
	CPUPercent *float64 `json:"cpu_percent"`
	// MemoryReservationBytes is what the running release reserves for the
	// service (ARCHITECTURE 6.2); nil when it reserves nothing.
	MemoryReservationBytes *int64 `json:"memory_reservation_bytes"`
	// NextMemoryReservationBytes is what the next deployment reserves with
	// the saved settings (0 for none); nil when the running release did
	// not record its services' memory shapes.
	NextMemoryReservationBytes *int64 `json:"next_memory_reservation_bytes"`
	// Color is the colour of the container of a blue/green app's public
	// service, listed under the public service (ARCHITECTURE 7.2); "" for
	// other containers. While a deploy runs both colours side by side, the
	// service has a row for each.
	Color string `json:"color,omitempty"`

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
	for i := range services {
		services[i].Name, services[i].Color = app.PublicServiceOf(services[i].Name)
	}
	sort.SliceStable(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	var running []string
	for _, svc := range services {
		if svc.State == "running" && svc.container != "" {
			running = append(running, svc.container)
		}
	}
	usage := e.containerUsage(ctx, running)
	for i := range services {
		if u, ok := usage[services[i].container]; ok {
			services[i].MemoryBytes, services[i].MemoryLimitBytes = &u.memory, &u.limit
			if u.hasCPU {
				services[i].CPUPercent = &u.cpu
			}
		}
	}
	if app.CurrentDeploymentID != nil {
		recorded, err := e.opts.Store.DeploymentMemoryReservations(ctx, *app.CurrentDeploymentID)
		if err != nil {
			return nil, err
		}
		for i := range services {
			if r, ok := recorded[*app.CurrentDeploymentID][services[i].Name]; ok {
				services[i].MemoryReservationBytes = &r
			}
		}
		shapes, err := e.opts.Store.DeploymentMemoryShapes(ctx, *app.CurrentDeploymentID)
		if err != nil {
			return nil, err
		}
		if recordedShapes, ok := shapes[*app.CurrentDeploymentID]; ok {
			next := NextReservations(app, e.opts.DefaultMemory, recordedShapes)
			for i := range services {
				if _, known := recordedShapes[services[i].Name]; known {
					r := next[services[i].Name]
					services[i].NextMemoryReservationBytes = &r
				}
			}
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

// PruneResult is the outcome of a manual cleanup. ReclaimedBytes is the
// space Docker reports it freed, summed over the commands that said so.
type PruneResult struct {
	Output         string `json:"output"`
	ReclaimedBytes int64  `json:"reclaimed_bytes"`
}

// buildCacheKeep is how long build cache stays after its last use. Prune
// removes what has not been used for longer (ARCHITECTURE chapter 10); it
// must match the filter of builderPruneArgs.
const buildCacheKeep = 168 * time.Hour

// builderPruneArgs prunes build cache unused for buildCacheKeep. --all is
// needed: without it, the BuildKit built into Docker keeps every record
// whose layer an image also uses, and with it every record behind it.
var builderPruneArgs = []string{"builder", "prune", "--all", "-f", "--filter", "until=168h"}

// Prune removes dangling images and build cache unused for a week
// (ARCHITECTURE chapter 10). It never touches volumes or tagged images.
func (e *Engine) Prune(ctx context.Context, images, builder bool) (PruneResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	var output bytes.Buffer
	var reclaimed int64
	run := func(args ...string) error {
		fmt.Fprintf(&output, "$ docker %s\n", strings.Join(args, " "))
		var out bytes.Buffer
		err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args, Stdout: &out, Stderr: &out})
		reclaimed += reclaimedBytes(out.String())
		output.Write(out.Bytes())
		return err
	}
	var errs []error
	if images {
		if err := run("image", "prune", "-f"); err != nil {
			errs = append(errs, fmt.Errorf("image prune: %w", err))
		}
	}
	if builder {
		if err := run(builderPruneArgs...); err != nil {
			errs = append(errs, fmt.Errorf("builder prune: %w", err))
		}
	}
	text := output.String()
	if len(text) > 8192 {
		text = "...\n" + text[len(text)-8192:]
	}
	return PruneResult{Output: text, ReclaimedBytes: reclaimed}, errors.Join(errs...)
}

// reclaimedBytes reads the total a prune command prints last:
// "Total reclaimed space: 1.2GB" (image prune) or "Total:\t1.861MB"
// (builder prune).
func reclaimedBytes(out string) int64 {
	var total int64
	for _, line := range strings.Split(out, "\n") {
		for _, prefix := range []string{"Total reclaimed space:", "Total:"} {
			if size, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
				total = parseDockerSize(size)
			}
		}
	}
	return total
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

// DiskUsageItem is one row of `docker system df`. ReclaimableBytes is what
// Prune would free, not Docker's own estimate (ARCHITECTURE chapter 10).
type DiskUsageItem struct {
	Type             string `json:"type"` // images | containers | volumes | build_cache
	TotalBytes       int64  `json:"total_bytes"`
	ReclaimableBytes int64  `json:"reclaimable_bytes"`
	Count            int    `json:"count"`
}

// dockerAttempts and dockerRetryDelay bound the retries of read-only docker
// commands that race container removal (`docker system df`, `docker stats`).
var (
	dockerAttempts   = 3
	dockerRetryDelay = 250 * time.Millisecond
)

// DockerDiskUsage reports how much disk Docker uses (`docker system df`)
// and how much of it Prune would free. The totals come from the summary;
// what Prune frees from the verbose listing, as Docker's reclaimable figure
// counts tagged images and recent build cache, which Prune keeps. Both walk
// every image and volume, so they can take seconds; they run concurrently
// (measured: together as fast as either alone).
func (e *Engine) DockerDiskUsage(ctx context.Context) ([]DiskUsageItem, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var summary, detail, built []byte
	var summaryErr, detailErr, builtErr error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		summary, summaryErr = e.retryingDocker(ctx, "system", "df", "--format", "json")
	}()
	go func() {
		defer wg.Done()
		detail, detailErr = e.retryingDocker(ctx, "system", "df", "-v", "--format", "json")
	}()
	go func() {
		defer wg.Done()
		built, builtErr = runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: composeBuiltDanglingArgs})
		if builtErr != nil {
			builtErr = fmt.Errorf("docker image ls: %w", builtErr)
		}
	}()
	wg.Wait()
	if err := errors.Join(summaryErr, detailErr, builtErr); err != nil {
		return nil, err
	}
	items, err := parseSystemDF(summary)
	if err != nil {
		return nil, err
	}
	reclaimable, err := prunableBytes(detail, e.opts.Clock.Now(), strings.Fields(string(built)))
	if err != nil {
		return nil, err
	}
	for i := range items {
		items[i].ReclaimableBytes = reclaimable[items[i].Type]
	}
	return items, nil
}

// retryingDocker runs a read-only docker command that walks every container
// or image: `docker system df` and `docker stats`.
//
// With the containerd image store (the default of new Docker installs), df
// fails when what it walks changes meanwhile: a container or image removed
// by a deployment ("rw layer snapshot not found for container ...",
// "content digest sha256:...: not found"), or a file deleted in a running
// container's writable layer ("snapshotter.Usage failed for ...: lstat
// ...: no such file or directory"). Errors the daemon returns are retried
// a few times; others (the daemon is not reachable) are returned at once.
func (e *Engine) retryingDocker(ctx context.Context, args ...string) ([]byte, error) {
	var err error
	for attempt := 1; ; attempt++ {
		var out []byte
		out, err = runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: args})
		if err == nil {
			return out, nil
		}
		if attempt == dockerAttempts || !strings.Contains(err.Error(), "Error response from daemon") {
			break
		}
		timer := time.NewTimer(time.Duration(attempt) * dockerRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("docker %s: %w", strings.Join(args, " "), err)
}

// composeBuiltDanglingArgs lists the dangling images Compose built (it
// labels them). Their layers are also build cache, so removing such an
// image alone frees nothing.
var composeBuiltDanglingArgs = []string{
	"image", "ls", "--no-trunc", "--format", "{{.ID}}",
	"--filter", "dangling=true", "--filter", "label=com.docker.compose.project",
}

var diskUsageTypes = map[string]string{
	"Images":        "images",
	"Containers":    "containers",
	"Local Volumes": "volumes",
	"Build Cache":   "build_cache",
}

// parseSystemDF reads the summary: one JSON object per type. Docker's
// Reclaimable column is not used (see DockerDiskUsage).
func parseSystemDF(out []byte) ([]DiskUsageItem, error) {
	items := []DiskUsageItem{}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var row struct {
			Type, Size, TotalCount string
		}
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, errors.New("parse docker system df output")
		}
		kind, ok := diskUsageTypes[row.Type]
		if !ok {
			continue
		}
		count, _ := strconv.Atoi(row.TotalCount)
		items = append(items, DiskUsageItem{
			Type:       kind,
			TotalBytes: parseDockerSize(row.Size),
			Count:      count,
		})
	}
	return items, nil
}

// dfTimeLayout is how `docker system df -v` prints build cache times
// (Go's time.Time.String).
const dfTimeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

// prunableBytes estimates, from `docker system df -v --format json`, what
// Prune frees, by type:
//
//   - images: `docker image prune` removes images without a tag that no
//     container (running or not) uses; each frees its unique size, unless
//     it was built here (its ID is in built): the build cache keeps its
//     layers until that cache goes too, so it is left out.
//   - build_cache: builderPruneArgs removes records unused for
//     buildCacheKeep, and a record only once every record built on it is
//     gone, so a record counts when it and all records after it are old
//     and not in use. Records shared with an image free nothing: the image
//     keeps the layer.
//
// Volumes and containers are never pruned. The estimate leaves out what
// becomes free only because the image prune ran first.
func prunableBytes(out []byte, now time.Time, built []string) (map[string]int64, error) {
	var df struct {
		Images []struct {
			ID, Tag, Containers, UniqueSize string
		}
		BuildCache []struct {
			ID, Parent, InUse, Shared, Size, CreatedAt, LastUsedAt string
		}
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &df); err != nil {
		return nil, errors.New("parse docker system df -v output")
	}
	result := map[string]int64{}
	for _, image := range df.Images {
		if image.Tag == "<none>" && image.Containers == "0" && !slices.Contains(built, image.ID) {
			result["images"] += parseDockerSize(image.UniqueSize)
		}
	}

	cutoff := now.Add(-buildCacheKeep)
	children := map[string][]int{}
	for i, record := range df.BuildCache {
		// A record made from several (a merge) lists every parent.
		for _, parent := range strings.Split(record.Parent, ",") {
			if parent = strings.TrimSpace(parent); parent != "" {
				children[parent] = append(children[parent], i)
			}
		}
	}
	removable := make([]*bool, len(df.BuildCache))
	var isRemovable func(i int) bool
	isRemovable = func(i int) bool {
		if removable[i] != nil {
			return *removable[i]
		}
		ok := false
		removable[i] = &ok // guards against a malformed cycle
		record := df.BuildCache[i]
		used := record.LastUsedAt
		if used == "" {
			used = record.CreatedAt
		}
		at, err := time.Parse(dfTimeLayout, used)
		ok = err == nil && at.Before(cutoff) && record.InUse != "true"
		for _, child := range children[record.ID] {
			if !ok {
				break
			}
			ok = isRemovable(child)
		}
		return ok
	}
	for i, record := range df.BuildCache {
		if isRemovable(i) && record.Shared != "true" {
			result["build_cache"] += parseDockerSize(record.Size)
		}
	}
	return result, nil
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

// usage is what docker stats reports for one container.
type usage struct {
	memory, limit int64
	cpu           float64
	hasCPU        bool
}

// containerUsage reads the current memory use, memory limit and CPU of
// containers with `docker stats`. Containers that are not running are absent
// from the result.
func (e *Engine) containerUsage(ctx context.Context, containers []string) map[string]usage {
	result := map[string]usage{}
	if len(containers) == 0 {
		return result
	}
	args := append([]string{"stats", "--no-stream", "--format", "json"}, containers...)
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: args})
	if err != nil {
		// The figures are a nicety; the listing works without them.
		return result
	}
	for _, line := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		var row struct{ Name, MemUsage, CPUPerc string }
		if json.Unmarshal(bytes.TrimSpace(line), &row) != nil || row.Name == "" {
			continue
		}
		used, limit, ok := strings.Cut(row.MemUsage, " / ")
		if !ok {
			continue
		}
		u := usage{memory: parseDockerSize(used), limit: parseDockerSize(limit)}
		// docker stats shows "--" for the CPU of a container it could not
		// measure twice; that is unknown, not zero.
		if cpu, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(row.CPUPerc, "%")), 64); err == nil && cpu >= 0 {
			u.cpu, u.hasCPU = roundCPU(cpu), true
		}
		result[row.Name] = u
	}
	return result
}
