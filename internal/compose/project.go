// Package compose handles everything koroku knows about Docker Compose
// projects: locating the Compose file, reading the normalized configuration,
// generating the override file and checking the deploy policy.
package compose

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// OverrideFileName is the name of the generated override inside a release directory.
const OverrideFileName = "koroku.override.yaml"

// Project is the subset of `docker compose config --format json` that koroku
// inspects. The full output contains resolved environment values and must
// never be logged.
type Project struct {
	Name     string                `json:"name"`
	Services map[string]Service    `json:"services"`
	Volumes  map[string]Volume     `json:"volumes"`
	Networks map[string]Network    `json:"networks"`
	Secrets  map[string]FileObject `json:"secrets"`
	Configs  map[string]FileObject `json:"configs"`

	// Raw is the whole normalized project as Compose printed it, for
	// finding the settings koroku does not allow (unsupportedSettings). It
	// may hold variable values and is never logged.
	Raw json.RawMessage `json:"-"`
}

// Service is one Compose service in normalized form.
type Service struct {
	Image       string   `json:"image"`
	Build       *Build   `json:"build"`
	Ports       []Port   `json:"ports"`
	Privileged  bool     `json:"privileged"`
	NetworkMode string   `json:"network_mode"`
	Pid         string   `json:"pid"`
	Ipc         string   `json:"ipc"`
	Uts         string   `json:"uts"`
	UsernsMode  string   `json:"userns_mode"`
	Cgroup      string   `json:"cgroup"`
	Volumes     []Mount  `json:"volumes"`
	VolumesFrom []string `json:"volumes_from"`
	Restart     string   `json:"restart"`
	MemLimit    string   `json:"mem_limit"`
	Deploy      *Deploy  `json:"deploy"`
	// MemReservation and OomScoreAdj are replaced by the override
	// (ARCHITECTURE 6.2); they are read to pick the form to write and to
	// check the merged result.
	MemReservation string `json:"mem_reservation"`
	OomScoreAdj    *int   `json:"oom_score_adj"`

	// Raw is the service's whole normalized definition, as Compose printed
	// it, for telling which services follow the commit (CommitServices).
	// It may hold variable values and is never logged.
	Raw json.RawMessage `json:"-"`

	CapAdd            []string          `json:"cap_add"`
	Devices           []json.RawMessage `json:"devices"`
	DeviceCgroupRules []string          `json:"device_cgroup_rules"`
	SecurityOpt       []string          `json:"security_opt"`
	// EnvFiles is only populated when the configuration was produced with
	// --no-env-resolution; otherwise Compose folds the files' content into
	// the environment and the paths are gone.
	EnvFiles []EnvFile `json:"env_file"`

	Runtime        string `json:"runtime"`
	OomKillDisable bool   `json:"oom_kill_disable"`
	MemswapLimit   string `json:"memswap_limit"`
	// StopGracePeriodRaw is how long Docker waits after SIGTERM before it
	// kills the container, as Compose prints it (a Go duration such as
	// "1m30s"); empty is Docker's default. See StopGracePeriod.
	StopGracePeriodRaw string `json:"stop_grace_period"`

	// DependsOn holds the services this one depends on. The normalized
	// configuration always prints the long form (a map of conditions);
	// only the names matter here.
	DependsOn map[string]json.RawMessage `json:"depends_on"`

	// ContainerName, Scale and Networks matter to blue/green deploys
	// (ARCHITECTURE 7.2): a fixed name or more than one container cannot
	// run twice side by side, and the coloured copies join the networks
	// of the public service under its name.
	ContainerName string `json:"container_name"`
	Scale         *int   `json:"scale"`
	// Networks are the networks the service joins, by key; the values
	// (aliases and addresses) are not read.
	Networks map[string]json.RawMessage `json:"networks"`

	Logging *Logging `json:"logging"`
}

// Logging is the logging section of a service.
type Logging struct {
	Driver string `json:"driver"`
}

// Build is the build section of a service.
type Build struct {
	Context            string            `json:"context"`
	Dockerfile         string            `json:"dockerfile"`
	AdditionalContexts map[string]string `json:"additional_contexts"`
	Network            string            `json:"network"`
	Privileged         bool              `json:"privileged"`
	CacheFrom          []string          `json:"cache_from"`
	CacheTo            []string          `json:"cache_to"`

	// raw is the whole build section as Compose printed it, for comparing
	// build definitions (see CompareBuilds). It may hold interpolated
	// values of the app's variables and must never be logged.
	raw json.RawMessage
}

// UnmarshalJSON decodes the fields koroku inspects and keeps the raw section.
func (b *Build) UnmarshalJSON(data []byte) error {
	type plain Build
	if err := json.Unmarshal(data, (*plain)(b)); err != nil {
		return err
	}
	b.raw = append(json.RawMessage(nil), data...)
	return nil
}

