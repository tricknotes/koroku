// Package review keeps review apps going after they are created
// (ARCHITECTURE 9.9): it copies the parent's data once the first
// deployment succeeded and refreshes the state of the pull requests.
package review

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// Defaults of the Watcher.
const (
	DefaultInterval   = 15 * time.Second
	DefaultPRInterval = 10 * time.Minute
)

// Why a copy into a review app was not started, as stored in
// review_apps.data_copy_detail. The console words them.
const (
	DetailNoDumpHook       = "no_dump_hook"
	DetailNoLoadHook       = "no_load_hook"
	DetailSourceNotRunning = "source_not_running"
	DetailTargetNotRunning = "target_not_running"
	DetailTargetNotStaging = "target_not_staging"
	DetailParentMissing    = "parent_missing"
	DetailError            = "error"
)

// Copier starts database copies (the deploy engine).
type Copier interface {
	StartCopy(ctx context.Context, req deploy.CopyRequest) (store.DBCopy, error)
}

// PullRequests reads one pull request (the GitHub App client).
type PullRequests interface {
	GetPullRequest(ctx context.Context, owner, repo string, number int) (github.PullRequest, error)
}

// Watcher is the one loop of koroku serve that looks after review apps.
type Watcher struct {
	Store  *store.Store
	Copier Copier
	// Pulls is optional; without it the state of pull requests is not
	// refreshed.
	Pulls  PullRequests
	Clock  clock.Clock
	Logger *slog.Logger
	// Interval is how often waiting copies are looked at; PRInterval how
	// often the pull requests are read again.
	Interval   time.Duration
	PRInterval time.Duration

	kick chan struct{}
}

// New returns a Watcher with the defaults filled in.
func New(w Watcher) *Watcher {
	if w.Clock == nil {
		w.Clock = clock.System{}
	}
	if w.Logger == nil {
		w.Logger = slog.New(slog.DiscardHandler)
	}
	if w.Interval <= 0 {
		w.Interval = DefaultInterval
	}
	if w.PRInterval <= 0 {
		w.PRInterval = DefaultPRInterval
	}
	w.kick = make(chan struct{}, 1)
	return &w
}

// Kick makes the watcher look at the waiting copies now (after a review app
// was created).
func (w *Watcher) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run looks after review apps until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	var lastPR time.Time
	for {
		w.StartCopies(ctx)
		if now := w.Clock.Now(); now.Sub(lastPR) >= w.PRInterval {
			lastPR = now
			w.RefreshPullRequests(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-w.kick:
		}
	}
}

