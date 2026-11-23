package compose

import (
	"bytes"
	"slices"
	"sort"
)

// The commit variables change with every deployment. They live in an env
// file of their own, given only to the services that every deployment
// recreates anyway, so that the others (a database, a cache) keep their
// configuration across code-only deployments (ARCHITECTURE 6).

// ReleaseEnvServices lists, sorted, the services that read the release env
// file: the public service (and its coloured copies in the blue/green
// form), the one-shot services, the built services (their image is per
// release) and the services whose definition follows the commit
// (CommitServices). It is nil without a release env file.
func (p OverrideParams) ReleaseEnvServices() []string {
	if p.ReleaseEnvFile == "" {
		return nil
	}
	set := map[string]bool{p.WebService: true}
	if p.BlueGreen != nil {
		for _, name := range ColoredNames(p.WebService) {
			set[name] = true
		}
	}
	for _, name := range p.Project.ServiceNames() {
		svc := p.Project.Services[name]
		if svc.HasBuild() || p.Project.OneShot(name) || slices.Contains(p.CommitServices, name) {
			set[name] = true
		}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CommitServices lists, sorted, the services without a build section
// whose definition differs between two normalizations of the same project
// that only differ in the commit variables: they refer to the commit by
// interpolation, in their image (`image: ...:sha-${KOROKU_COMMIT_SHORT}`),
// their environment (`KOROKU_COMMIT_SHA: ${KOROKU_COMMIT_SHA}`) or
// anywhere else.
func CommitServices(actual, probe Project) []string {
	var out []string
	for _, name := range actual.ServiceNames() {
		svc := actual.Services[name]
		if svc.HasBuild() {
			continue
		}
		other, ok := probe.Services[name]
		if !ok {
			continue
		}
		if other.Image != svc.Image || !bytes.Equal(other.Raw, svc.Raw) {
			out = append(out, name)
		}
	}
	return out
}
