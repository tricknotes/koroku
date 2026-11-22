package deploy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// The read-only view of an app's Compose setup (ARCHITECTURE 9.10): the
// override koroku generated for a release, the merged configuration, and
// the app's containers compared with the current release. Nothing here
// changes a container or a file, so it takes no operation lock.

// composeViewTimeout bounds each command the view runs.
const composeViewTimeout = 30 * time.Second

// providedEnvPrefix marks the variables koroku writes itself; they are not
// secrets (ARCHITECTURE 6).
const providedEnvPrefix = "KOROKU_"

// ComposeRelease is one kept release the view can show.
type ComposeRelease struct {
	DeploymentID  int64      `json:"deployment_id"`
	CommitSHA     string     `json:"commit_sha"`
	CommitMessage string     `json:"commit_message"`
	FinishedAt    *time.Time `json:"finished_at"`
	// Current is true for the app's current release.
	Current bool `json:"current"`
}

// ComposeView is the override and the merged configuration of a release.
type ComposeView struct {
	DeploymentID        int64            `json:"deployment_id"`
	CurrentDeploymentID *int64           `json:"current_deployment_id"`
	Releases            []ComposeRelease `json:"releases"`
	// ComposeFile is the repository's Compose file, relative to the
	// release directory; Files are the -f arguments in order.
	ComposeFile string   `json:"compose_file"`
	Files       []string `json:"files"`
	// Override is koroku.override.yaml as written.
	Override string `json:"override"`
	// Config is the merged configuration with secrets masked, as YAML; nil
	// when `docker compose config` failed, and ConfigError says why.
	Config      *string `json:"config"`
	ConfigError string  `json:"config_error"`
}

// ComposeView returns the view of the release of deployment id, or of the
// current release when id is 0. It fails with ErrNotDeployed when the app
// has no current release on disk and with ErrReleaseUnavailable when the
// requested release is not kept.
func (e *Engine) ComposeView(ctx context.Context, app store.App, id int64) (ComposeView, error) {
	current, hasCurrent := e.currentRelease(app)
	if id == 0 {
		if !hasCurrent {
			return ComposeView{}, ErrNotDeployed
		}
		id = current
	}
	releases, err := e.keptReleases(ctx, app)
	if err != nil {
		return ComposeView{}, err
	}
	known := false
	for _, r := range releases {
		known = known || r.DeploymentID == id
	}
	if !known {
		if id == current {
			return ComposeView{}, ErrNotDeployed
		}
		return ComposeView{}, ErrReleaseUnavailable
	}
	meta, err := e.readMeta(app.Slug, id)
	if err != nil {
		return ComposeView{}, ErrReleaseUnavailable
	}
	dir := e.ReleaseDir(app.Slug, id)
	override, err := os.ReadFile(filepath.Join(dir, compose.OverrideFileName))
	if err != nil {
		return ComposeView{}, ErrReleaseUnavailable
	}
	masker, err := e.viewMasker(ctx, app, id)
	if err != nil {
		return ComposeView{}, err
	}
	view := ComposeView{
		DeploymentID: id,
		Releases:     releases,
		ComposeFile:  meta.ComposeFile,
		Files:        []string{meta.ComposeFile, compose.OverrideFileName},
		Override:     masker.MaskLong(string(override)),
	}
	if hasCurrent {
		view.CurrentDeploymentID = &current
	}
	config, err := e.displayConfig(ctx, app, id, meta.ComposeFile, masker)
	if err != nil {
		view.ConfigError = masker.Mask(err.Error())
	} else {
		view.Config = &config
	}
	return view, nil
}

// currentRelease returns the app's current release if its files are kept.
func (e *Engine) currentRelease(app store.App) (int64, bool) {
	if app.CurrentDeploymentID == nil || !e.ReleaseAvailable(app, *app.CurrentDeploymentID) {
		return 0, false
	}
	return *app.CurrentDeploymentID, true
}