// StartCopies starts the copy of the parent's data into each review app
// that waits for one and has a successful deployment.
func (w *Watcher) StartCopies(ctx context.Context) {
	reviews, err := w.Store.ListReviewApps(ctx, 0)
	if err != nil {
		if ctx.Err() == nil {
			w.Logger.Error("listing review apps failed", "error", err)
		}
		return
	}
	for _, r := range reviews {
		if r.DataCopy != store.DataCopyWaiting {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		w.startCopy(ctx, r)
	}
}

func (w *Watcher) startCopy(ctx context.Context, r store.ReviewApp) {
	app, err := w.Store.GetAppByID(ctx, r.AppID)
	if err != nil {
		w.Logger.Error("loading a review app failed", "app_id", r.AppID, "error", err)
		return
	}
	if app.CurrentDeploymentID == nil {
		// The load hook's service runs once a deployment succeeded.
		return
	}
	logger := w.Logger.With("app", app.Slug)
	parent, err := w.Store.GetAppByID(ctx, r.ParentAppID)
	if errors.Is(err, store.ErrNotFound) {
		w.setCopy(ctx, logger, app, store.DataCopyFailed, DetailParentMissing)
		return
	}
	if err != nil {
		logger.Error("loading the parent of a review app failed", "error", err)
		return
	}
	// The hooks are those of the parent, copied when the app was made;
	// the target's own settings decide, as for any copy.
	switch {
	case parent.DumpHookService == "" || parent.DumpHookCommand == "":
		w.setCopy(ctx, logger, app, store.DataCopySkipped, DetailNoDumpHook)
		return
	case app.LoadHookService == "" || app.LoadHookCommand == "":
		w.setCopy(ctx, logger, app, store.DataCopySkipped, DetailNoLoadHook)
		return
	}
	c, err := w.Copier.StartCopy(ctx, deploy.CopyRequest{
		Source: parent, Target: app, Kind: store.CopySourceLive, SkipBackup: true,
	})
	if errors.Is(err, deploy.ErrBusy) {
		// The parent or the review app is deploying or busy: next round.
		return
	}
	if err != nil {
		detail := DetailError
		switch {
		case errors.Is(err, deploy.ErrSourceNotRunning):
			detail = DetailSourceNotRunning
		case errors.Is(err, deploy.ErrTargetNotRunning):
			detail = DetailTargetNotRunning
		case errors.Is(err, deploy.ErrTargetNotStaging):
			detail = DetailTargetNotStaging
		case errors.Is(err, deploy.ErrNoDumpHook):
			detail = DetailNoDumpHook
		case errors.Is(err, deploy.ErrNoLoadHook):
			detail = DetailNoLoadHook
		default:
			logger.Error("starting the copy of the parent's data failed", "error", err)
		}
		w.setCopy(ctx, logger, app, store.DataCopyFailed, detail)
		return
	}
	logger.Info("copying the parent's data into the review app", "copy", c.ID, "parent", parent.Slug)
	w.setCopy(ctx, logger, app, store.DataCopyStarted, "")
	now := w.Clock.Now()
	// koroku itself started the copy (no actor), like any other copy it is
	// on both apps' records (ARCHITECTURE 10.7).
	if _, err := w.Store.RecordActivity(ctx, app.ID, "db_copy.started",
		map[string]any{"copy_id": c.ID, "source": parent.Slug, "source_kind": c.SourceKind}, now); err != nil {
		logger.Error("recording the copy failed", "error", err)
	}
	if _, err := w.Store.RecordActivity(ctx, parent.ID, "db_copy.sent",
		map[string]any{"source_kind": c.SourceKind}, now); err != nil {
		logger.Error("recording the copy failed", "error", err)
	}
}

func (w *Watcher) setCopy(ctx context.Context, logger *slog.Logger, app store.App, state, detail string) {
	if state != store.DataCopyStarted {
		logger.Info("the parent's data is not copied into the review app", "state", state, "reason", detail)
	}
	if err := w.Store.SetReviewDataCopy(ctx, app.ID, state, detail); err != nil {
		logger.Error("recording the copy of a review app failed", "error", err)
	}
}

// RefreshPullRequests reads the pull request of every review app again.
// A failure keeps the state that was known.
func (w *Watcher) RefreshPullRequests(ctx context.Context) {
	if w.Pulls == nil {
		return
	}
	reviews, err := w.Store.ListReviewApps(ctx, 0)
	if err != nil {
		if ctx.Err() == nil {
			w.Logger.Error("listing review apps failed", "error", err)
		}
		return
	}
	for _, r := range reviews {
		if ctx.Err() != nil {
			return
		}
		app, err := w.Store.GetAppByID(ctx, r.AppID)
		if err != nil {
			continue
		}
		owner, repo, ok := strings.Cut(app.RepoFullName, "/")
		if !ok {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		pr, err := w.Pulls.GetPullRequest(callCtx, owner, repo, r.PRNumber)
		cancel()
		if err != nil {
			w.Logger.Warn("reading the pull request of a review app failed", "app", app.Slug, "error", err)
			continue
		}
		if err := w.Store.UpdateReviewPullRequest(ctx, r.AppID, pr.Title, pr.HeadBranch, pr.State, w.Clock.Now()); err != nil {
			w.Logger.Error("recording the pull request of a review app failed", "app", app.Slug, "error", err)
		}
	}
}
