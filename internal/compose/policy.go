package compose

import (
	"fmt"
	"path/filepath"
	"regexp"
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
)

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
	WebService string
	WebPort    int
	HostPort   int
	// Relax waives the relaxable rules for this app.
	Relax bool
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

	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Service != found[j].Service {
			return found[i].Service < found[j].Service
		}
		return found[i].Rule < found[j].Rule
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

// resolvePath cleans a path and follows symlinks where the path exists, so
// that a symlink inside the checkout cannot smuggle a host directory in.
func resolvePath(path string) string {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved
	}
	return clean
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