// EnvFile is one env_file entry of a service.
type EnvFile struct {
	Path string `json:"path"`
}

// UnmarshalJSON accepts both the object form and a bare path.
func (e *EnvFile) UnmarshalJSON(data []byte) error {
	var path string
	if err := json.Unmarshal(data, &path); err == nil {
		e.Path = path
		return nil
	}
	type plain EnvFile
	return json.Unmarshal(data, (*plain)(e))
}

// Network is a top-level network.
type Network struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	External bool   `json:"external"`
}

// FileObject is a top-level secret or config.
type FileObject struct {
	Name     string `json:"name"`
	File     string `json:"file"`
	External bool   `json:"external"`
}

// HasBuild reports whether the service is built from source.
func (s Service) HasBuild() bool { return s.Build != nil }

// DeployMemoryLimit returns deploy.resources.limits.memory, or "".
func (s Service) DeployMemoryLimit() string {
	if s.Deploy == nil || s.Deploy.Resources == nil || s.Deploy.Resources.Limits == nil {
		return ""
	}
	return s.Deploy.Resources.Limits.Memory
}

// DeployMemoryReservation returns deploy.resources.reservations.memory, or "".
func (s Service) DeployMemoryReservation() string {
	if s.Deploy == nil || s.Deploy.Resources == nil || s.Deploy.Resources.Reservations == nil {
		return ""
	}
	return s.Deploy.Resources.Reservations.Memory
}

// Port is one published port.
type Port struct {
	HostIP    string `json:"host_ip"`
	Target    int    `json:"target"`
	Published string `json:"published"`
	Protocol  string `json:"protocol"`
}

// Mount is one volume entry of a service.
type Mount struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only"`
}

// Deploy mirrors the part of the deploy section that carries resource
// limits and the number of containers.
type Deploy struct {
	Resources *Resources `json:"resources"`
	Replicas  *int       `json:"replicas"`
}

// Resources holds resource limits and reservations.
type Resources struct {
	Limits       *Limits `json:"limits"`
	Reservations *Limits `json:"reservations"`
}

// Limits holds a memory limit or reservation.
type Limits struct {
	Memory string `json:"memory"`
}

// Volume is a top-level named volume.
type Volume struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	External   bool              `json:"external"`
	DriverOpts map[string]string `json:"driver_opts"`
}

// ParseProject decodes the output of `docker compose config --format json`.
func ParseProject(data []byte) (Project, error) {
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		// The input may contain secrets, so the error carries no excerpt.
		return Project{}, errors.New("parse compose config: output is not the expected JSON")
	}
	if len(p.Services) == 0 {
		return Project{}, errors.New("parse compose config: the project defines no services")
	}
	var raw struct {
		Services map[string]json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return Project{}, errors.New("parse compose config: output is not the expected JSON")
	}
	for name, svc := range p.Services {
		svc.Raw = raw.Services[name]
		p.Services[name] = svc
	}
	p.Raw = append(json.RawMessage(nil), data...)
	return p, nil
}

