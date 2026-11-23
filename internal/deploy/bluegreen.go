package deploy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// Blue/green deploys of the public service (ARCHITECTURE 7.2). The new
// release's copy of the public service starts next to the one that serves,
// on the other port; once it answers, the database and the proxy switch to
// it, the other services are recreated, and the old copy stops.

// Step keys of a blue/green deploy, besides the ones of chapter 7.
const (
	StepSwitch   = "switch"
	StepServices = "services"
	StepDrain    = "drain"
)

// releaseIsBlueGreen reports whether the release of deployment id has the
// blue/green form (coloured copies of the public service).
func (e *Engine) releaseIsBlueGreen(slug string, id int64) bool {
	meta, err := e.readMeta(slug, id)
	return err == nil && meta.BlueGreen
}

// policyParams are the policy parameters of a release of the app.
func (e *Engine) policyParams(app store.App, releaseDir string, blueGreen bool) compose.PolicyParams {
	params := compose.PolicyParams{
		ReleaseDir:  releaseDir,
		ProjectName: app.ProjectName(),
		EnvFile:     e.EnvFile(app.Slug),
		WebService:  app.WebService,
		WebPort:     app.WebPort,
		HostPort:    app.HostPort,
		Relax:       app.RelaxPolicy,
		// Injected into the services that read the commit variables.
		ReleaseEnvFile: e.ReleaseEnvFile(app.Slug),
	}
	if blueGreen {
		params.BlueGreen = true
		params.AltHostPort = app.AltHostPort
	}
	return params
}

// BlueGreenProblems returns what keeps the app's running release from
// blue/green deploys (ARCHITECTURE 7.2), as found when it was deployed.
// known is false when the release did not record it.
func (e *Engine) BlueGreenProblems(app store.App) (problems []compose.Violation, known bool) {
	if app.CurrentDeploymentID == nil {
		return nil, false
	}
	meta, err := e.readMeta(app.Slug, *app.CurrentDeploymentID)
	if err != nil || meta.BlueGreenProblems == nil {
		return nil, false
	}
	return *meta.BlueGreenProblems, true
}

// composeRun runs one docker compose call; deployments write the output
// into their log, manual operations keep it for the error.
type composeRun func(ctx context.Context, inv compose.Invocation, dir string, sub ...string) error

func (e *Engine) deployRun(log *deployLog) composeRun {
	return func(ctx context.Context, inv compose.Invocation, dir string, sub ...string) error {
		if sub[0] == "up" {
			return e.composePulling(ctx, inv, dir, log, sub...)
		}
		return e.compose(ctx, inv, dir, log, sub...)
	}
}

// releaseColor is the colour a blue/green release starts in when its public
// service is not replaced side by side: the app's colour, blue when it has
// none. It is "" for a release that runs the public service as itself.
func (e *Engine) releaseColor(app store.App, id int64) string {
	if !e.releaseIsBlueGreen(app.Slug, id) {
		return ""
	}
	if app.WebColor != "" {
		return app.WebColor
	}
	return store.ColorBlue
}

// releaseServices lists the services of release id that run in color: all
// but the public service itself and the copy of the other colour. It is
// nil for a release without the blue/green form, where every service runs.
func (e *Engine) releaseServices(app store.App, id int64, color string) []string {
	if color == "" {
		return nil
	}
	meta, err := e.readMeta(app.Slug, id)
	if err != nil || len(meta.Services) == 0 {
		return nil
	}
	skip := []string{app.WebService, app.ColoredService(store.OtherColor(color))}
	var out []string
	for _, name := range meta.Services {
		if !slices.Contains(skip, name) {
			out = append(out, name)
		}
	}
	return out
}

// liveServices are the services of the app's current release that run, for
// the calls that would otherwise start both colours (start, restart); nil
// when every service of the project runs.
func (e *Engine) liveServices(app store.App) []string {
	if app.CurrentDeploymentID == nil {
		return nil
	}
	return e.releaseServices(app, *app.CurrentDeploymentID, e.releaseColor(app, *app.CurrentDeploymentID))
}

