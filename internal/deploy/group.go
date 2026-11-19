package deploy

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// Groups (ARCHITECTURE 6.1). koroku attaches the chosen services to the
// group network itself with `docker network connect --alias`, between
// creating and starting the containers, from the database on every `up`.
// Through Compose the bare service name would be on the shared network too
// and could resolve to another app's service of the same name.

// GroupServiceError is returned when services chosen for a group cannot
// join it (not in the release, container_name, network_mode ...).
type GroupServiceError struct {
	Violations []compose.Violation
}

func (e *GroupServiceError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = v.Service + ": " + v.Message
	}
	return "services cannot join the group: " + strings.Join(parts, "; ")
}

// groupViolations checks the app's chosen services against a release.
func groupViolations(app store.App, project compose.Project) []compose.Violation {
	if app.GroupID == nil {
		return nil
	}
	return compose.CheckGroupServices(project, app.GroupServices)
}

// upApp starts the app's containers with `up --wait`. When services of the
// app join a group, the containers are created first, attached to the
// group network and only then started, so that a service that talks to
// another member while starting passes its health check.
func (e *Engine) upApp(ctx context.Context, app store.App, run func(sub ...string) error) error {
	wait := strconv.Itoa(int(e.opts.WaitTimeout / time.Second))
	if app.GroupID != nil && len(app.GroupServices) > 0 {
		group, err := e.opts.Store.GetGroup(ctx, *app.GroupID)
		if err != nil {
			return fmt.Errorf("load the app's group: %w", err)
		}
		if err := e.ensureGroupNetwork(ctx, group); err != nil {
			return err
		}
		if err := run("up", "--no-start", "--remove-orphans"); err != nil {
			return err
		}
		if err := e.attachToGroup(ctx, app, group.Network(), app.GroupServices); err != nil {
			return err
		}
	}
	return run("up", "-d", "--remove-orphans", "--wait", "--wait-timeout", wait)
}

// attachToGroup connects the containers of the chosen services to the
// group network under their group hostname, and disconnects the others.
func (e *Engine) attachToGroup(ctx context.Context, app store.App, network string, services []string) error {
	containers, err := e.projectContainers(ctx, app.ProjectName())
	if err != nil {
		return err
	}
	for _, c := range containers {
		on := slices.Contains(c.Networks, network)
		want := slices.Contains(services, c.Service)
		switch {
		case want && !on:
			if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{
				"network", "connect", "--alias", compose.GroupHostname(c.Service, app.Slug), network, c.ID,
			}}); err != nil {
				return fmt.Errorf("connect %s to %s: %w", c.Name, network, err)
			}
		case !want && on:
			if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{
				"network", "disconnect", "--force", network, c.ID,
			}}); err != nil {
				return fmt.Errorf("disconnect %s from %s: %w", c.Name, network, err)
			}
		}
	}
	return nil
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
		base, ok, err := e.currentProject(ctx, app)
		if err != nil {
			return err
		}
		if ok {
			if found := compose.CheckGroupServices(base, services); len(found) > 0 {
				return &GroupServiceError{Violations: found}
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

// currentProject returns the normalized running release, or ok=false when
// the app has no release on disk.
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
