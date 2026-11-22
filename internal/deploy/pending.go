package deploy

import (
	"context"
	"errors"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// ErrDeferred is returned by EnqueuePush and EnqueueWorkflowRun when a
// command runs on the app: the deployment was not queued but kept, and is
// queued when the command ends (ARCHITECTURE 5.2).
var ErrDeferred = errors.New("a command is running on the app; the deployment is queued when it ends")

// ActAutoDeployDropped is the activity koroku records when it could not
// queue a deferred auto-deploy (ARCHITECTURE 10.7).
const ActAutoDeployDropped = "auto_deploy.dropped"

// Reasons of ActAutoDeployDropped.
const (
	DropDiskFull           = "disk_full"
	DropAutoDeployDisabled = "auto_deploy_disabled"
	DropStopped            = "stopped"
	DropBranchChanged      = "branch_changed"
	DropWorkflowChanged    = "workflow_changed"
)

// pendingTimeout bounds queueing one deferred auto-deploy.
const pendingTimeout = 30 * time.Second

// queueAllPendingAutoDeploys queues what was deferred before a restart.
// No command runs yet when the worker starts.
func (e *Engine) queueAllPendingAutoDeploys(ctx context.Context) {
	list, err := e.opts.Store.ListPendingAutoDeploys(ctx)
	if err != nil {
		if ctx.Err() == nil {
			e.opts.Logger.Error("reading deferred auto-deploys failed; they are retried after the next command", "error", err)
		}
		return
	}
	for _, p := range list {
		e.queuePendingAutoDeploy(ctx, p.AppID)
	}
}

// queuePendingAutoDeploy queues the auto-deploy kept for the app while a
// command ran, once nothing runs on it any more. Settings changed since the
// push win: a deployment the app would no longer get is dropped, and so is
// one the disk usage limit refuses. Both leave an activity entry, since
// GitHub was told the delivery went through. A database failure keeps the
// row for the next command's end or the next start.
func (e *Engine) queuePendingAutoDeploy(parent context.Context, appID int64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), pendingTimeout)
	defer cancel()
	logger := e.opts.Logger.With("app_id", appID)
	p, err := e.opts.Store.GetPendingAutoDeploy(ctx, appID)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		logger.Error("reading a deferred auto-deploy failed; it is retried later", "error", err)
		return
	}
	app, err := e.opts.Store.GetAppByID(ctx, appID)
	if errors.Is(err, store.ErrNotFound) {
		return // deleted with the app
	}
	if err != nil {
		logger.Error("loading the app of a deferred auto-deploy failed; it is retried later", "error", err)
		return
	}
	logger = logger.With("app", app.Slug, "commit", p.CommitSHA)
	if reason := pendingDropReason(app, p); reason != "" {
		e.dropPending(ctx, app, p, reason)
		return
	}
	d, err := e.enqueue(ctx, app, p.Ref, p.CommitSHA, p.CommitMessage, nil,
		store.Origin{Trigger: store.TriggerPush, Pusher: p.Pusher, Workflow: p.Workflow, Deferred: true})
	var diskFull *DiskFullError
	switch {
	case err == nil:
		logger.Info("deferred auto-deploy queued", "deployment", d.ID)
	case errors.Is(err, ErrDeferred):
		// Another command started first; its end queues this.
	case errors.As(err, &diskFull):
		logger.Warn("a deferred auto-deploy could not be queued", "error", err)
		e.dropPending(ctx, app, p, DropDiskFull)
	default:
		logger.Error("queueing a deferred auto-deploy failed; it is retried later", "error", err)
	}
}

// pendingDropReason says why the app would no longer deploy p, or "".
func pendingDropReason(app store.App, p store.PendingAutoDeploy) string {
	switch {
	case !app.AutoDeploy || !app.UsesGitHubApp():
		return DropAutoDeployDisabled
	case app.DesiredState == store.DesiredStopped:
		return DropStopped
	case app.EffectiveAutoDeployBranch() != p.Ref:
		return DropBranchChanged
	case app.AutoDeployWorkflow != p.Workflow:
		return DropWorkflowChanged
	}
	return ""
}

func (e *Engine) dropPending(ctx context.Context, app store.App, p store.PendingAutoDeploy, reason string) {
	if err := e.opts.Store.DeletePendingAutoDeploy(ctx, app.ID); err != nil {
		e.opts.Logger.Error("forgetting a deferred auto-deploy failed", "app", app.Slug, "error", err)
		return
	}
	e.opts.Logger.Warn("deferred auto-deploy dropped", "app", app.Slug, "commit", p.CommitSHA, "reason", reason)
	if _, err := e.opts.Store.RecordActivity(ctx, app.ID, ActAutoDeployDropped, map[string]any{
		"commit_sha": p.CommitSHA,
		"branch":     p.Ref,
		"pusher":     p.Pusher,
		"workflow":   p.Workflow,
		"reason":     reason,
	}, e.opts.Clock.Now()); err != nil {
		e.opts.Logger.Error("recording activity failed", "action", ActAutoDeployDropped, "error", err)
	}
}
