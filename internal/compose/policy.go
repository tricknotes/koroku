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

	// Groups (ARCHITECTURE 6.1). Neither is waived by the relax flag.
	RuleGroupNetwork      = "group_network"
	RuleGroupNameConflict = "group_name_conflict"
)

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
	// Relax waives the relaxable rules for this app.
	Relax bool
	// GroupNetwork is the network of the app's group, or "" outside a
	// group, and GroupServices the services allowed on it.
	GroupNetwork  string
	GroupServices []string
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
// would be reachable from outside.
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
		if svc.Privileged {
			add(name, RulePrivileged, "privileged: true is not allowed", true)
		}
		for key := range svc.Networks {
			network := p.Networks[key]
			if params.GroupNetwork != "" && network.Name == params.GroupNetwork && !slices.Contains(params.GroupServices, name) {
				add(name, RuleGroupNetwork, fmt.Sprintf("the service is on the group network %q but was not chosen to join it", network.Name), false)
			}
		}
		if svc.NetworkMode == "host" {
			add(name, RuleHostNetwork, "network_mode: host is not allowed", true)
		}
		if svc.Pid == "host" {
			add(name, RuleHostPID, "pid: host is not allowed", true)
		}
		for _, m := range svc.Volumes {
			if m.Type != "bind" {
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
		for _, port := range svc.Ports {
			if name == params.WebService && isWebBinding(port, params) {
				continue
			}
			add(name, RulePortExposure,
				fmt.Sprintf("unexpected published port %s (only the web service may publish, on loopback)", describePort(port)), false)
		}
	}

	for _, name := range p.ServiceNames() {
		svc := p.Services[name]
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
		}
		if len(svc.Devices) > 0 || len(svc.DeviceCgroupRules) > 0 {
			add(name, RuleDevice, "host devices are not allowed", true)
		}
		for _, opt := range svc.SecurityOpt {
			lowered := strings.ToLower(opt)
			if strings.Contains(lowered, "unconfined") || strings.Contains(lowered, "disable") || strings.Contains(lowered, "systempaths") {
				add(name, RuleSecurityOpt, fmt.Sprintf("security_opt: %s weakens the container sandbox", opt), true)
			}
		}
		for _, envFile := range svc.EnvFiles {
			if envFile.Path == params.EnvFile && params.EnvFile != "" {
				continue
			}
			if !isInside(releaseDir, resolvePath(envFile.Path)) {
				add(name, RuleFileOutside, fmt.Sprintf("env_file %s is outside the repository checkout", envFile.Path), true)
			}
		}
		if svc.Build != nil {
			paths := map[string]string{"build context": svc.Build.Context}
			if filepath.IsAbs(svc.Build.Dockerfile) {
				paths["Dockerfile"] = svc.Build.Dockerfile
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

	for name, volume := range p.Volumes {
		if device := volume.DriverOpts["device"]; strings.HasPrefix(device, "/") {
			add("", RuleVolumeHostPath, fmt.Sprintf("volume %q mounts the host path %s through driver options", name, device), true)
		}
		if params.ProjectName != "" && (volume.External || !strings.HasPrefix(volume.Name, params.ProjectName+"_")) {
			add("", RuleForeignVolume, fmt.Sprintf("volume %q refers to %q, which does not belong to this app", name, volume.Name), true)
		}
	}
	for name, network := range p.Networks {
		if strings.HasPrefix(network.Name, GroupNetworkPrefix) {
			// Only the app's own group network, which koroku created; the
			// relax flag must not reach another group (ARCHITECTURE 6.1).
			if params.GroupNetwork == "" || network.Name != params.GroupNetwork || !network.External {
				add("", RuleGroupNetwork, fmt.Sprintf("network %q refers to the group network %q, which this app may not join", name, network.Name), false)
			}
			continue
		}
		if params.ProjectName != "" && (network.External || !strings.HasPrefix(network.Name, params.ProjectName+"_")) {
			add("", RuleForeignNetwork, fmt.Sprintf("network %q refers to %q, which does not belong to this app", name, network.Name), true)
		}
	}
	for kind, objects := range map[string]map[string]FileObject{"secret": p.Secrets, "config": p.Configs} {
		for name, object := range objects {
			if object.File != "" && !isInside(releaseDir, resolvePath(object.File)) {
				add("", RuleFileOutside, fmt.Sprintf("%s %q reads %s, which is outside the repository checkout", kind, name, object.File), true)
			}
		}
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

func isWebBinding(port Port, params PolicyParams) bool {
	return port.HostIP == "127.0.0.1" &&
		port.Target == params.WebPort &&
		port.Published == strconv.Itoa(params.HostPort) &&
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
