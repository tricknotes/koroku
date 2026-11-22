package deploy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// GitHubDeploymentsAPI is the part of the GitHub App client that records
// deployments on GitHub.
type GitHubDeploymentsAPI interface {
	CreateDeployment(ctx context.Context, owner, repo string, in github.NewDeployment) (int64, error)
	CreateDeploymentStatus(ctx context.Context, owner, repo string, id int64, in github.DeploymentStatus) error
}

// GitHubDeploymentsOptions configure a GitHubDeployments reporter.
type GitHubDeploymentsOptions struct {
	Store  *store.Store
	Client GitHubDeploymentsAPI
	Clock  clock.Clock
	Logger *slog.Logger
	// ConsoleURL is the console's origin; log_url points at its deploy page.
	ConsoleURL string
	// BaseDomain gives the automatic URL of apps without a primary domain.
	BaseDomain string

	// CallTimeout bounds one request to GitHub. Default 10 seconds.
	CallTimeout time.Duration
	// RetryDelays are the waits before the retries of a request that failed
	// on the network, with a 5xx or by a rate limit. Default 2s and 10s.
	RetryDelays []time.Duration
	// DeniedFor is how long an app is skipped after GitHub refused the
	// App's permission (403). Default one hour.
	DeniedFor time.Duration
	// ResyncInterval is how often finished deployments whose final status
	// did not reach GitHub are sent again. Default 10 minutes.
	ResyncInterval time.Duration
}

// GitHubDeployments mirrors koroku's deployments into the deployments of the
// GitHub repository (ARCHITECTURE 5.1). It implements StatusReporter.
//
// Everything is best effort and off the deploy path: Report only queues the
// change, one goroutine (Run) talks to GitHub, and failures are logged.
type GitHubDeployments struct {
	opts  GitHubDeploymentsOptions
	queue chan StatusChange

	mu sync.Mutex
	// denied holds, per app ID, until when the app is skipped after a 403.
	denied map[int64]time.Time
}

// githubQueueSize bounds the status changes waiting for GitHub. A deployment
// makes three or four; more than this means GitHub is unreachable.
const githubQueueSize = 256

// resyncWindow is how far back finished deployments are sent again.
const resyncWindow = 24 * time.Hour

