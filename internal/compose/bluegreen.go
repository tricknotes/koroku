package compose

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Blue/green deploys of the public service (ARCHITECTURE 7.2): the override
// copies the public service into two coloured services that differ only in
// the host port, and disables the service itself with a profile.

// DisabledProfile is the profile that keeps the public service of a
// blue/green release out of `up` and `start`. It stays usable by name
// (`build web`, `run --no-deps web`), and configuration calls activate it so
// that the service is part of the normalized project.
const DisabledProfile = "koroku-disabled"

// The colours of the copies of the public service.
const (
	ColorBlue  = "blue"
	ColorGreen = "green"
)

// Colors lists the colours in the order the override writes them.
var Colors = []string{ColorBlue, ColorGreen}

// ColoredName is the name of the copy of service in one colour.
func ColoredName(service, color string) string { return service + "-" + color }

// ColoredNames are both coloured copies of service.
func ColoredNames(service string) []string {
	return []string{ColoredName(service, ColorBlue), ColoredName(service, ColorGreen)}
}

// RuleBlueGreen rejects a Compose file whose public service cannot run
// twice side by side. The relax flag does not waive it: a project that
// breaks the conditions fails in ways nobody sees from outside the app.
const RuleBlueGreen = "blue_green"

// BlueGreenParams turn the override into the blue/green form.
type BlueGreenParams struct {
	// ComposeFile is the repository's Compose file relative to the release
	// directory, where the override lives: the coloured services copy the
	// public service from it with extends.
	ComposeFile string
	// AltHostPort is where green is published; blue uses HostPort.
	AltHostPort int
}

// CheckBlueGreen checks the conditions of ARCHITECTURE 7.2 on the
// repository's project alone (without the override): the public service
// web can be copied and run twice, and nothing else refers to it by name.
func CheckBlueGreen(p Project, web string) []Violation {
	var found []Violation
	add := func(service, message string) {
		found = append(found, Violation{Service: service, Rule: RuleBlueGreen, Message: message})
	}
	svc, ok := p.Services[web]
	if !ok {
		return nil // reported by RuleWebServiceMissing
	}
	if svc.ContainerName != "" {
		add(web, fmt.Sprintf("container_name %s would be used by both colours at once; remove it", svc.ContainerName))
	}
	if p.OneShot(web) {
		add(web, "the public service is a one-shot service that another service waits for")
	}
	if n := replicas(svc); n > 1 {
		add(web, fmt.Sprintf("the public service runs %d containers; blue/green replaces one", n))
	}
	for _, colored := range ColoredNames(web) {
		if _, taken := p.Services[colored]; taken {
			add(colored, fmt.Sprintf("the service name %s is used for a colour of the public service", colored))
		}
	}
	for _, name := range p.ServiceNames() {
		if name == web {
			continue
		}
		other := p.Services[name]
		var refs []string
		if _, ok := other.DependsOn[web]; ok {
			refs = append(refs, "depends_on")
		}
		for field, value := range map[string]string{"network_mode": other.NetworkMode, "ipc": other.Ipc, "pid": other.Pid} {
			if value == "service:"+web {
				refs = append(refs, field)
			}
		}
		for _, from := range other.VolumesFrom {
			source := strings.TrimPrefix(from, "service:")
			if i := strings.IndexByte(source, ':'); i >= 0 {
				source = source[:i]
			}
			if source == web {
				refs = append(refs, "volumes_from")
				break
			}
		}
		if len(refs) > 0 {
			sort.Strings(refs)
			add(name, fmt.Sprintf("refers to the public service %s (%s), which blue/green deploys disable", web, strings.Join(refs, ", ")))
		}
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].Service != found[j].Service {
			return found[i].Service < found[j].Service
		}
		return found[i].Message < found[j].Message
	})
	return found
}

func replicas(svc Service) int {
	n := 1
	if svc.Scale != nil {
		n = *svc.Scale
	}
	if svc.Deploy != nil && svc.Deploy.Replicas != nil {
		n = *svc.Deploy.Replicas
	}
	return n
}

// ExtendsFailure reports whether Compose output says that a coloured copy
// of web could not be made because the Compose file does not define web
// itself (it comes from an included file): `cannot extend service
// "web-blue" in ...: service "web" not found in compose.yaml`.
func ExtendsFailure(output, web string) bool {
	for _, colored := range ColoredNames(web) {
		if strings.Contains(output, fmt.Sprintf("cannot extend service %q", colored)) {
			return true
		}
	}
	return false
}

// IncludedViolation is the RuleBlueGreen finding for a public service the
// Compose file takes from an included file.
func IncludedViolation(web, composeFile string) Violation {
	return Violation{Service: web, Rule: RuleBlueGreen,
		Message: fmt.Sprintf("the public service must be defined in %s itself, not in a file it includes", composeFile)}
}

// colorNetworks renders the networks a coloured copy joins with the name
// of the public service as an alias, so that other services reach it by
// that name. A service with a network_mode joins no networks.
func colorNetworks(b *strings.Builder, web string, svc Service) {
	if svc.NetworkMode != "" {
		return
	}
	names := make([]string, 0, len(svc.Networks))
	for name := range svc.Networks {
		names = append(names, name)
	}
	if len(names) == 0 {
		names = []string{"default"}
	}
	sort.Strings(names)
	b.WriteString("    networks:\n")
	for _, name := range names {
		fmt.Fprintf(b, "      %s:\n        aliases:\n          - %s\n", quote(name), quote(web))
	}
}

// isColored reports whether name is a coloured copy of web.
func isColored(name, web string) bool { return slices.Contains(ColoredNames(web), name) }