// keptReleases lists the releases whose files are still on disk, newest
// first, with rollback records read as the release they started.
func (e *Engine) keptReleases(ctx context.Context, app store.App) ([]ComposeRelease, error) {
	// Retention keeps KeepReleases distinct releases (ARCHITECTURE 7.1);
	// rollbacks to them can sit in between, so look further back than that.
	succeeded, err := e.opts.Store.SucceededDeployments(ctx, app.ID, 50)
	if err != nil {
		return nil, err
	}
	var out []ComposeRelease
	seen := map[int64]bool{}
	for _, d := range succeeded {
		id := RollbackTarget(d)
		if seen[id] {
			continue
		}
		seen[id] = true
		if !e.ReleaseAvailable(app, id) {
			continue
		}
		target := d
		if id != d.ID {
			if target, err = e.opts.Store.GetDeployment(ctx, id); err != nil {
				continue
			}
		}
		out = append(out, ComposeRelease{
			DeploymentID:  id,
			CommitSHA:     target.CommitSHA,
			CommitMessage: target.CommitMessage,
			FinishedAt:    target.FinishedAt,
			Current:       app.CurrentDeploymentID != nil && *app.CurrentDeploymentID == id,
		})
		if len(out) == e.opts.KeepReleases {
			break
		}
	}
	return out, nil
}

// viewMasker holds the values the view must never show: the saved
// variables and those in the live env file and in the release's
// snapshot, which may still hold values deleted from the database since.
// The variables koroku provides are left out; they are not secrets, and a
// full commit SHA in an image name would otherwise be hidden.
func (e *Engine) viewMasker(ctx context.Context, app store.App, id int64) (*secrets.Masker, error) {
	masker := secrets.NewMasker()
	if _, _, err := e.renderEnv(ctx, app, "", masker); err != nil {
		return nil, err
	}
	for _, path := range []string{e.EnvFile(app.Slug), filepath.Join(e.stateDir(app.Slug, id), "env")} {
		content, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read env file: %w", err)
		}
		for _, entry := range parseEnvFileEntries(string(content)) {
			if !strings.HasPrefix(entry[0], providedEnvPrefix) {
				masker.Add(entry[1])
			}
		}
	}
	return masker, nil
}

// displayConfig runs `docker compose config` on a release as deployments
// do (same project, files in the same order, same --env-file), without
// interpolation and without resolving env files, so that no value of a
// variable enters the output, and returns it as masked YAML.
func (e *Engine) displayConfig(ctx context.Context, app store.App, id int64, composeFile string, masker *secrets.Masker) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, composeViewTimeout)
	defer cancel()
	inv := e.invocation(app, id, composeFile, true)
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{
		Name: "docker",
		Args: inv.Args("config", "--format", "json", "--no-interpolate", "--no-env-resolution"),
		Dir:  e.ReleaseDir(app.Slug, id),
	})
	if err != nil {
		return "", fmt.Errorf("docker compose config: %w", err)
	}
	return compose.DisplayConfig(out, masker.MaskLong)
}

// --- containers ---

// Container mismatch markers (ARCHITECTURE 9.10).
const (
	MismatchOutdated          = "outdated"
	MismatchOrphan            = "orphan"
	MismatchMemoryLimit       = "memory_limit"
	MismatchMemoryReservation = "memory_reservation"
	MismatchOomScoreAdj       = "oom_score_adj"
	MismatchOOMKilled         = "oom_killed"
	MismatchRestarting        = "restarting"
	MismatchRestarted         = "restarted"
	MismatchUnhealthy         = "unhealthy"
	MismatchExited            = "exited"
)

// How the deployment that created a container was found.
const (
	MatchedByImage     = "image"
	MatchedByCreatedAt = "created_at"
)