// NewGitHubDeployments builds the reporter. Call Run to start it.
func NewGitHubDeployments(opts GitHubDeploymentsOptions) *GitHubDeployments {
	if opts.Clock == nil {
		opts.Clock = clock.System{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.CallTimeout == 0 {
		opts.CallTimeout = 10 * time.Second
	}
	if opts.RetryDelays == nil {
		opts.RetryDelays = []time.Duration{2 * time.Second, 10 * time.Second}
	}
	if opts.DeniedFor == 0 {
		opts.DeniedFor = time.Hour
	}
	if opts.ResyncInterval == 0 {
		opts.ResyncInterval = 10 * time.Minute
	}
	return &GitHubDeployments{
		opts:   opts,
		queue:  make(chan StatusChange, githubQueueSize),
		denied: map[int64]time.Time{},
	}
}

// Report queues a status change. It never blocks; when the queue is full
// the change is dropped (the periodic resync sends final statuses again).
func (g *GitHubDeployments) Report(change StatusChange) {
	select {
	case g.queue <- change:
	default:
		g.opts.Logger.Warn("GitHub deployment updates are backed up; dropping one",
			"deployment", change.DeploymentID)
	}
}

// Run sends queued changes to GitHub until ctx is canceled. At start and
// then every ResyncInterval it also sends the final status of deployments
// that finished without one reaching GitHub (koroku stopped, or GitHub
// failed, in between).
func (g *GitHubDeployments) Run(ctx context.Context) {
	g.resync(ctx)
	ticker := time.NewTicker(g.opts.ResyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case change := <-g.queue:
			g.sync(ctx, change)
		case <-ticker.C:
			g.resync(ctx)
		}
	}
}

func (g *GitHubDeployments) resync(ctx context.Context) {
	ids, err := g.opts.Store.StaleGitHubDeployments(ctx, g.opts.Clock.Now().Add(-resyncWindow), 50)
	if err != nil {
		if ctx.Err() == nil {
			g.opts.Logger.Warn("listing deployments to send to GitHub again failed", "error", err)
		}
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		g.sync(ctx, StatusChange{DeploymentID: id})
	}
}

// stateRank orders GitHub states so that a status is never replaced by an
// earlier one (changes may be handled after the deployment moved on).
func stateRank(state string) int {
	switch state {
	case github.DeploymentQueued:
		return 1
	case github.DeploymentInProgress:
		return 2
	case github.DeploymentSuccess, github.DeploymentFailure, github.DeploymentError, stateGone:
		return 3
	}
	return 0
}

// stateGone is stored when GitHub no longer knows the deployment (it was
// deleted there); nothing more is sent for it.
const stateGone = "gone"

// sync brings the GitHub deployment of one koroku deployment up to date.
func (g *GitHubDeployments) sync(ctx context.Context, change StatusChange) {
	d, err := g.opts.Store.GetDeployment(ctx, change.DeploymentID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			g.opts.Logger.Warn("loading a deployment for GitHub failed", "deployment", change.DeploymentID, "error", err)
		}
		return
	}
	app, err := g.opts.Store.GetAppByID(ctx, d.AppID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			g.opts.Logger.Warn("loading an app for GitHub failed", "deployment", d.ID, "error", err)
		}
		return
	}
	owner, repo, ok := githubRepo(app)
	if !ok || g.isDenied(app.ID) {
		return
	}
	logger := g.opts.Logger.With("app", app.Slug, "deployment", d.ID)
	status := describeStatus(d, change.RolledBack)

	link, err := g.opts.Store.GetGitHubDeployment(ctx, d.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if d.Status == store.StatusCanceled {
			// Canceled before GitHub heard of it: nothing to show there.
			return
		}
		// A review app is an environment that will be deleted
		// (ARCHITECTURE 9.9).
		_, reviewErr := g.opts.Store.GetReviewApp(ctx, app.ID)
		transient := reviewErr == nil
		var id int64
		err := g.call(ctx, func(ctx context.Context) error {
			var err error
			id, err = g.opts.Client.CreateDeployment(ctx, owner, repo, github.NewDeployment{
				SHA:         d.CommitSHA,
				Environment: app.Slug,
				Description: deploymentDescription(d),
				Production:  app.Environment == store.EnvironmentProduction,
				Transient:   transient,
			})
			return err
		})
		if err != nil {
			g.failed(logger, app.ID, "creating the GitHub deployment failed", err)
			return
		}
		if err := g.opts.Store.SaveGitHubDeployment(ctx, d.ID, id, g.opts.Clock.Now()); err != nil {
			logger.Warn("recording the GitHub deployment failed", "error", err)
			return
		}
		link = store.GitHubDeployment{DeploymentID: d.ID, GitHubID: id}
	case err != nil:
		if ctx.Err() == nil {
			logger.Warn("loading the GitHub deployment failed", "error", err)
		}
		return
	}
	if stateRank(status.State) <= stateRank(link.State) {
		return
	}
	status.LogURL = g.logURL(app, d)
	if status.State == github.DeploymentSuccess {
		status.EnvironmentURL = g.environmentURL(ctx, app)
	}
	err = g.call(ctx, func(ctx context.Context) error {
		return g.opts.Client.CreateDeploymentStatus(ctx, owner, repo, link.GitHubID, status)
	})
	state := status.State
	if errors.Is(err, github.ErrNotFound) {
		logger.Info("the GitHub deployment no longer exists; not updating it", "github_deployment", link.GitHubID)
		state, err = stateGone, nil
	}
	if err != nil {
		g.failed(logger, app.ID, "updating the GitHub deployment failed", err)
		return
	}
	if err := g.opts.Store.SetGitHubDeploymentState(ctx, d.ID, state, g.opts.Clock.Now()); err != nil {
		logger.Warn("recording the GitHub deployment status failed", "error", err)
	}
}

// call runs one request with a timeout, retrying failures that may pass:
// network errors, 5xx and rate limits.
func (g *GitHubDeployments) call(ctx context.Context, do func(context.Context) error) error {
	for attempt := 0; ; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, g.opts.CallTimeout)
		err := do(callCtx)
		cancel()
		if err == nil || !retryable(err) || attempt >= len(g.opts.RetryDelays) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(g.opts.RetryDelays[attempt]):
		}
	}
}

func retryable(err error) bool {
	switch {
	case errors.Is(err, github.ErrRateLimited):
		return true
	case errors.Is(err, github.ErrNotFound), errors.Is(err, github.ErrUnauthorized),
		errors.Is(err, github.ErrInvalidArgument), errors.Is(err, github.ErrNotConfigured):
		return false
	}
	var apiErr *github.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= http.StatusInternalServerError
	}
	// Network errors and timeouts.
	return true
}

