package compose

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
)

// Links between apps (ARCHITECTURE 6.1): a name in the consumer's Compose
// file means a service another app in the group exposes. koroku creates a
// network per link and attaches the target service under that name with
// `docker network connect --alias`, outside the Compose configuration: a
// network joined through Compose also carries the bare service name, which
// can resolve to another app's service of the same name.

// LinkNetworkPrefix starts the name of every link network. It must match
// store.LinkNetworkPrefix.
const LinkNetworkPrefix = "koroku_link_"

// dnsLabelPattern is a DNS label: at most 63 characters.
var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidLinkName reports whether a service or link name can be used on a
// link network: it must be a DNS label.
func ValidLinkName(name string) bool { return dnsLabelPattern.MatchString(name) }

// LinkUse is what a consumer app asks of one link.
type LinkUse struct {
	Name     string
	Services []string
}

// CheckLinkServices reports the services that cannot be attached to a link
// network: missing from the project, a name that is not a DNS label, a
// container_name (it would put an arbitrary name on the network) or a
// network_mode (the service has no network of its own). It covers both the
// services an app exposes and the ones that use its links.
func CheckLinkServices(p Project, services []string) []Violation {
	sorted := slices.Clone(services)
	sort.Strings(sorted)
	var found []Violation
	for _, name := range slices.Compact(sorted) {
		if msg := linkServiceProblem(p, name); msg != "" {
			found = append(found, Violation{Service: name, Rule: RuleLinkService, Message: msg})
		}
	}
	return found
}

func linkServiceProblem(p Project, name string) string {
	svc, ok := p.Services[name]
	switch {
	case !ok:
		return "the service is not defined in the Compose file"
	case !ValidLinkName(name):
		return "the service name is not a valid DNS label, so it cannot be linked"
	case svc.ContainerName != "":
		return fmt.Sprintf("the service sets container_name %q, so it cannot be linked", svc.ContainerName)
	case svc.NetworkMode != "":
		return fmt.Sprintf("the service uses network_mode %q, so it cannot be linked", svc.NetworkMode)
	}
	return ""
}

// CheckLinks checks the consumer side of an app's links against its
// project: the services that use each link, and that no link name is also a
// name of the app's own (a stopped service of that name would let the name
// resolve to the other app instead).
func CheckLinks(p Project, links []LinkUse) []Violation {
	own := map[string]bool{}
	for name, svc := range p.Services {
		own[name] = true
		if svc.ContainerName != "" {
			own[svc.ContainerName] = true
		}
	}
	var services []string
	var found []Violation
	for _, link := range links {
		services = append(services, link.Services...)
		if own[link.Name] {
			found = append(found, Violation{Service: link.Name, Rule: RuleLinkService,
				Message: fmt.Sprintf("the link name %q is also a service of this app; rename the service or the link", link.Name)})
		}
	}
	return append(found, CheckLinkServices(p, services)...)
}
