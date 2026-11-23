package compose

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Policy rule identifiers.
const (
	RuleWebServiceMissing = "web_service_missing"
	RulePrivileged        = "privileged"
	RuleHostNetwork       = "host_network"
	RuleHostPID           = "host_pid"
	RuleDockerSocket      = "docker_socket"
	RuleBindOutside       = "bind_outside_release"
	RulePortExposure      = "port_exposure"

	// Rules beyond the original list: other ways a Compose file reaches the
	// host or another app.
	RuleVolumeHostPath   = "volume_host_path"
	RuleForeignVolume    = "foreign_volume"
	RuleForeignNetwork   = "foreign_network"
	RuleHostNamespace    = "host_namespace"
	RuleForeignNamespace = "foreign_namespace"
	RuleCapability       = "capability"
	RuleDevice           = "device"
	RuleSecurityOpt      = "security_opt"
	RuleFileOutside      = "file_outside_release"
	RuleBindWritable     = "bind_writable"
	RuleRuntime          = "runtime"
	RuleMemoryGuard      = "memory_guard"
	// RuleForeignImage is an image koroku built for an app, named by a
	// service that does not build it.
	RuleForeignImage = "foreign_image"
	// RuleUnsupported is a setting that is not on the list of the ones
	// koroku allows (see schema.go), or a value of an allowed setting that
	// no other rule names.
	RuleUnsupported = "unsupported_setting"
)

// releaseImagePrefix starts the name of every image koroku builds
// (ReleaseImage): the prefix of the Compose project names.
const releaseImagePrefix = "koroku-"

// allowedCapabilities are Docker's default capabilities plus two that
// ordinary services ask for (memory locking, scheduling priority). Anything
// else changes what the container can do to the host.
var allowedCapabilities = map[string]bool{
	"AUDIT_WRITE": true, "CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "FSETID": true,
	"KILL": true, "MKNOD": true, "NET_BIND_SERVICE": true, "NET_RAW": true, "SETFCAP": true,
	"SETGID": true, "SETPCAP": true, "SETUID": true, "SYS_CHROOT": true,
	"IPC_LOCK": true, "SYS_NICE": true,
}

// Violation is one policy finding.
type Violation struct {
	Service string `json:"service"`
	Rule    string `json:"rule"`
	Message string `json:"message"`
	// Relaxable findings are waived by the app's "relax policy" flag.
	Relaxable bool `json:"relaxable"`
}

func (v Violation) String() string {
	if v.Service == "" {
		return fmt.Sprintf("[%s] %s", v.Rule, v.Message)
	}
	return fmt.Sprintf("[%s] service %q: %s", v.Rule, v.Service, v.Message)
}

// PolicyParams is the context a project is checked against.
type PolicyParams struct {
	// ReleaseDir is the checkout; bind mounts must stay inside it.
	ReleaseDir string
	// ProjectName prefixes the volumes and networks that belong to the app.
	// Empty skips the ownership rules.
	ProjectName string
	// EnvFile is the platform's env file, the one env_file outside the
	// checkout that is expected.
	EnvFile    string
	WebService string
	WebPort    int
	HostPort   int
	// ReleaseEnvFile is the second env file koroku injects, with the
	// commit variables (ARCHITECTURE 6).
	ReleaseEnvFile string
	// BlueGreen checks a release in the blue/green form (ARCHITECTURE
	// 7.2): the coloured copies of the public service publish on HostPort
	// (blue) and AltHostPort (green), and the public service itself
	// publishes nothing.
	BlueGreen   bool
	AltHostPort int
	// Relax waives the relaxable rules for this app.
	Relax bool
}

// allowedBinding returns the host port a service of the merged project may
// publish on loopback, or 0 when it may publish nothing.
func (params PolicyParams) allowedBinding(service string) int {
	if !params.BlueGreen {
		if service == params.WebService {
			return params.HostPort
		}
		return 0
	}
	switch service {
	case ColoredName(params.WebService, ColorBlue):
		return params.HostPort
	case ColoredName(params.WebService, ColorGreen):
		return params.AltHostPort
	}
	return 0
}