// ContainerView lists the app's containers compared with its current
// release.
type ContainerView struct {
	CurrentDeploymentID *int64 `json:"current_deployment_id"`
	// Compared is false when the current release could not be read (no
	// release, or `docker compose config` failed): the configuration
	// markers are then missing, and ComparisonError says why.
	Compared        bool            `json:"compared"`
	ComparisonError string          `json:"comparison_error"`
	Containers      []ContainerInfo `json:"containers"`
}

// ContainerInfo is what the view shows of one container. It is picked
// from `docker container inspect` field by field; the environment is
// never read.
type ContainerInfo struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	// ImageDigest is the registry digest of the image; nil for images
	// built on the host, which have none.
	ImageDigest *string   `json:"image_digest"`
	CreatedAt   time.Time `json:"created_at"`
	// DeploymentID is the deployment that most likely created the
	// container, found as MatchedBy says; nil when none fits.
	DeploymentID *int64 `json:"deployment_id"`
	MatchedBy    string `json:"matched_by"`
	// Current says whether the container was created from the current
	// release's configuration (its config hash); nil when not compared.
	Current      *bool      `json:"current"`
	State        string     `json:"state"`
	Health       string     `json:"health"`
	ExitCode     int        `json:"exit_code"`
	StartedAt    *time.Time `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at"`
	RestartCount int        `json:"restart_count"`
	OOMKilled    bool       `json:"oom_killed"`
	// The values Docker applies; 0 means none, as Docker reports it.
	MemoryLimitBytes       int64 `json:"memory_limit_bytes"`
	MemoryReservationBytes int64 `json:"memory_reservation_bytes"`
	OomScoreAdj            int   `json:"oom_score_adj"`
	// Expected is what the current release asks for; nil when not
	// compared or the service is not in the current release.
	Expected   *ExpectedResources `json:"expected"`
	Ports      []PublishedPort    `json:"ports"`
	Mismatches []string           `json:"mismatches"`
}

// ExpectedResources are the values the current release sets.
type ExpectedResources struct {
	MemoryLimitBytes       int64 `json:"memory_limit_bytes"`
	MemoryReservationBytes int64 `json:"memory_reservation_bytes"`
	OomScoreAdj            int   `json:"oom_score_adj"`
}

// PublishedPort is a port published on the host.
type PublishedPort struct {
	HostIP        string `json:"host_ip"`
	HostPort      string `json:"host_port"`
	ContainerPort string `json:"container_port"`
}

// Containers lists the app's containers, running or stopped, without
// one-off command containers, and marks where they differ from the
// current release.
func (e *Engine) Containers(ctx context.Context, app store.App) (ContainerView, error) {
	view := ContainerView{Containers: []ContainerInfo{}}
	ids, err := e.projectContainers(ctx, app)
	if err != nil {
		return view, err
	}
	var containers []inspectedContainer
	if len(ids) > 0 {
		if containers, err = e.inspectForView(ctx, ids); err != nil {
			return view, err
		}
	}
	digests := e.imageDigests(ctx, containers)

	masker, err := e.viewMasker(ctx, app, valueOr(app.CurrentDeploymentID))
	if err != nil {
		return view, err
	}
	var expected *releaseExpectation
	if current, ok := e.currentRelease(app); ok {
		view.CurrentDeploymentID = &current
		expected, err = e.releaseExpectation(ctx, app, current)
		if err != nil {
			view.ComparisonError = masker.Mask(err.Error())
		}
	} else if app.CurrentDeploymentID != nil {
		view.CurrentDeploymentID = app.CurrentDeploymentID
		view.ComparisonError = "the files of the current release are not kept"
	}
	view.Compared = expected != nil

	deployments, err := e.opts.Store.ListDeployments(ctx, app.ID, 200)
	if err != nil {
		return view, err
	}
	for _, c := range containers {
		info := c.info(app, digests)
		info.DeploymentID, info.MatchedBy = creatingDeployment(app, info, deployments)
		if expected != nil {
			expected.compare(&info, c.configHash)
		}
		info.Mismatches = append(info.Mismatches, stateMismatches(info)...)
		info.Name = masker.MaskLong(info.Name)
		info.Image = masker.MaskLong(info.Image)
		view.Containers = append(view.Containers, info)
	}
	sort.SliceStable(view.Containers, func(i, j int) bool {
		a, b := view.Containers[i], view.Containers[j]
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Name < b.Name
	})
	return view, nil
}

