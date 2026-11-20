package deploy

import (
	"context"
	"errors"
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

// Links between apps (ARCHITECTURE 6.1). Each link has its own network.
// koroku attaches the target service under the link's name and the
// consumer's services without a name, with `docker network connect`,
// between creating and starting the containers, from the database on every
// `up`. Through Compose the bare service names would be on the network too
// and could resolve to the other app's service of the same name.

// LinkServiceError is returned when services or link names chosen in the
// console cannot be used with the app's running release.
type LinkServiceError struct {
	Violations []compose.Violation
}

func (e *LinkServiceError) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = v.Service + ": " + v.Message
	}
	return "cannot link: " + strings.Join(parts, "; ")
}

// installationID is the ID of this installation (see store.InstallationID),
// read once.
func (e *Engine) installationID(ctx context.Context) (string, error) {
	e.mu.Lock()
	id := e.installation
	e.mu.Unlock()
	if id != "" {
		return id, nil
	}
	id, err := e.opts.Store.InstallationID(ctx)
	if err != nil {
		return "", err
	}
	e.mu.Lock()
	e.installation = id
	e.mu.Unlock()
	return id, nil
}

// linkViolations checks what the app's group membership and links ask of
// a release: the exposed services and, for its own links, the services that
// use them and the link names.
func (e *Engine) linkViolations(ctx context.Context, app store.App, project compose.Project) ([]compose.Violation, error) {
	var found []compose.Violation
	if app.GroupID != nil {
		found = append(found, compose.CheckLinkServices(project, app.GroupServices)...)
	}
	links, err := e.opts.Store.ListAppLinks(ctx, app.ID)
	if err != nil {
		return nil, err
	}
	var uses []compose.LinkUse
	for _, l := range links {
		uses = append(uses, compose.LinkUse{Name: l.Name, Services: l.Services})
	}
	return append(found, compose.CheckLinks(project, uses)...), nil
}

// attachment is where the containers of one app belong: network -> service
// -> alias ("" for the consumer side, which gets no name).
type attachment map[string]map[string]string

func (e *Engine) desiredAttachment(ctx context.Context, app store.App) (attachment, []store.Link, error) {
	links, err := e.opts.Store.ListLinksOf(ctx, app.ID)
	if err != nil {
		return nil, nil, err
	}
	if len(links) == 0 {
		return nil, nil, nil
	}
	installation, err := e.installationID(ctx)
	if err != nil {
		return nil, nil, err
	}
	want := attachment{}
	for _, l := range links {
		network := l.Network(installation)
		if want[network] == nil {
			want[network] = map[string]string{}
		}
		if l.AppID == app.ID {
			for _, service := range l.Services {
				want[network][service] = ""
			}
		} else {
			want[network][l.TargetService] = l.Name
		}
	}
	return want, links, nil
}

// upApp starts the app's containers with `up --wait`. When the app takes
// part in links, the containers are created first, attached to the link
// networks and only then started, so that a service that talks to another
// app while starting passes its health check.
func (e *Engine) upApp(ctx context.Context, app store.App, run func(sub ...string) error) error {
	wait := strconv.Itoa(int(e.opts.WaitTimeout / time.Second))
	want, links, err := e.desiredAttachment(ctx, app)
	if err != nil {
		return err
	}
	if len(want) > 0 {
		installation, err := e.installationID(ctx)
		if err != nil {
			return err
		}
		for _, l := range links {
			if err := e.ensureLinkNetwork(ctx, installation, l); err != nil {
				return err
			}
		}
		if err := run("up", "--no-start", "--remove-orphans"); err != nil {
			return err
		}
		if err := e.attachLinks(ctx, app, want); err != nil {
			return err
		}
	}
	return run("up", "-d", "--remove-orphans", "--wait", "--wait-timeout", wait)
}