// containerServices lists the services of the app that have a container,
// running or not.
func (e *Engine) containerServices(ctx context.Context, app store.App) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runner.Output(ctx, e.opts.Runner, runner.Cmd{
		Name: "docker", Args: e.projectOnly(app).Args("ps", "--all", "--format", "json"),
	})
	if err != nil {
		return nil, fmt.Errorf("docker compose ps: %w", err)
	}
	statuses, err := parseComposePS(out)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, s := range statuses {
		if !slices.Contains(names, s.Name) {
			names = append(names, s.Name)
		}
	}
	return names, nil
}

// removeServices stops and removes the containers of the named services
// that exist. `--remove-orphans` does not remove them: they are services
// of the project, or disabled by a profile (ARCHITECTURE 7.2).
func (e *Engine) removeServices(ctx context.Context, app store.App, names []string, run composeRun) error {
	existing, err := e.containerServices(ctx, app)
	if err != nil {
		return err
	}
	var remove []string
	for _, name := range names {
		if slices.Contains(existing, name) {
			remove = append(remove, name)
		}
	}
	if len(remove) == 0 {
		return nil
	}
	return run(ctx, e.projectOnly(app), "", append([]string{"rm", "--stop", "--force"}, remove...)...)
}

// staleColors are the containers that do not receive requests: the
// coloured copies other than the app's colour (both when it has none).
func staleColors(app store.App) []string {
	var out []string
	for _, color := range compose.Colors {
		if color != app.WebColor {
			out = append(out, app.ColoredService(color))
		}
	}
	return out
}

// upRelease brings release id up. A blue/green release runs in the app's
// colour (blue when it has none) and the containers of the public service
// in any other form are removed first; a release that runs the public
// service as itself removes the coloured copies first. The app's colour is
// recorded once the containers are up.
func (e *Engine) upRelease(ctx context.Context, app store.App, id int64, dockerConfig string, run composeRun) error {
	meta, err := e.readMeta(app.Slug, id)
	if err != nil {
		return err
	}
	color := e.releaseColor(app, id)
	switch {
	case color != "":
		remove := []string{app.WebService, app.ColoredService(store.OtherColor(color))}
		if err := e.removeServices(ctx, app, remove, run); err != nil {
			return fmt.Errorf("remove the other form of the public service: %w", err)
		}
	case app.UsesColors():
		if err := e.removeServices(ctx, app, compose.ColoredNames(app.WebService), run); err != nil {
			return fmt.Errorf("remove the coloured copies of the public service: %w", err)
		}
	}
	inv := e.invocation(app, id, meta.ComposeFile, true)
	inv.DockerConfig = dockerConfig
	args := []string{"up", "-d", "--pull", "missing", "--remove-orphans", "--wait", "--wait-timeout", e.waitSeconds()}
	args = append(args, e.releaseServices(app, id, color)...)
	if err := run(ctx, inv, e.ReleaseDir(app.Slug, id), args...); err != nil {
		return err
	}
	if color != app.WebColor {
		if err := e.opts.Store.SetAppWebColor(ctx, app.ID, color); err != nil {
			return err
		}
	}
	return nil
}

// blueGreenRelease is the release a blue/green deploy (or a rollback to a
// blue/green release) starts.
type blueGreenRelease struct {
	id           int64
	composeFile  string
	project      compose.Project
	dockerConfig string
	// env and releaseEnv are the contents of the env files it runs with.
	env, releaseEnv    []byte
	releaseEnvServices []string
}

// blueGreenFailure is a failed blue/green deploy that cleaned up after
// itself: the automatic rollback of chapter 7 does not apply.
type blueGreenFailure struct {
	err error
	// restored is set when the other services were brought back to the
	// previous release (the failure came after the switch).
	restored bool
}

func (f *blueGreenFailure) Error() string { return f.err.Error() }
func (f *blueGreenFailure) Unwrap() error { return f.err }

// memoryProber reads how much memory new containers can use.
// system.Host implements it.
type memoryProber interface {
	AvailableMemory() (uint64, error)
}

