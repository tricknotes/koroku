package compose

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
)

// Groups (ARCHITECTURE 6.1): the chosen services of the apps in a group are
// attached to a network koroku creates, and reach each other as
// <service>.<slug>.koroku.internal. koroku attaches them with
// `docker network connect --alias`, outside the Compose configuration: a
// network joined through Compose also carries the bare service name, which
// can resolve to another app's service of the same name.

// GroupNetworkPrefix starts the name of every group network. Slugs cannot
// contain "_", so no app's own network (koroku-<slug>_*) starts with it.
// It must match store.GroupNetworkPrefix.
const GroupNetworkPrefix = "koroku_group_"

var groupServicePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidGroupService reports whether a service name can join a group: it
// becomes a DNS label in the service's hostname.
func ValidGroupService(name string) bool { return groupServicePattern.MatchString(name) }

// GroupHostname is the name other members use to reach a joined service.
func GroupHostname(service, slug string) string {
	return service + "." + slug + ".koroku.internal"
}

// CheckGroupServices reports the chosen services that cannot join the group
// network: missing from the project, a name that is not a DNS label, a
// container_name (it would put an arbitrary name on the shared network) or
// a network_mode (the service has no network of its own).
func CheckGroupServices(p Project, services []string) []Violation {
	sorted := slices.Clone(services)
	sort.Strings(sorted)
	var found []Violation
	add := func(service, message string) {
		found = append(found, Violation{Service: service, Rule: RuleGroupService, Message: message})
	}
	for _, name := range sorted {
		svc, ok := p.Services[name]
		switch {
		case !ok:
			add(name, "the service joins the group but is not defined in the Compose file")
		case !ValidGroupService(name):
			add(name, "the service name is not a valid DNS label, so it cannot join a group")
		case svc.ContainerName != "":
			add(name, fmt.Sprintf("the service sets container_name %q, so it cannot join a group", svc.ContainerName))
		case svc.NetworkMode != "":
			add(name, fmt.Sprintf("the service uses network_mode %q, so it cannot join a group", svc.NetworkMode))
		}
	}
	return found
}
