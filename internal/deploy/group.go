package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// Groups (ARCHITECTURE 6.1). The group part of the override is written
// from the database right before every `up`, never stored in a release, so
// a rollback cannot bring back a membership the app no longer has.

// UnknownServicesError is returned when services chosen for a group are not
// in the app's running release.
type UnknownServicesError struct {
	Services []string
}

func (e *UnknownServicesError) Error() string {
	return "services not defined in the running release: " + strings.Join(e.Services, ", ")
}

// groupResult is what applyGroup found out about the app's group.
type groupResult struct {
	// Network is the group network, or "" when the app is in no group.
	Network  string
	Services []string
	// Conflicts are names that could resolve to another member.
	Conflicts []compose.Violation
}

func (g groupResult) policyParams(p compose.PolicyParams) compose.PolicyParams {
	p.GroupNetwork = g.Network
	p.GroupServices = g.Services
	return p
}

func (e *Engine) groupOverridePath(slug string) string {
	return filepath.Join(e.appDir(slug), compose.GroupOverrideFileName)
}

// applyGroup writes or removes the app's group override for a release
// whose project (without the group override) is base. It returns the
// invocation with the override added and the project as Compose merges it.
func (e *Engine) applyGroup(ctx context.Context, app store.App, inv compose.Invocation, base compose.Project, dir string, stderr io.Writer) (compose.Invocation, compose.Project, groupResult, error) {
	path := e.groupOverridePath(app.Slug)
	if app.GroupID == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return inv, base, groupResult{}, fmt.Errorf("remove group override: %w", err)
		}
		return inv, base, groupResult{}, nil
	}
	group, err := e.opts.Store.GetGroup(ctx, *app.GroupID)
	if err != nil {
		return inv, base, groupResult{}, fmt.Errorf("load the app's group: %w", err)
	}
	result := groupResult{Network: group.Network(), Services: app.GroupServices}
	conflicts, err := e.groupConflicts(ctx, app, group, compose.MemberOf(app.Slug, base, app.GroupServices))
	if err != nil {
		return inv, base, groupResult{}, err
	}
	result.Conflicts = conflicts

	override, err := compose.GroupOverride(compose.GroupOverrideParams{
		Project: base, Slug: app.Slug, Network: group.Network(), Services: app.GroupServices,
	})
	if err != nil {
		return inv, base, groupResult{}, err
	}
	if override == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return inv, base, groupResult{}, fmt.Errorf("remove group override: %w", err)
		}
		return inv, base, result, nil
	}
	if err := writeFileAtomic(path, override, 0o644); err != nil {
		return inv, base, groupResult{}, fmt.Errorf("write group override: %w", err)
	}
	if err := e.ensureGroupNetwork(ctx, group); err != nil {
		return inv, base, groupResult{}, err
	}
	inv.Files = append(slices.Clone(inv.Files), path)
	merged, err := e.composeConfigTo(ctx, inv, dir, stderr)
	if err != nil {
		return inv, base, groupResult{}, err
	}
	return inv, merged, result, nil
}

// groupConflicts runs the name check against the other members, as their
// containers and the database describe them. A member that has not started
// yet is represented by its chosen services; its own deploy checks the rest.
func (e *Engine) groupConflicts(ctx context.Context, app store.App, group store.Group, self compose.GroupMember) ([]compose.Violation, error) {
	members, err := e.opts.Store.ListGroupMembers(ctx, group.ID)
	if err != nil {
		return nil, err
	}
	var others []compose.GroupMember
	for _, member := range members {
		if member.ID == app.ID {
			continue
		}
		m, err := e.memberFromContainers(ctx, member)
		if err != nil {
			return nil, err
		}
		others = append(others, m)
	}
	return compose.CheckGroupNames(self, others), nil
}

// projectContainer is one container of an app as `docker ps` lists it.
type projectContainer struct {
	ID, Name, Service string
	Networks          []string
}