func valueOr(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// projectContainers returns the IDs of the app's containers, stopped ones
// included.
func (e *Engine) projectContainers(ctx context.Context, app store.App) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, composeViewTimeout)
	defer cancel()
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: []string{
		"ps", "--all", "--no-trunc", "--quiet",
		"--filter", "label=com.docker.compose.project=" + app.ProjectName(),
	}})
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// inspectedContainer is the part of `docker container inspect` the view
// reads. Config.Env is deliberately absent: the values never get decoded.
type inspectedContainer struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Image   string `json:"Image"`
	State   struct {
		Status     string `json:"Status"`
		OOMKilled  bool   `json:"OOMKilled"`
		ExitCode   int    `json:"ExitCode"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	RestartCount int `json:"RestartCount"`
	HostConfig   struct {
		Memory            int64                    `json:"Memory"`
		MemoryReservation int64                    `json:"MemoryReservation"`
		OomScoreAdj       int                      `json:"OomScoreAdj"`
		PortBindings      map[string][]portBinding `json:"PortBindings"`
	} `json:"HostConfig"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]portBinding `json:"Ports"`
	} `json:"NetworkSettings"`

	configHash string
}

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

const (
	labelService    = "com.docker.compose.service"
	labelConfigHash = "com.docker.compose.config-hash"
	labelOneOff     = "com.docker.compose.oneoff"
)

func (e *Engine) inspectForView(ctx context.Context, ids []string) ([]inspectedContainer, error) {
	ctx, cancel := context.WithTimeout(ctx, composeViewTimeout)
	defer cancel()
	// A container removed between ps and inspect makes inspect fail but
	// still print the others (as in ARCHITECTURE 9.3); use what came out.
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{
		Name: "docker", Args: append([]string{"container", "inspect"}, ids...),
	})
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		return nil, fmt.Errorf("docker container inspect: %w", err)
	}
	return parseInspectForView(out)
}

func parseInspectForView(out []byte) ([]inspectedContainer, error) {
	var all []inspectedContainer
	if err := json.Unmarshal(out, &all); err != nil {
		return nil, errors.New("parse docker container inspect output")
	}
	containers := all[:0]
	for _, c := range all {
		if strings.EqualFold(c.Config.Labels[labelOneOff], "true") {
			continue // a one-off command (ARCHITECTURE 9.6)
		}
		c.configHash = c.Config.Labels[labelConfigHash]
		containers = append(containers, c)
	}
	return containers, nil
}

// imageDigests maps image IDs to their registry digest. It is best effort:
// without it the view still shows the image ID.
func (e *Engine) imageDigests(ctx context.Context, containers []inspectedContainer) map[string]string {
	digests := map[string]string{}
	var ids []string
	seen := map[string]bool{}
	for _, c := range containers {
		if c.Image != "" && !seen[c.Image] {
			seen[c.Image] = true
			ids = append(ids, c.Image)
		}
	}
	if len(ids) == 0 {
		return digests
	}
	ctx, cancel := context.WithTimeout(ctx, composeViewTimeout)
	defer cancel()
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{
		Name: "docker", Args: append([]string{"image", "inspect"}, ids...),
	})
	if err != nil && len(bytes.TrimSpace(out)) == 0 {
		e.opts.Logger.Warn("reading image digests failed", "error", err)
		return digests
	}
	var images []struct {
		ID          string   `json:"Id"`
		RepoDigests []string `json:"RepoDigests"`
	}
	if json.Unmarshal(out, &images) != nil {
		return digests
	}
	for _, img := range images {
		if len(img.RepoDigests) > 0 {
			digests[img.ID] = img.RepoDigests[0]
		}
	}
	return digests
}