// PolicyResult separates findings that block the deploy from waived ones.
type PolicyResult struct {
	Violations []Violation
	Waived     []Violation
}

// OK reports whether the deploy may proceed.
func (r PolicyResult) OK() bool { return len(r.Violations) == 0 }

// PolicyError is returned by the deploy engine when the policy blocks a deploy.
type PolicyError struct {
	Violations []Violation
}

func (e *PolicyError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = v.String()
	}
	return "policy check failed: " + strings.Join(parts, "; ")
}

// CheckPolicy inspects the normalized project (repository file plus override)
// against ARCHITECTURE chapter 6.
//
// Port exposure is never waived: with Docker, a published port bypasses the
// host firewall, so anything other than the web service's loopback binding
// would be reachable from outside. Neither is the stop grace limit
// (RuleStopGracePeriod), which protects other apps and koroku's timeouts.
func CheckPolicy(p Project, params PolicyParams) PolicyResult {
	var found []Violation
	add := func(service, rule, message string, relaxable bool) {
		found = append(found, Violation{Service: service, Rule: rule, Message: message, Relaxable: relaxable})
	}

	if _, ok := p.Services[params.WebService]; !ok {
		add("", RuleWebServiceMissing,
			fmt.Sprintf("web service %q is not defined in the Compose file (services: %s)",
				params.WebService, strings.Join(p.ServiceNames(), ", ")), false)
	}

	releaseDir := resolvePath(params.ReleaseDir)
	for _, name := range p.ServiceNames() {
		svc := p.Services[name]
		for _, port := range svc.Ports {
			if allowed := params.allowedBinding(name); allowed != 0 && isWebBinding(port, params.WebPort, allowed) {
				continue
			}
			add(name, RulePortExposure,
				fmt.Sprintf("unexpected published port %s (only the web service may publish, on loopback)", describePort(port)), false)
		}
		if params.BlueGreen && isColored(name, params.WebService) {
			// A copy of the public service, which is checked itself.
			continue
		}
		if svc.Privileged {
			add(name, RulePrivileged, "privileged: true is not allowed", true)
		}
		if svc.NetworkMode == "host" {
			add(name, RuleHostNetwork, "network_mode: host is not allowed", true)
		}
		if svc.Pid == "host" {
			add(name, RuleHostPID, "pid: host is not allowed", true)
		}
		for _, m := range svc.Volumes {
			switch m.Type {
			case "bind":
			case "volume", "tmpfs":
				continue
			default:
				// An image mount reads any image on the host, another
				// app's included.
				add(name, RuleUnsupported, fmt.Sprintf("volumes: mount type %q is not allowed (use a named volume, a read-only bind mount of a file in the repository, or tmpfs)", m.Type), true)
				continue
			}
			source := resolvePath(m.Source)
			switch {
			case isDockerSocket(m.Source) || isDockerSocket(source):
				add(name, RuleDockerSocket, "mounting the Docker socket is not allowed", true)
			case !isInside(releaseDir, source):
				add(name, RuleBindOutside,
					fmt.Sprintf("bind mount of %s is outside the repository checkout", m.Source), true)
			case !m.ReadOnly:
				// A container that can write to the checkout can replace
				// the source of another bind mount with a symlink to any
				// host path (Docker follows it at the next start), and can
				// rewrite the Compose files koroku starts the app from.
				add(name, RuleBindWritable,
					fmt.Sprintf("bind mount of %s must be read-only (add :ro; keep data in a named volume)", m.Source), true)
			}
		}
	}

	for _, name := range p.ServiceNames() {
		svc := p.Services[name]
		if params.BlueGreen && isColored(name, params.WebService) {
			continue
		}
		for field, value := range map[string]string{"ipc": svc.Ipc, "uts": svc.Uts, "userns_mode": svc.UsernsMode, "cgroup": svc.Cgroup} {
			if value == "host" {
				add(name, RuleHostNamespace, fmt.Sprintf("%s: host is not allowed", field), true)
			}
		}
		// Joining a container by name crosses the project boundary; sharing
		// with a service of the same project (service:<name>) does not.
		for field, value := range map[string]string{"network_mode": svc.NetworkMode, "pid": svc.Pid, "ipc": svc.Ipc} {
			if strings.HasPrefix(value, "container:") {
				add(name, RuleForeignNamespace, fmt.Sprintf("%s: %s joins a container outside the app", field, value), true)
			}
		}
		// Every other value is either one of the few that stay inside the
		// app or a name Docker resolves on the host: network_mode takes the
		// name of any network there, another app's included.
		for _, setting := range []struct {
			field, value string
			service      bool
			allowed      []string
		}{
			{"network_mode", svc.NetworkMode, true, []string{"none"}},
			{"pid", svc.Pid, true, nil},
			{"ipc", svc.Ipc, true, []string{"none", "private", "shareable"}},
			{"uts", svc.Uts, false, nil},
			{"userns_mode", svc.UsernsMode, false, nil},
			{"cgroup", svc.Cgroup, false, []string{"private"}},
		} {
			value := setting.value
			if value == "" || value == "host" || strings.HasPrefix(value, "container:") || slices.Contains(setting.allowed, value) {
				continue
			}
			if other, ok := strings.CutPrefix(value, "service:"); ok && setting.service {
				if _, defined := p.Services[other]; defined {
					continue
				}
			}
			if setting.field == "network_mode" {
				add(name, RuleForeignNetwork, fmt.Sprintf("network_mode: %s joins a network that does not belong to this app (list the app's own networks under networks:)", value), true)
			} else {
				add(name, RuleForeignNamespace, fmt.Sprintf("%s: %s is not allowed", setting.field, value), true)
			}
		}
		for _, from := range svc.VolumesFrom {
			if strings.HasPrefix(from, "container:") {
				add(name, RuleForeignNamespace, fmt.Sprintf("volumes_from: %s takes volumes of a container outside the app", from), true)
			}
		}
		for _, capability := range svc.CapAdd {
			normalized := strings.TrimPrefix(strings.ToUpper(capability), "CAP_")
			if !allowedCapabilities[normalized] {
				add(name, RuleCapability, fmt.Sprintf("cap_add: %s is not allowed", capability), true)
			}
		}
		if svc.Runtime != "" && svc.Runtime != "runc" {
			add(name, RuleRuntime, fmt.Sprintf("runtime: %s is not allowed", svc.Runtime), true)
		}
		if svc.OomKillDisable {
			add(name, RuleMemoryGuard, "oom_kill_disable defeats the memory limit", true)
		}
		if svc.MemswapLimit == "-1" {
			add(name, RuleMemoryGuard, "memswap_limit: -1 allows unlimited swap beyond the memory limit", true)
		}
		if svc.Build != nil {
			// A build with host networking can talk to services that only
			// listen on the host's loopback, such as the proxy's admin API.
			if svc.Build.Network == "host" {
				add(name, RuleHostNetwork, "build.network: host is not allowed", true)
			}
			if svc.Build.Privileged {
				add(name, RulePrivileged, "build.privileged: true is not allowed", true)
			}
			if network := svc.Build.Network; network != "" && network != "host" && network != "default" && network != "none" {
				add(name, RuleUnsupported, fmt.Sprintf("build.network: %s is not allowed", network), true)
			}
			// A local cache is a directory on the host that the build
			// reads or fills as root.
			for _, cache := range []struct {
				field   string
				entries []string
			}{{"cache_from", svc.Build.CacheFrom}, {"cache_to", svc.Build.CacheTo}} {
				for _, entry := range cache.entries {
					if strings.Contains(strings.ToLower(strings.ReplaceAll(entry, " ", "")), "type=local") {
						add(name, RuleUnsupported, fmt.Sprintf("build.%s: a local cache directory is not allowed", cache.field), true)
					}
				}
			}
		}
		if svc.Logging != nil && !slices.Contains([]string{"", "json-file", "local", "none"}, svc.Logging.Driver) {
			// Other drivers send the output elsewhere (the host's
			// journal, a remote address); koroku reads it from Docker.
			add(name, RuleUnsupported, fmt.Sprintf("logging.driver: %s is not allowed (json-file, local or none)", svc.Logging.Driver), true)
		}
		if !svc.HasBuild() && params.ProjectName != "" && isForeignReleaseImage(svc.Image, params.ProjectName, p) {
			add(name, RuleForeignImage, fmt.Sprintf("image %s is one that koroku builds for an app; a service runs it only by building it (build:)", svc.Image), true)
		}
		if len(svc.Devices) > 0 || len(svc.DeviceCgroupRules) > 0 {
			add(name, RuleDevice, "host devices are not allowed", true)
		}
		for _, opt := range svc.SecurityOpt {
			// Only the option that tightens the sandbox is allowed.
			// The others loosen it or replace a profile, and a profile
			// read from a file of the repository allows whatever the
			// repository says.
			if !noNewPrivileges.MatchString(opt) {
				add(name, RuleSecurityOpt, fmt.Sprintf("security_opt: %s weakens the container sandbox", opt), true)
			}
		}
		for _, envFile := range svc.EnvFiles {
			if envFile.Path == params.EnvFile && params.EnvFile != "" {
				continue
			}
			if envFile.Path == params.ReleaseEnvFile && params.ReleaseEnvFile != "" {
				continue
			}
			if !isInside(releaseDir, resolvePath(envFile.Path)) {
				add(name, RuleFileOutside, fmt.Sprintf("env_file %s is outside the repository checkout", envFile.Path), true)
			}
		}
		if svc.Build != nil {
			paths := map[string]string{"build context": svc.Build.Context}
			switch dockerfile := svc.Build.Dockerfile; {
			case filepath.IsAbs(dockerfile):
				paths["Dockerfile"] = dockerfile
			case dockerfile != "" && filepath.IsAbs(svc.Build.Context):
				// Relative to the context, and free to leave it with
				// "..": the builder prints the lines it cannot parse.
				paths["Dockerfile"] = filepath.Join(svc.Build.Context, dockerfile)
			}
			for key, value := range svc.Build.AdditionalContexts {
				paths["additional context "+key] = value
			}
			for what, path := range paths {
				// Contexts may also be URLs or image references; only local
				// paths can pull host files into an image.
				if path == "" || !filepath.IsAbs(path) {
					continue
				}
				if !isInside(releaseDir, resolvePath(path)) {
					add(name, RuleFileOutside, fmt.Sprintf("%s %s is outside the repository checkout", what, path), true)
				}
			}
		}
	}

	// The old colour stops on its own after the switch, so the public
	// service counts once.
	graced := p
	if params.BlueGreen {
		graced = p.WithoutServices(ColoredNames(params.WebService)...)
	}
	if total := graced.TotalStopGracePeriod(); total > MaxStopGracePeriod {
		var set []ServiceGrace
		for _, name := range graced.ServiceNames() {
			if d, ok := graced.Services[name].StopGracePeriod(); ok {
				set = append(set, ServiceGrace{Service: name, Period: d})
			}
		}
		add("", RuleStopGracePeriod,
			fmt.Sprintf("stop_grace_period adds up to %s across the services (%s), above the limit of %s",
				total, DescribeGraces(set), MaxStopGracePeriod), false)
	}

	for name, volume := range p.Volumes {
		// The local driver hands its options to mount(2): a bind or an
		// overlay names host directories in "o" as well as in "device", and
		// a block device is the host's. Only the network and memory file
		// systems keep the volume off the host's files.
		kind := strings.ToLower(volume.DriverOpts["type"])
		switch device := volume.DriverOpts["device"]; {
		case strings.HasPrefix(device, "/"):
			add("", RuleVolumeHostPath, fmt.Sprintf("volume %q mounts the host path %s through driver options", name, device), true)
		case len(volume.DriverOpts) > 0 && kind != "nfs" && kind != "nfs4" && kind != "tmpfs":
			add("", RuleVolumeHostPath, fmt.Sprintf("volume %q uses driver options of type %q, which can mount host paths (only nfs, nfs4 and tmpfs are allowed)", name, volume.DriverOpts["type"]), true)
		}
		if volume.Driver != "" && volume.Driver != "local" {
			add("", RuleUnsupported, fmt.Sprintf("volume %q uses the driver %q (only the local driver is allowed)", name, volume.Driver), true)
		}
		if params.ProjectName != "" && (volume.External || !strings.HasPrefix(volume.Name, params.ProjectName+"_")) {
			add("", RuleForeignVolume, fmt.Sprintf("volume %q refers to %q, which does not belong to this app", name, volume.Name), true)
		}
	}
	for name, network := range p.Networks {
		if params.ProjectName != "" && (network.External || !strings.HasPrefix(network.Name, params.ProjectName+"_")) {
			add("", RuleForeignNetwork, fmt.Sprintf("network %q refers to %q, which does not belong to this app", name, network.Name), true)
		}
		// macvlan and ipvlan put containers on the host's own network.
		if network.Driver != "" && network.Driver != "bridge" {
			add("", RuleUnsupported, fmt.Sprintf("network %q uses the driver %q (only bridge is allowed)", name, network.Driver), true)
		}
	}
	for kind, objects := range map[string]map[string]FileObject{"secret": p.Secrets, "config": p.Configs} {
		for name, object := range objects {
			if object.File != "" && !isInside(releaseDir, resolvePath(object.File)) {
				add("", RuleFileOutside, fmt.Sprintf("%s %q reads %s, which is outside the repository checkout", kind, name, object.File), true)
			}
		}
	}

	for _, u := range unsupportedSettings(p.Raw, func(service string) bool {
		return params.BlueGreen && isColored(service, params.WebService)
	}) {
		add(u.Service, RuleUnsupported, fmt.Sprintf("%s is not a setting koroku allows", u.Path), true)
	}

	// Map iteration above is unordered; the report must not be.
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Service != found[j].Service {
			return found[i].Service < found[j].Service
		}
		if found[i].Rule != found[j].Rule {
			return found[i].Rule < found[j].Rule
		}
		return found[i].Message < found[j].Message
	})
	var result PolicyResult
	for _, v := range found {
		if v.Relaxable && params.Relax {
			result.Waived = append(result.Waived, v)
		} else {
			result.Violations = append(result.Violations, v)
		}
	}
	return result
}