func (e *Engine) projectContainers(ctx context.Context, project string) ([]projectContainer, error) {
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: []string{
		"ps", "--all", "--no-trunc",
		"--filter", "label=com.docker.compose.project=" + project,
		"--format", `{{.ID}}	{{.Names}}	{{.Label "com.docker.compose.service"}}	{{.Networks}}`,
	}})
	if err != nil {
		return nil, fmt.Errorf("list the containers of %s: %w", project, err)
	}
	var containers []projectContainer
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 3 || fields[0] == "" {
			continue
		}
		c := projectContainer{ID: fields[0], Name: fields[1], Service: fields[2]}
		if len(fields) > 3 && fields[3] != "" {
			c.Networks = strings.Split(fields[3], ",")
		}
		containers = append(containers, c)
	}
	return containers, nil
}

func (e *Engine) memberFromContainers(ctx context.Context, app store.App) (compose.GroupMember, error) {
	containers, err := e.projectContainers(ctx, app.ProjectName())
	if err != nil {
		return compose.GroupMember{}, err
	}
	m := compose.GroupMember{Slug: app.Slug}
	add := func(list *[]string, name string) {
		if name != "" && !slices.Contains(*list, name) {
			*list = append(*list, name)
		}
	}
	for _, service := range app.GroupServices {
		add(&m.Names, service)
		add(&m.Joined, service)
	}
	for _, c := range containers {
		add(&m.Names, c.Service)
		add(&m.Names, c.Name)
		if slices.Contains(app.GroupServices, c.Service) {
			add(&m.Joined, c.Name)
		}
	}
	return m, nil
}

// ensureGroupNetwork creates the group's network when it does not exist.
// It is internal: it never becomes a way out of the host.
func (e *Engine) ensureGroupNetwork(ctx context.Context, group store.Group) error {
	if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{
		"network", "inspect", "--format", "{{.Name}}", group.Network(),
	}, Stdout: io.Discard, Stderr: io.Discard}); err == nil {
		return nil
	}
	if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{
		"network", "create", "--internal",
		"--label", "io.koroku.group=" + strconv.FormatInt(group.ID, 10),
		group.Network(),
	}, Stdout: io.Discard}); err != nil {
		return fmt.Errorf("create the group network %s: %w", group.Network(), err)
	}
	return nil
}

// disconnectFromGroup takes the app's containers off a group network,
// except those of the services in keep.
func (e *Engine) disconnectFromGroup(ctx context.Context, app store.App, network string, keep []string) error {
	containers, err := e.projectContainers(ctx, app.ProjectName())
	if err != nil {
		return err
	}
	for _, c := range containers {
		if !slices.Contains(c.Networks, network) || slices.Contains(keep, c.Service) {
			continue
		}
		if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{
			"network", "disconnect", "--force", network, c.ID,
		}}); err != nil {
			return fmt.Errorf("disconnect %s from %s: %w", c.Name, network, err)
		}
	}
	return nil
}

// CreateGroup records a group and creates its network.
func (e *Engine) CreateGroup(ctx context.Context, name, environment string) (store.Group, error) {
	group, err := e.opts.Store.CreateGroup(ctx, name, environment, e.opts.Clock.Now())
	if err != nil {
		return store.Group{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	if err := e.ensureGroupNetwork(ctx, group); err != nil {
		// The network is created again before the first `up` that needs it.
		e.opts.Logger.Warn("creating the group network failed", "group", group.ID, "error", err)
	}
	return group, nil
}

// DeleteGroup removes a group without members and its network.
func (e *Engine) DeleteGroup(ctx context.Context, id int64) error {
	group, err := e.opts.Store.GetGroup(ctx, id)
	if err != nil {
		return err
	}
	if err := e.opts.Store.DeleteGroup(ctx, id); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	e.removeGroupNetwork(ctx, group.Network())
	return nil
}

func (e *Engine) removeGroupNetwork(ctx context.Context, network string) {
	var output strings.Builder
	err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{"network", "rm", network}, Stdout: &output, Stderr: &output})
	if err != nil && !strings.Contains(output.String(), "not found") {
		e.opts.Logger.Warn("removing the group network failed", "network", network, "error", err)
	}
}