// ServiceNames returns the service names in sorted order.
func (p Project) ServiceNames() []string {
	names := make([]string, 0, len(p.Services))
	for name := range p.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Dependencies returns the services that service depends on, directly or
// through other services, in sorted order. The service itself is not
// included, even when a cycle leads back to it.
func (p Project) Dependencies(service string) []string {
	seen := map[string]bool{service: true}
	var walk func(name string)
	var deps []string
	walk = func(name string) {
		for dep := range p.Services[name].DependsOn {
			if seen[dep] {
				continue
			}
			seen[dep] = true
			deps = append(deps, dep)
			walk(dep)
		}
	}
	walk(service)
	sort.Strings(deps)
	return deps
}

// OneShot reports whether another service waits for this one to finish
// (depends_on with condition service_completed_successfully): a migration
// or seed that runs once per deployment and exits (ARCHITECTURE 7). It
// uses no memory after that, so it gets no reservation (ARCHITECTURE 6.2).
func (p Project) OneShot(service string) bool {
	for _, other := range p.Services {
		raw, ok := other.DependsOn[service]
		if !ok {
			continue
		}
		var dep struct {
			Condition string `json:"condition"`
		}
		if json.Unmarshal(raw, &dep) == nil && dep.Condition == "service_completed_successfully" {
			return true
		}
	}
	return false
}

// WithoutServices returns a copy of the project without the named
// services. The services map is new; the services themselves are shared.
func (p Project) WithoutServices(names ...string) Project {
	out := p
	out.Services = make(map[string]Service, len(p.Services))
	for name, svc := range p.Services {
		if !slices.Contains(names, name) {
			out.Services[name] = svc
		}
	}
	return out
}

// BuiltServices lists, sorted, the services with a build section.
func (p Project) BuiltServices() []string {
	var out []string
	for _, name := range p.ServiceNames() {
		if p.Services[name].HasBuild() {
			out = append(out, name)
		}
	}
	return out
}

// NeedsBuild reports whether any service has a build section.
func (p Project) NeedsBuild() bool {
	for _, s := range p.Services {
		if s.HasBuild() {
			return true
		}
	}
	return false
}

// PulledImages lists, sorted and without duplicates, the images of the
// services without a build section: the ones `pull` fetches. In a merged
// and interpolated project they are the references as Docker stores them
// (such as ghcr.io/octo/app:sha-0123abc).
func (p Project) PulledImages() []string {
	seen := map[string]bool{}
	var images []string
	for _, s := range p.Services {
		if !s.HasBuild() && s.Image != "" && !seen[s.Image] {
			seen[s.Image] = true
			images = append(images, s.Image)
		}
	}
	sort.Strings(images)
	return images
}

// NeedsPull reports whether any service runs a prebuilt image.
func (p Project) NeedsPull() bool {
	for _, s := range p.Services {
		if !s.HasBuild() {
			return true
		}
	}
	return false
}

// defaultFiles are tried in order when the app does not configure a path.
var defaultFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

// DefaultFiles returns the Compose file names tried when an app does not
// configure a path, in order of preference.
func DefaultFiles() []string { return append([]string(nil), defaultFiles...) }

// DetectFile returns the Compose file to use, relative to the release
// directory. A configured path must stay inside the release directory.
func DetectFile(releaseDir, configured string) (string, error) {
	if configured != "" {
		clean := filepath.Clean(configured)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("compose file %q must be a path inside the repository", configured)
		}
		if !isRegularFile(filepath.Join(releaseDir, clean)) {
			return "", fmt.Errorf("compose file %q was not found in the repository", configured)
		}
		// A directory on the way may be a symlink out of the checkout.
		if !isInside(resolvePath(releaseDir), resolvePath(filepath.Join(releaseDir, clean))) {
			return "", fmt.Errorf("compose file %q must be a path inside the repository", configured)
		}
		return clean, nil
	}
	for _, name := range defaultFiles {
		if isRegularFile(filepath.Join(releaseDir, name)) {
			return name, nil
		}
	}
	return "", fmt.Errorf("no Compose file found in the repository (looked for %s)", strings.Join(defaultFiles, ", "))
}

func isRegularFile(path string) bool {
	// Lstat: a symlink could point outside the repository.
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular()
}

// Invocation describes how `docker compose` is called for one release.
type Invocation struct {
	// ProjectName is passed with -p; it namespaces containers and volumes.
	ProjectName string
	// Files are passed with -f in order (the repository file, then the override).
	Files []string
	// EnvFile is passed with --env-file for ${VAR} interpolation. With this
	// flag Compose does not read the repository's own .env for interpolation.
	EnvFile string
	// ReleaseEnvFile, when set, is passed with a second --env-file: the
	// commit variables live in a file of their own (ARCHITECTURE 6), and
	// interpolation sees both.
	ReleaseEnvFile string
	// Profiles are passed with --profile. Configuration, build and pull
	// calls of a blue/green release name DisabledProfile, so that the
	// public service the override disables is part of the project.
	Profiles []string
	// BlueGreen marks a call on a release in the blue/green form
	// (ARCHITECTURE 7.2): configuration calls add DisabledProfile.
	BlueGreen bool
	// DockerConfig is the Docker client config directory holding the
	// app's registry credential (ARCHITECTURE 6.1). Empty means the
	// default config of the user koroku runs as.
	DockerConfig string
}

// Env returns the environment entries the call needs on top of koroku's
// own: DOCKER_CONFIG when the app has a registry credential. The
// credential is in the file the variable points to, never in arguments.
func (i Invocation) Env() []string {
	if i.DockerConfig == "" {
		return nil
	}
	return []string{"DOCKER_CONFIG=" + i.DockerConfig}
}

// Args returns the full argument list for `docker`, ending with sub.
func (i Invocation) Args(sub ...string) []string {
	args := []string{"compose", "-p", i.ProjectName}
	for _, f := range i.Files {
		args = append(args, "-f", f)
	}
	if i.EnvFile != "" {
		args = append(args, "--env-file", i.EnvFile)
	}
	if i.ReleaseEnvFile != "" {
		args = append(args, "--env-file", i.ReleaseEnvFile)
	}
	for _, profile := range i.Profiles {
		args = append(args, "--profile", profile)
	}
	return append(args, sub...)
}