// attachLinks connects the app's containers to the link networks in want
// and takes them off every other link network of this installation.
func (e *Engine) attachLinks(ctx context.Context, app store.App, want attachment) error {
	installation, err := e.installationID(ctx)
	if err != nil {
		return err
	}
	ours := compose.LinkNetworkPrefix + installation + "_"
	containers, err := e.projectContainers(ctx, app.ProjectName())
	if err != nil {
		return err
	}
	for _, c := range containers {
		for _, network := range c.Networks {
			if !strings.HasPrefix(network, ours) {
				continue
			}
			if _, ok := want[network][c.Service]; ok {
				continue
			}
			if err := e.disconnect(ctx, network, c.ID); err != nil {
				return fmt.Errorf("disconnect %s from %s: %w", c.Name, network, err)
			}
		}
		for network, services := range want {
			alias, ok := services[c.Service]
			if !ok || slices.Contains(c.Networks, network) {
				continue
			}
			args := []string{"network", "connect"}
			if alias != "" {
				args = append(args, "--alias", alias)
			}
			args = append(args, network, c.ID)
			if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: args}); err != nil {
				return fmt.Errorf("connect %s to %s: %w", c.Name, network, err)
			}
		}
	}
	return nil
}

func (e *Engine) disconnect(ctx context.Context, network, container string) error {
	return e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{
		"network", "disconnect", "--force", network, container,
	}})
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

// ensureLinkNetwork creates the link's network when it does not exist. It
// is internal: it never becomes a way out of the host.
func (e *Engine) ensureLinkNetwork(ctx context.Context, installation string, l store.Link) error {
	network := l.Network(installation)
	if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{
		"network", "inspect", "--format", "{{.Name}}", network,
	}, Stdout: io.Discard, Stderr: io.Discard}); err == nil {
		return nil
	}
	if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{
		"network", "create", "--internal",
		"--label", "io.koroku.installation=" + installation,
		"--label", "io.koroku.link=" + strconv.FormatInt(l.ID, 10),
		network,
	}, Stdout: io.Discard}); err != nil {
		return fmt.Errorf("create the link network %s: %w", network, err)
	}
	return nil
}

// removeLinkNetwork takes every container off a link network and removes
// it. Problems are logged: the database no longer has the link, and the
// next start of koroku tries again.
func (e *Engine) removeLinkNetwork(ctx context.Context, network string) {
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: []string{
		"network", "inspect", "--format", "{{range $id, $c := .Containers}}{{$id}} {{end}}", network,
	}})
	if err != nil {
		return // already gone
	}
	for _, id := range strings.Fields(string(out)) {
		if err := e.disconnect(ctx, network, id); err != nil {
			e.opts.Logger.Warn("taking a container off a link network failed", "network", network, "error", err)
		}
	}
	if err := e.opts.Runner.Run(ctx, runner.Cmd{Name: "docker", Args: []string{"network", "rm", network}, Stdout: io.Discard}); err != nil {
		e.opts.Logger.Warn("removing a link network failed", "network", network, "error", err)
	}
}

func (e *Engine) removeLinkNetworks(ctx context.Context, links []store.Link) error {
	if len(links) == 0 {
		return nil
	}
	installation, err := e.installationID(ctx)
	if err != nil {
		return err
	}
	for _, l := range links {
		e.removeLinkNetwork(ctx, l.Network(installation))
	}
	return nil
}

// CreateGroup records a group.
func (e *Engine) CreateGroup(ctx context.Context, name, environment string) (store.Group, error) {
	return e.opts.Store.CreateGroup(ctx, name, environment, e.opts.Clock.Now())
}

// DeleteGroup removes a group without members.
func (e *Engine) DeleteGroup(ctx context.Context, id int64) error {
	return e.opts.Store.DeleteGroup(ctx, id)
}

// SetGroup changes the app's group and the services it exposes. Links
// that no longer hold are deleted and their networks removed right away.
func (e *Engine) SetGroup(ctx context.Context, app store.App, groupID *int64, services []string) error {
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	if groupID != nil {
		group, err := e.opts.Store.GetGroup(ctx, *groupID)
		if err != nil {
			return err
		}
		if group.Environment != app.Environment {
			return store.ErrGroupEnvironment
		}
	}
	if groupID != nil && len(services) > 0 {
		base, ok, err := e.currentProject(ctx, app)
		if err != nil {
			return err
		}
		if ok {
			if found := compose.CheckLinkServices(base, services); len(found) > 0 {
				return &LinkServiceError{Violations: found}
			}
		}
	}
	gone, err := e.opts.Store.SetAppGroup(ctx, app.ID, groupID, services, e.opts.Clock.Now())
	if err != nil {
		return err
	}
	return e.removeLinkNetworks(ctx, gone)
}