func (c inspectedContainer) info(app store.App, digests map[string]string) ContainerInfo {
	info := ContainerInfo{
		Name:                   strings.TrimPrefix(c.Name, "/"),
		Service:                c.Config.Labels[labelService],
		Image:                  c.Config.Image,
		ImageID:                c.Image,
		CreatedAt:              parseDockerTime(c.Created),
		State:                  c.State.Status,
		ExitCode:               c.State.ExitCode,
		StartedAt:              optionalDockerTime(c.State.StartedAt),
		FinishedAt:             optionalDockerTime(c.State.FinishedAt),
		RestartCount:           c.RestartCount,
		OOMKilled:              c.State.OOMKilled,
		MemoryLimitBytes:       c.HostConfig.Memory,
		MemoryReservationBytes: c.HostConfig.MemoryReservation,
		OomScoreAdj:            c.HostConfig.OomScoreAdj,
		Ports:                  publishedPorts(c),
		Mismatches:             []string{},
	}
	if c.State.Health != nil {
		info.Health = c.State.Health.Status
	}
	if d, ok := digests[c.Image]; ok {
		info.ImageDigest = &d
	}
	return info
}

func parseDockerTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// optionalDockerTime returns nil for Docker's zero time (never started or
// never finished).
func optionalDockerTime(s string) *time.Time {
	t := parseDockerTime(s)
	if t.IsZero() || t.Year() <= 1 {
		return nil
	}
	return &t
}

// publishedPorts reads the bindings in effect, or the configured ones for
// a stopped container.
func publishedPorts(c inspectedContainer) []PublishedPort {
	bindings := c.NetworkSettings.Ports
	if len(bindings) == 0 {
		bindings = c.HostConfig.PortBindings
	}
	ports := []PublishedPort{}
	for containerPort, list := range bindings {
		for _, b := range list {
			if b.HostPort == "" {
				continue
			}
			ports = append(ports, PublishedPort{HostIP: b.HostIP, HostPort: b.HostPort, ContainerPort: containerPort})
		}
	}
	sort.Slice(ports, func(i, j int) bool {
		if ports[i].ContainerPort != ports[j].ContainerPort {
			return ports[i].ContainerPort < ports[j].ContainerPort
		}
		return ports[i].HostIP < ports[j].HostIP
	})
	return ports
}

// creatingDeployment finds the deployment that most likely created the
// container: the release in a per-release image tag (`:r<id>`), else the
// last deployment that started before the container was created.
func creatingDeployment(app store.App, info ContainerInfo, deployments []store.Deployment) (*int64, string) {
	prefix := compose.ReleaseImage(app.ProjectName(), info.Service, "r")
	if rest, ok := strings.CutPrefix(info.Image, prefix); ok {
		if id, err := strconv.ParseInt(rest, 10, 64); err == nil && id > 0 {
			return &id, MatchedByImage
		}
	}
	if info.CreatedAt.IsZero() {
		return nil, ""
	}
	var best *store.Deployment
	for i := range deployments {
		d := &deployments[i]
		if d.StartedAt == nil || d.StartedAt.After(info.CreatedAt) {
			continue
		}
		if best == nil || d.StartedAt.After(*best.StartedAt) {
			best = d
		}
	}
	if best == nil {
		return nil, ""
	}
	id := best.ID
	return &id, MatchedByCreatedAt
}

// releaseExpectation is what the current release asks of each service.
type releaseExpectation struct {
	hashes    map[string]string
	resources map[string]ExpectedResources
}