// shuttingDown reports whether ctx ended because koroku stops, as opposed
// to a cancel by the user or the job's timeout. Nothing is undone then: the
// startup cleanup removes what does not receive requests.
func shuttingDown(ctx context.Context) bool {
	cause := context.Cause(ctx)
	return errors.Is(cause, context.Canceled) && !errors.Is(cause, errCanceledByUser)
}

// deployBlueGreen runs the start of a blue/green release (ARCHITECTURE 7.2).
// When the public service does not run (first deploy, stopped, asleep), the
// release starts at once in the app's colour, as in chapter 7; otherwise
// its copy in the other colour starts next to the serving one.
func (e *Engine) deployBlueGreen(ctx context.Context, d store.Deployment, app store.App, rel blueGreenRelease, log *deployLog) (bool, error) {
	if current, err := e.opts.Store.GetAppByID(ctx, app.ID); err == nil {
		app = current
	}
	oldService := app.LiveService(app.WebService)
	running, err := e.runningServices(ctx, app)
	if err != nil {
		return false, err
	}
	if app.CurrentDeploymentID == nil || app.SleepingSince != nil || !slices.Contains(running, oldService) {
		return e.startBlueGreenAlone(ctx, d, app, rel, log)
	}

	oldColor := app.WebColor
	newColor := store.OtherColor(oldColor)
	newService := app.ColoredService(newColor)
	newPort := app.ColorPort(newColor)
	oldName := oldColor
	if oldName == "" {
		oldName = app.WebService
	}

	log.Step(StepStart, "Starting %s on port %d", newColor, newPort)
	if err := e.checkMemoryForColor(app, rel.project.Services[newService], log); err != nil {
		return false, err
	}
	e.logStopGraces(ctx, app, rel.project.WithoutServices(compose.ColoredNames(app.WebService)...), log)
	// Copies left by an interrupted deploy hold the port.
	if err := e.removeServices(ctx, app, staleColors(app), e.deployRun(log)); err != nil {
		return false, err
	}
	if err := e.opts.Store.SetDeploymentWebColor(ctx, d.ID, newColor); err != nil {
		return false, err
	}
	existing, err := e.containerServices(ctx, app)
	if err != nil {
		return false, err
	}
	var startNames, otherNames []string
	for _, name := range rel.project.ServiceNames() {
		switch {
		case name == app.WebService || slices.Contains(compose.ColoredNames(app.WebService), name):
		case rel.project.OneShot(name):
			startNames = append(startNames, name)
		case !slices.Contains(existing, name):
			// New in this release: nothing to keep running.
			startNames = append(startNames, name)
		default:
			otherNames = append(otherNames, name)
		}
	}
	startNames = append(startNames, newService)

	// From here on the env files are the new release's.
	if err := e.writeLiveEnv(app.Slug, rel.env, rel.releaseEnv); err != nil {
		return false, fmt.Errorf("write env file: %w", err)
	}
	run := e.deployRun(log)
	inv := e.invocation(app, rel.id, rel.composeFile, true)
	inv.DockerConfig = rel.dockerConfig
	dir := e.ReleaseDir(app.Slug, rel.id)
	undoStart := func(cause error) (bool, error) {
		if !shuttingDown(ctx) {
			undoCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
			defer cancel()
			e.undoNewColor(undoCtx, app, newService, log)
		}
		return true, &blueGreenFailure{err: cause}
	}
	upArgs := append([]string{"up", "-d", "--pull", "missing", "--no-deps", "--wait", "--wait-timeout", e.waitSeconds()}, startNames...)
	if err := run(ctx, inv, dir, upArgs...); err != nil {
		line := e.logContainerOutput(ctx, app, log, startNames...)
		return undoStart(withOutput(fmt.Errorf("start %s: %w", newService, err), line))
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", newPort, app.HealthcheckPath)
	log.Step(StepCheck, "Checking %s", url)
	if err := e.opts.Health.Wait(ctx, url, e.opts.HealthTimeout); err != nil {
		line := e.logContainerOutput(ctx, app, log, newService)
		return undoStart(withOutput(err, line))
	}
	log.Printf("The app is answering.")

	log.Step(StepSwitch, "Switching traffic from %s to %s", oldName, newColor)
	if err := e.switchColor(ctx, app, d.ID, newColor); err != nil {
		e.switchBack(app, d.ID, oldColor, false, log)
		return undoStart(err)
	}
	log.Printf("Requests now reach %s on port %d.", newColor, newPort)

	log.Step(StepServices, "Recreating the other services")
	if len(otherNames) == 0 {
		log.Printf("No other service.")
	} else {
		e.noteEnvChange(app, rel.env, otherNames, rel.releaseEnvServices, log)
		args := append([]string{"up", "-d", "--pull", "missing", "--no-deps", "--wait", "--wait-timeout", e.waitSeconds()}, otherNames...)
		if err := run(ctx, inv, dir, args...); err != nil {
			line := e.logContainerOutput(ctx, app, log, otherNames...)
			err = withOutput(fmt.Errorf("recreate the other services: %w", err), line)
			if shuttingDown(ctx) {
				return true, &blueGreenFailure{err: err}
			}
			restored := e.restoreOldRelease(app, d.ID, oldColor, newService, log)
			return true, &blueGreenFailure{err: err, restored: restored}
		}
	}

	log.Step(StepDrain, "Stopping %s", oldName)
	e.drain(ctx, app, oldService, newService, inv, dir, log)
	return true, nil
}

// startBlueGreenAlone starts a blue/green release when no copy of the
// public service runs: all services at once, in the app's colour (blue
// when it has none). There is nothing to switch from.
func (e *Engine) startBlueGreenAlone(ctx context.Context, d store.Deployment, app store.App, rel blueGreenRelease, log *deployLog) (bool, error) {
	color := app.WebColor
	if color == "" {
		color = store.ColorBlue
	}
	log.Step(StepStart, "Starting %s on port %d", color, app.ColorPort(color))
	log.Printf("The public service is not running; all services start at once.")
	e.logStopGraces(ctx, app, rel.project.WithoutServices(compose.ColoredNames(app.WebService)...), log)
	if err := e.endSleep(ctx, app, log); err != nil {
		return false, err
	}
	if err := e.opts.Store.SetDeploymentWebColor(ctx, d.ID, color); err != nil {
		return false, err
	}
	if err := e.writeLiveEnv(app.Slug, rel.env, rel.releaseEnv); err != nil {
		return false, fmt.Errorf("write env file: %w", err)
	}
	if err := e.upRelease(ctx, app, rel.id, rel.dockerConfig, e.deployRun(log)); err != nil {
		line := e.logContainerOutput(ctx, app, log)
		return true, withOutput(fmt.Errorf("start containers: %w", err), line)
	}
	return e.routeAndCheck(ctx, app, log)
}

// checkMemoryForColor fails the deployment when the host cannot hold a
// second copy of the public service: MemAvailable and SwapFree together
// below the copy's memory limit (ARCHITECTURE 7.2). Without a reading or a
// limit the deployment goes on.
func (e *Engine) checkMemoryForColor(app store.App, svc compose.Service, log *deployLog) error {
	prober, ok := e.opts.System.(memoryProber)
	if !ok {
		return nil
	}
	limit, ok := compose.ParseMemory(svc.MemLimit)
	if !ok {
		if limit, ok = compose.ParseMemory(svc.DeployMemoryLimit()); !ok {
			return nil
		}
	}
	free, err := prober.AvailableMemory()
	if err != nil {
		log.Printf("Reading the free memory failed (%v); starting anyway.", err)
		return nil
	}
	if free >= uint64(limit) {
		return nil
	}
	msg := fmt.Sprintf("Not enough memory to run two %s containers (%s available, %s needed).",
		app.WebService, formatMiB(int64(free)), formatMiB(limit))
	log.Printf("%s", msg)
	log.Printf("Free memory (stop or put other apps to sleep, lower the limit) or turn blue/green deploys off, then deploy again.")
	return errors.New(msg)
}

// formatMiB renders bytes in whole MiB, the unit of memory limits.
func formatMiB(n int64) string { return fmt.Sprintf("%d MiB", n>>20) }

// switchColor records the new colour and loads the proxy configuration
// built from it.
func (e *Engine) switchColor(ctx context.Context, app store.App, id int64, color string) error {
	if err := e.opts.Store.SetAppWebColor(ctx, app.ID, color); err != nil {
		return err
	}
	now := e.opts.Clock.Now()
	if err := e.opts.Store.SetDeploymentSwitchedAt(ctx, id, &now); err != nil {
		return err
	}
	if e.opts.Router != nil {
		if err := e.opts.Router.Sync(ctx); err != nil {
			return fmt.Errorf("update routes: %w", err)
		}
	}
	return nil
}

// switchBack records the old colour again; with route, it also loads the
// proxy configuration (the old colour still runs).
func (e *Engine) switchBack(app store.App, id int64, oldColor string, route bool, log *deployLog) {
	ctx, cancel := context.WithTimeout(context.Background(), e.finishTimeout)
	defer cancel()
	if err := e.opts.Store.SetAppWebColor(ctx, app.ID, oldColor); err != nil {
		log.Printf("Recording the old colour again failed: %v", err)
	}
	if err := e.opts.Store.SetDeploymentSwitchedAt(ctx, id, nil); err != nil {
		log.Printf("Clearing the switch time failed: %v", err)
	}
	if route && e.opts.Router != nil {
		if err := e.opts.Router.Sync(ctx); err != nil {
			log.Printf("Switching the routes back failed; the periodic update retries: %v", err)
		} else {
			log.Printf("Requests reach the previous release again.")
		}
	}
}

// undoNewColor removes the new colour's copy and puts the running
// release's env files back. The old colour, the database and the workers
// were not touched.
func (e *Engine) undoNewColor(ctx context.Context, app store.App, newService string, log *deployLog) {
	log.Printf("Removing %s; the previous release keeps serving.", newService)
	if err := e.removeServices(ctx, app, []string{newService}, e.deployRun(log)); err != nil {
		log.Printf("Removing %s failed: %v", newService, err)
	}
	if app.CurrentDeploymentID == nil {
		return
	}
	env, release, err := e.readEnvSnapshots(app.Slug, *app.CurrentDeploymentID)
	if err == nil {
		err = e.writeLiveEnv(app.Slug, env, release)
	}
	if err != nil {
		log.Printf("Restoring the env file of the running release failed: %v", err)
	}
}

// restoreOldRelease undoes a blue/green deploy after the switch: the old
// colour receives the requests again, the other services go back to the
// running release and the new colour is removed. It reports whether the
// other services run the previous release again.
func (e *Engine) restoreOldRelease(app store.App, id int64, oldColor, newService string, log *deployLog) bool {
	log.Step(StepRollback, "Switching back and restoring the other services of deployment #%d", *app.CurrentDeploymentID)
	e.switchBack(app, id, oldColor, true, log)
	ctx, cancel := context.WithTimeout(context.Background(), e.upTimeout())
	defer cancel()
	prev := *app.CurrentDeploymentID
	restored := func() bool {
		meta, err := e.readMeta(app.Slug, prev)
		if err != nil {
			log.Printf("Restoring is not possible: the previous release is no longer on disk.")
			return false
		}
		dockerConfig, err := e.registryConfig(ctx, app, log.masker)
		if err != nil {
			log.Printf("Restoring failed: %v", err)
			return false
		}
		project, err := e.revalidatedProject(ctx, app, prev, meta.ComposeFile, dockerConfig)
		if err != nil {
			log.Printf("Restoring is not possible: the previous release no longer passes validation (%v).", err)
			return false
		}
		env, release, err := e.readEnvSnapshots(app.Slug, prev)
		if err == nil {
			err = e.writeLiveEnv(app.Slug, env, release)
		}
		if err != nil {
			log.Printf("Restoring the env file failed: %v", err)
			return false
		}
		var names []string
		for _, name := range project.ServiceNames() {
			if name == app.WebService || slices.Contains(compose.ColoredNames(app.WebService), name) || project.OneShot(name) {
				continue
			}
			names = append(names, name)
		}
		if len(names) == 0 {
			return true
		}
		inv := e.invocation(app, prev, meta.ComposeFile, true)
		inv.DockerConfig = dockerConfig
		args := append([]string{"up", "-d", "--pull", "missing", "--no-deps", "--wait", "--wait-timeout", e.waitSeconds()}, names...)
		if err := e.deployRun(log)(ctx, inv, e.ReleaseDir(app.Slug, prev), args...); err != nil {
			log.Printf("Restoring the other services failed: %v", err)
			return false
		}
		log.Printf("Restored %s of deployment #%d.", strings.Join(names, ", "), prev)
		return true
	}()
	log.Printf("Removing %s.", newService)
	if err := e.removeServices(ctx, app, []string{newService}, e.deployRun(log)); err != nil {
		log.Printf("Removing %s failed: %v", newService, err)
	}
	return restored
}

// drain waits the app's drain time after the switch, then stops and
// removes the old copy of the public service and the containers of
// services the release no longer has. A failure here leaves the
// deployment succeeded: the requests already reach the new colour, and the
// next deploy or koroku's start removes what is left.
func (e *Engine) drain(ctx context.Context, app store.App, oldService, newService string, inv compose.Invocation, dir string, log *deployLog) {
	wait := time.Duration(app.BlueGreenDrainSeconds) * time.Second
	if wait > 0 {
		log.Printf("Waiting %s for the requests in flight.", wait)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			if shuttingDown(ctx) {
				log.Printf("koroku is shutting down; %s is removed when it starts again.", oldService)
				return
			}
			log.Printf("The wait was cut short.")
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	run := e.deployRun(log)
	project := e.projectOnly(app)
	if err := run(ctx, project, "", "stop", oldService); err != nil {
		log.Printf("Stopping %s failed (it no longer receives requests): %v", oldService, err)
		return
	}
	if err := run(ctx, project, "", "rm", "--force", oldService); err != nil {
		log.Printf("Removing %s failed (it no longer receives requests): %v", oldService, err)
		return
	}
	// The equivalent of --remove-orphans: the containers of services the
	// Compose file no longer has. The new colour is unchanged and stays.
	args := []string{"up", "-d", "--pull", "missing", "--no-deps", "--remove-orphans", "--wait", "--wait-timeout", e.waitSeconds(), newService}
	if err := run(ctx, inv, dir, args...); err != nil {
		log.Printf("Removing the containers of removed services failed: %v", err)
	}
}

// CleanUpColors removes, for every app that may run its public service in
// colours, the coloured copies that do not receive requests: a deploy
// interrupted before its switch left the new colour, one interrupted after
// it the old one (ARCHITECTURE 7.2). It runs at koroku's start, under each
// app's operation lock.
func (e *Engine) CleanUpColors(ctx context.Context) {
	apps, err := e.opts.Store.ListApps(ctx)
	if err != nil {
		e.opts.Logger.Warn("listing apps for the cleanup of colours failed", "error", err)
		return
	}
	for _, app := range apps {
		if !app.UsesColors() && app.AltHostPort == 0 {
			continue
		}
		func() {
			lock := e.locks.get(app.ID)
			lock.Lock()
			defer lock.Unlock()
			cleanCtx, cancel := context.WithTimeout(ctx, opTimeout)
			defer cancel()
			var output strings.Builder
			run := func(ctx context.Context, inv compose.Invocation, dir string, sub ...string) error {
				return e.composeTo(ctx, inv, dir, &output, sub...)
			}
			if err := e.removeServices(cleanCtx, app, staleColors(app), run); err != nil {
				e.opts.Logger.Warn("removing colours that receive no requests failed", "app", app.Slug, "error", err,
					"output", lastOutputLine(output.String()))
			} else if output.Len() > 0 {
				e.opts.Logger.Info("removed colours that receive no requests", "app", app.Slug)
			}
		}()
	}
}