// SetGroup changes the app's group and the services that join it. Joining
// takes effect on the next `up`; leaving disconnects the containers now.
func (e *Engine) SetGroup(ctx context.Context, app store.App, groupID *int64, services []string) error {
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	var group store.Group
	if groupID != nil {
		if group, err = e.opts.Store.GetGroup(ctx, *groupID); err != nil {
			return err
		}
		if group.Environment != app.Environment {
			return store.ErrGroupEnvironment
		}
	}
	// With a running release, check the chosen services now rather than
	// at the next deploy.
	if groupID != nil && len(services) > 0 {
		if base, ok, err := e.currentProject(ctx, app); err != nil {
			return err
		} else if ok {
			var missing []string
			for _, service := range services {
				if _, defined := base.Services[service]; !defined {
					missing = append(missing, service)
				}
			}
			if len(missing) > 0 {
				return &UnknownServicesError{Services: missing}
			}
			conflicts, err := e.groupConflicts(ctx, app, group, compose.MemberOf(app.Slug, base, services))
			if err != nil {
				return err
			}
			if len(conflicts) > 0 {
				return &compose.PolicyError{Violations: conflicts}
			}
		}
	}

	if err := e.opts.Store.SetAppGroup(ctx, app.ID, groupID, services, e.opts.Clock.Now()); err != nil {
		return err
	}
	if app.GroupID != nil {
		var keep []string
		if groupID != nil && *groupID == *app.GroupID {
			keep = services
		}
		old := store.Group{ID: *app.GroupID}
		if err := e.disconnectFromGroup(ctx, app, old.Network(), keep); err != nil {
			return fmt.Errorf("the group was changed, but taking the containers off the old network failed: %w", err)
		}
	}
	return nil
}

// currentProject returns the normalized running release without the group
// override, or ok=false when the app has no release on disk.
func (e *Engine) currentProject(ctx context.Context, app store.App) (compose.Project, bool, error) {
	inv, dir, ok := e.currentInvocation(app)
	if !ok {
		return compose.Project{}, false, nil
	}
	project, err := e.composeConfigTo(ctx, inv, dir, io.Discard)
	if err != nil {
		return compose.Project{}, false, err
	}
	return project, true, nil
}

// ReconcileGroups makes the group networks match the database: missing
// networks are created, networks of deleted groups are removed when no
// container uses them, and containers that are not allowed on a group
// network are taken off it. It runs when koroku starts.
func (e *Engine) ReconcileGroups(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	groups, err := e.opts.Store.ListGroups(ctx)
	if err != nil {
		return err
	}
	known := map[string]store.Group{}
	for _, g := range groups {
		known[g.Network()] = g
		if err := e.ensureGroupNetwork(ctx, g); err != nil {
			e.opts.Logger.Warn("creating a group network failed", "network", g.Network(), "error", err)
		}
	}
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: []string{
		"network", "ls", "--filter", "name=" + compose.GroupNetworkPrefix, "--format", "{{.Name}}",
	}})
	if err != nil {
		return fmt.Errorf("list group networks: %w", err)
	}
	for _, network := range strings.Fields(string(out)) {
		if !strings.HasPrefix(network, compose.GroupNetworkPrefix) {
			continue // the filter matches substrings
		}
		if _, ok := known[network]; !ok {
			e.removeGroupNetwork(ctx, network)
		}
	}

	apps, err := e.opts.Store.ListApps(ctx)
	if err != nil {
		return err
	}
	for _, app := range apps {
		containers, err := e.projectContainers(ctx, app.ProjectName())
		if err != nil {
			e.opts.Logger.Warn("listing containers for the group check failed", "app", app.Slug, "error", err)
			continue
		}
		for _, c := range containers {
			for _, network := range c.Networks {
				if !strings.HasPrefix(network, compose.GroupNetworkPrefix) {
					continue
				}
				g, ok := known[network]
				allowed := ok && app.GroupID != nil && *app.GroupID == g.ID && slices.Contains(app.GroupServices, c.Service)
				if allowed {
					continue
				}
				if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{
					"network", "disconnect", "--force", network, c.ID,
				}}); err != nil {
					e.opts.Logger.Warn("taking a container off a group network failed", "container", c.Name, "network", network, "error", err)
				} else {
					e.opts.Logger.Info("took a container off a group network it may not join", "container", c.Name, "network", network)
				}
			}
		}
	}
	return nil
}
