package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/store"
)

// Rollbacks to a kept release (ARCHITECTURE 7.1): no fetch, build or pull;
// the release's own files, images and environment snapshot are started
// again.

var (
	// ErrReleaseUnavailable is returned when a deployment's release is not
	// kept any more (or never succeeded), so it cannot be started again.
	ErrReleaseUnavailable = errors.New("the release of this deployment is not available any more")
	// ErrOtherApp is returned when the deployment belongs to another app.
	ErrOtherApp = errors.New("the deployment belongs to another app")
)

// ReleaseAvailable reports whether the release of deployment id is kept:
// its metadata, its override and its environment snapshot are on disk.
func (e *Engine) ReleaseAvailable(app store.App, id int64) bool {
	if _, err := e.readMeta(app.Slug, id); err != nil {
		return false
	}
	for _, path := range []string{
		filepath.Join(e.ReleaseDir(app.Slug, id), compose.OverrideFileName),
		filepath.Join(e.stateDir(app.Slug, id), "env"),
	} {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

// RollbackTarget resolves the release a rollback to deployment d would
// start: d itself, or the release a rollback record points at.
func RollbackTarget(d store.Deployment) int64 {
	if d.SourceDeploymentID != nil {
		return *d.SourceDeploymentID
	}
	return d.ID
}

// EnqueueRollback queues a rollback of the app to the release of
// deployment fromID.
func (e *Engine) EnqueueRollback(ctx context.Context, app store.App, fromID int64) (store.Deployment, error) {
	from, err := e.opts.Store.GetDeployment(ctx, fromID)
	if err != nil {
		return store.Deployment{}, err
	}
	if from.AppID != app.ID {
		return store.Deployment{}, ErrOtherApp
	}
	target := from
	if from.SourceDeploymentID != nil {
		if target, err = e.opts.Store.GetDeployment(ctx, *from.SourceDeploymentID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return store.Deployment{}, ErrReleaseUnavailable
			}
			return store.Deployment{}, err
		}
	}
	if from.Status != store.StatusSucceeded || target.Status != store.StatusSucceeded || !e.ReleaseAvailable(app, target.ID) {
		return store.Deployment{}, ErrReleaseUnavailable
	}
	// Like a deployment, a rollback would wait for a running command and
	// hold up the single worker (ARCHITECTURE 9.6).
	e.admit.Lock()
	defer e.admit.Unlock()
	if e.taskRunning(app.ID) {
		return store.Deployment{}, ErrBusy
	}
	// Hold the worker off this rollback's log until the stale file is gone
	// and the queued line is written, as Enqueue does.
	e.queueing.Lock()
	defer e.queueing.Unlock()
	d, superseded, err := e.opts.Store.EnqueueRollback(ctx, target, e.opts.Clock.Now())
	if err != nil {
		return store.Deployment{}, err
	}
	if e.afterInsert != nil {
		e.afterInsert()
	}
	for _, id := range superseded {
		appendLogLine(e.LogPath(app.Slug, id), "Canceled: "+store.SupersededError+".")
		e.report(StatusChange{DeploymentID: id})
	}
	e.report(StatusChange{DeploymentID: d.ID})
	path := e.LogPath(app.Slug, d.ID)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		e.opts.Logger.Warn("removing a stale deploy log failed", "path", path, "error", err)
	}
	appendLogLine(path, fmt.Sprintf("Queued rollback to deployment #%d (%s).", target.ID, d.CommitSHA))
	e.notify()
	return d, nil
}

// executeRollback starts the kept release of d's source again. touched
// reports whether the running containers may have been changed.
func (e *Engine) executeRollback(ctx context.Context, d store.Deployment, app store.App, log *deployLog) (touched bool, err error) {
	src := *d.SourceDeploymentID

	log.Step(StepPrepare, "Reusing the release of deployment #%d", src)
	if !e.ReleaseAvailable(app, src) {
		return false, ErrReleaseUnavailable
	}
	meta, err := e.readMeta(app.Slug, src)
	if err != nil {
		return false, ErrReleaseUnavailable
	}
	env, releaseEnv, err := e.readEnvSnapshots(app.Slug, src)
	if err != nil {
		return false, ErrReleaseUnavailable
	}
	// The environment snapshot is what the release ran with; the env tab
	// compares the saved variables with it (ARCHITECTURE 9.2).
	fingerprints, ok, err := e.opts.Store.DeploymentEnvFingerprints(ctx, src)
	if err != nil {
		return false, err
	}
	if ok {
		if err := e.opts.Store.SetDeploymentEnvFingerprints(ctx, d.ID, fingerprints); err != nil {
			return false, err
		}
	}
	log.Printf("Compose file: %s; images: %v", meta.ComposeFile, meta.Images)

	log.Step(StepValidate, "Validating the release again")
	project, err := e.revalidatedProject(ctx, app, src, meta.ComposeFile)
	if err != nil {
		return false, err
	}

	// The kept images are normally all there; a missing one is pulled.
	dockerConfig, err := e.registryConfig(ctx, app, log.masker)
	if err != nil {
		return false, err
	}
	if app.BlueGreen && meta.BlueGreen {
		// The kept release's override has both colours: it starts next to
		// the serving one, like a deployment (ARCHITECTURE 7.2).
		return e.deployBlueGreen(ctx, d, app, blueGreenRelease{
			id: src, composeFile: meta.ComposeFile, project: project, dockerConfig: dockerConfig,
			env: env, releaseEnv: releaseEnv, releaseEnvServices: meta.ReleaseEnvServices,
		}, log)
	}

	log.Step(StepStart, "Starting containers")
	if app.BlueGreen {
		log.Printf("The release predates blue/green deploys of this app: the public service is recreated in place.")
	}
	e.logStopGraces(ctx, app, project, log)
	if err := e.endSleep(ctx, app, log); err != nil {
		return false, err
	}
	if err := e.writeLiveEnv(app.Slug, env, releaseEnv); err != nil {
		return false, fmt.Errorf("write env file: %w", err)
	}
	if meta.BlueGreen {
		if err := e.opts.Store.SetDeploymentWebColor(ctx, d.ID, e.releaseColor(app, src)); err != nil {
			return false, err
		}
	}
	if err := e.upRelease(ctx, app, src, dockerConfig, e.deployRun(log)); err != nil {
		line := e.logContainerOutput(ctx, app, log)
		return true, withOutput(fmt.Errorf("start containers: %w", err), line)
	}
	return e.routeAndCheck(ctx, app, log)
}