// releaseExpectation reads the config hash of each service of a release
// (`config --hash '*'`, which prints names and hashes only) and the
// resources its merged configuration sets. The merged configuration is
// interpolated so that sizes come out in bytes; only the three values are
// kept from it, and the output is never returned or logged.
func (e *Engine) releaseExpectation(ctx context.Context, app store.App, id int64) (*releaseExpectation, error) {
	meta, err := e.readMeta(app.Slug, id)
	if err != nil {
		return nil, fmt.Errorf("read release: %w", err)
	}
	inv := e.invocation(app, id, meta.ComposeFile, true)
	dir := e.ReleaseDir(app.Slug, id)

	hashCtx, cancel := context.WithTimeout(ctx, composeViewTimeout)
	defer cancel()
	out, err := runner.Output(hashCtx, e.opts.Runner, runner.Cmd{
		Name: "docker", Args: inv.Args("config", "--hash", "*"), Dir: dir,
	})
	if err != nil {
		return nil, fmt.Errorf("docker compose config --hash: %w", err)
	}
	hashes := parseConfigHashes(out)

	configCtx, cancelConfig := context.WithTimeout(ctx, composeViewTimeout)
	defer cancelConfig()
	var stderr bytes.Buffer
	project, err := e.composeConfigTo(configCtx, inv, dir, &stderr)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, lastOutputLine(stderr.String()))
	}
	resources := make(map[string]ExpectedResources, len(project.Services))
	for name, svc := range project.Services {
		resources[name] = expectedResources(svc)
	}
	return &releaseExpectation{hashes: hashes, resources: resources}, nil
}

// parseConfigHashes reads `docker compose config --hash` lines of the form
// "<service> <hash>".
func parseConfigHashes(out []byte) map[string]string {
	hashes := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 {
			hashes[fields[0]] = fields[1]
		}
	}
	return hashes
}

func expectedResources(svc compose.Service) ExpectedResources {
	var r ExpectedResources
	if v, ok := compose.ParseMemory(svc.MemLimit); ok {
		r.MemoryLimitBytes = v
	} else if v, ok := compose.ParseMemory(svc.DeployMemoryLimit()); ok {
		r.MemoryLimitBytes = v
	}
	if v, ok := compose.ParseMemory(svc.MemReservation); ok {
		r.MemoryReservationBytes = v
	} else if v, ok := compose.ParseMemory(svc.DeployMemoryReservation()); ok {
		r.MemoryReservationBytes = v
	}
	if svc.OomScoreAdj != nil {
		r.OomScoreAdj = *svc.OomScoreAdj
	}
	return r
}

// compare marks where the container differs from the release.
func (x *releaseExpectation) compare(info *ContainerInfo, configHash string) {
	want, inRelease := x.resources[info.Service]
	if !inRelease {
		info.Mismatches = append(info.Mismatches, MismatchOrphan)
		current := false
		info.Current = &current
		return
	}
	info.Expected = &want
	current := configHash != "" && configHash == x.hashes[info.Service]
	info.Current = &current
	if !current {
		info.Mismatches = append(info.Mismatches, MismatchOutdated)
	}
	if info.MemoryLimitBytes != want.MemoryLimitBytes {
		info.Mismatches = append(info.Mismatches, MismatchMemoryLimit)
	}
	if info.MemoryReservationBytes != want.MemoryReservationBytes {
		info.Mismatches = append(info.Mismatches, MismatchMemoryReservation)
	}
	if info.OomScoreAdj != want.OomScoreAdj {
		info.Mismatches = append(info.Mismatches, MismatchOomScoreAdj)
	}
}

// stateMismatches marks troubles that need no release to compare with.
func stateMismatches(info ContainerInfo) []string {
	var out []string
	if info.OOMKilled {
		out = append(out, MismatchOOMKilled)
	}
	if info.State == "restarting" {
		out = append(out, MismatchRestarting)
	}
	if info.RestartCount > 0 {
		out = append(out, MismatchRestarted)
	}
	if info.Health == "unhealthy" {
		out = append(out, MismatchUnhealthy)
	}
	if info.State == "exited" && info.ExitCode != 0 {
		out = append(out, MismatchExited)
	}
	return out
}