// failed logs a failed request. A refused permission (the App lacks
// Deployments: write) is logged once and the app is skipped for a while;
// a repository the App is not installed on is skipped silently.
func (g *GitHubDeployments) failed(logger *slog.Logger, appID int64, msg string, err error) {
	switch {
	case errors.Is(err, github.ErrUnauthorized):
		g.mu.Lock()
		g.denied[appID] = g.opts.Clock.Now().Add(g.opts.DeniedFor)
		g.mu.Unlock()
		logger.Warn("GitHub refused to record deployments; give the GitHub App the "+
			"Deployments: Read and write permission (docs/SETUP.md 4.1). Skipping this app for a while",
			"error", err, "skip_for", g.opts.DeniedFor.String())
	case errors.Is(err, github.ErrNotFound):
		// A public repository the App is not installed on.
		logger.Debug("the GitHub App cannot see the repository; not recording deployments", "error", err)
	default:
		logger.Warn(msg, "error", err)
	}
}

func (g *GitHubDeployments) isDenied(appID int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	until, ok := g.denied[appID]
	if !ok {
		return false
	}
	if g.opts.Clock.Now().Before(until) {
		return true
	}
	delete(g.denied, appID)
	return false
}

// logURL is the console's deploy page with the deployment opened.
func (g *GitHubDeployments) logURL(app store.App, d store.Deployment) string {
	if g.opts.ConsoleURL == "" {
		return ""
	}
	return g.opts.ConsoleURL + "/apps/" + app.Slug + "/deploy?deployment=" + strconv.FormatInt(d.ID, 10)
}

// environmentURL is the app's primary URL.
func (g *GitHubDeployments) environmentURL(ctx context.Context, app store.App) string {
	return PrimaryURL(ctx, g.opts.Store, app, g.opts.BaseDomain)
}

// PrimaryURL is the app's primary URL: its primary domain when that is
// active, otherwise the automatic one (the same rule as the console's
// primary_url). It is "" without a base domain and a primary domain.
func PrimaryURL(ctx context.Context, st *store.Store, app store.App, baseDomain string) string {
	if domains, err := st.ListDomains(ctx, app.ID); err == nil {
		for _, dom := range domains {
			if dom.IsPrimary && dom.Status != store.DomainPending {
				return "https://" + dom.Hostname
			}
		}
	}
	if baseDomain == "" {
		return ""
	}
	return "https://" + app.Slug + "." + baseDomain
}

// deploymentKind names a deployment in the descriptions shown on GitHub.
func deploymentKind(d store.Deployment) string {
	switch {
	case d.SourceDeploymentID != nil:
		return fmt.Sprintf("Rollback to koroku deployment #%d", *d.SourceDeploymentID)
	case d.PromotedFromID != nil:
		return "Promotion from staging"
	}
	return "Deployment"
}

func deploymentDescription(d store.Deployment) string {
	return deploymentKind(d) + " by koroku"
}

// describeStatus maps koroku's state machine onto GitHub's states. The
// description never carries the error message: it may name paths or
// commands of the host, and GitHub shows it to everyone who can read the
// repository. The console (log_url) has the details.
func describeStatus(d store.Deployment, rolledBack bool) github.DeploymentStatus {
	kind := deploymentKind(d)
	switch d.Status {
	case store.StatusQueued:
		return github.DeploymentStatus{State: github.DeploymentQueued, Description: kind + " queued"}
	case store.StatusRunning:
		return github.DeploymentStatus{State: github.DeploymentInProgress, Description: kind + " in progress"}
	case store.StatusSucceeded:
		return github.DeploymentStatus{State: github.DeploymentSuccess, Description: kind + " succeeded"}
	case store.StatusCanceled:
		if d.Error == store.SupersededError {
			return github.DeploymentStatus{State: github.DeploymentError, Description: kind + " superseded by a newer one"}
		}
		return github.DeploymentStatus{State: github.DeploymentError, Description: kind + " canceled"}
	}
	// Failed.
	if d.Error == store.InterruptedError {
		return github.DeploymentStatus{State: github.DeploymentError,
			Description: kind + " interrupted: koroku restarted"}
	}
	desc := kind + " failed"
	if rolledBack {
		desc += "; the previous release was restored"
	}
	return github.DeploymentStatus{State: github.DeploymentFailure, Description: desc}
}