// CreateLink records that name in the app's Compose file means
// targetService of the target app. The target's containers are attached
// right away; the app's own services join on its next deploy or start.
func (e *Engine) CreateLink(ctx context.Context, app store.App, name string, target store.App, targetService string, services []string) (store.Link, error) {
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return store.Link{}, err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()

	base, ok, err := e.currentProject(ctx, app)
	if err != nil {
		return store.Link{}, err
	}
	if ok {
		if found := compose.CheckLinks(base, []compose.LinkUse{{Name: name, Services: services}}); len(found) > 0 {
			return store.Link{}, &LinkServiceError{Violations: found}
		}
	}
	link, err := e.opts.Store.CreateLink(ctx, app.ID, name, target.ID, targetService, services, e.opts.Clock.Now())
	if err != nil {
		return store.Link{}, err
	}
	installation, err := e.installationID(ctx)
	if err != nil {
		return link, err
	}
	if err := e.ensureLinkNetwork(ctx, installation, link); err != nil {
		e.opts.Logger.Warn("creating a link network failed", "link", link.ID, "error", err)
		return link, nil
	}
	if want, _, err := e.desiredAttachment(ctx, target); err == nil {
		if err := e.attachLinks(ctx, target, want); err != nil {
			// The target joins on its next deploy or start.
			e.opts.Logger.Warn("attaching the link target failed", "link", link.ID, "error", err)
		}
	}
	return link, nil
}

// DeleteLink removes a link of the app and its network.
func (e *Engine) DeleteLink(ctx context.Context, app store.App, id int64) error {
	release, err := e.beginOp(ctx, app)
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	link, err := e.opts.Store.GetLink(ctx, app.ID, id)
	if err != nil {
		return err
	}
	if err := e.opts.Store.DeleteLink(ctx, app.ID, id); err != nil {
		return err
	}
	return e.removeLinkNetworks(ctx, []store.Link{link})
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

// ReconcileLinks makes this installation's link networks match the
// database when koroku starts: networks of existing links are created and
// the apps' containers attached as the links say, networks of deleted links
// are removed, and containers that may not be on a network are taken off.
// Networks of other installations on the same daemon are left alone.
func (e *Engine) ReconcileLinks(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	installation, err := e.installationID(ctx)
	if err != nil {
		return err
	}
	links, err := e.opts.Store.ListLinks(ctx)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, l := range links {
		known[l.Network(installation)] = true
		if err := e.ensureLinkNetwork(ctx, installation, l); err != nil {
			e.opts.Logger.Warn("creating a link network failed", "link", l.ID, "error", err)
		}
	}
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{Name: "docker", Args: []string{
		"network", "ls", "--filter", "label=io.koroku.installation=" + installation, "--format", "{{.Name}}",
	}})
	if err != nil {
		return fmt.Errorf("list link networks: %w", err)
	}
	for _, network := range strings.Fields(string(out)) {
		if !known[network] {
			e.removeLinkNetwork(ctx, network)
		}
	}

	apps, err := e.opts.Store.ListApps(ctx)
	if err != nil {
		return err
	}
	var failed []string
	for _, app := range apps {
		want, _, err := e.desiredAttachment(ctx, app)
		if err == nil {
			err = e.attachLinks(ctx, app, want)
		}
		if err != nil {
			failed = append(failed, app.Slug)
			e.opts.Logger.Warn("attaching an app to its link networks failed", "app", app.Slug, "error", err)
		}
	}
	if len(failed) > 0 {
		return errors.New("some apps could not be attached to their link networks: " + strings.Join(failed, ", "))
	}
	return nil
}