func isWebBinding(port Port, webPort, hostPort int) bool {
	return port.HostIP == "127.0.0.1" &&
		port.Target == webPort &&
		port.Published == strconv.Itoa(hostPort) &&
		(port.Protocol == "" || port.Protocol == "tcp")
}

func describePort(port Port) string {
	host := port.HostIP
	if host == "" {
		host = "0.0.0.0"
	}
	published := port.Published
	if published == "" {
		published = "(random)"
	}
	return fmt.Sprintf("%s:%s->%d", host, published, port.Target)
}

// noNewPrivileges matches the security_opt forms of no-new-privileges.
var noNewPrivileges = regexp.MustCompile(`(?i)^no-new-privileges([:=](true|false))?$`)

// isForeignReleaseImage reports whether image names an image koroku builds
// for an app (ReleaseImage) other than one of this project's services.
// Every app's images live side by side in the host's Docker, so naming one
// is enough to run or read another app's build. The coloured copies of a
// blue/green release run the image of the service they copy.
func isForeignReleaseImage(image, projectName string, p Project) bool {
	repo := strings.ToLower(image)
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, prefix := range []string{"docker.io/", "index.docker.io/", "library/"} {
		repo = strings.TrimPrefix(repo, prefix)
	}
	if !strings.HasPrefix(repo, releaseImagePrefix) {
		return false
	}
	for name := range p.Services {
		if repo == strings.ToLower(projectName+"-"+name) {
			return false
		}
	}
	return true
}

var dockerSocketPattern = regexp.MustCompile(`(^|/)docker\.sock$`)

func isDockerSocket(path string) bool {
	return dockerSocketPattern.MatchString(filepath.Clean(path))
}

// resolvePath cleans a path and follows symlinks, so that a symlink inside
// the checkout cannot smuggle a host directory in. When the path does not
// exist yet (Docker creates missing bind sources), the deepest existing
// ancestor is resolved and the rest appended: `link/new` with link -> / is
// /new, not a path inside the checkout.
func resolvePath(path string) string {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return clean
	}
	rest := ""
	current := clean
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return clean
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}

func isInside(dir, path string) bool {
	if dir == "" || !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
