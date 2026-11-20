package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// Promoter queues promotions and previews their image reuse
// (ARCHITECTURE 9.7). The deploy engine implements it.
type Promoter interface {
	EnqueuePromotion(ctx context.Context, app store.App, source store.Deployment) (store.Deployment, error)
	PreviewPromotionImages(ctx context.Context, target, src store.App, source store.Deployment) ([]deploy.ImageDecision, error)
}

// Why a promotion cannot happen. They are part of the API.
const (
	promotionNotPaired          = "not_paired"
	promotionNotProduction      = "not_production"
	promotionSourceNotStaging   = "source_not_staging"
	promotionRepositoryMismatch = "repository_mismatch"
	promotionNoSourceRelease    = "no_source_release"
)

// promotionCommitLimit is how far back the preview looks for production's
// commit in staging's history.
const promotionCommitLimit = 30

// sameRepository reports whether two apps deploy from one repository.
func sameRepository(a, b store.App) bool {
	return a.CloneURL() != "" && strings.EqualFold(a.CloneURL(), b.CloneURL())
}

// pairingProblem checks a pairing of target (promoting) and source
// (promoted from) without looking at releases. "" means it is valid.
func pairingProblem(target, source store.App) string {
	switch {
	case target.Environment != store.EnvironmentProduction:
		return promotionNotProduction
	case source.Environment != store.EnvironmentStaging:
		return promotionSourceNotStaging
	case !sameRepository(target, source):
		return promotionRepositoryMismatch
	}
	return ""
}

// promotionSource resolves the app's promotion source and its current
// release. reason is non-empty when the app cannot promote now.
func (s *Server) promotionSource(ctx context.Context, app store.App) (store.App, store.Deployment, string, error) {
	if app.PromoteFromAppID == nil {
		return store.App{}, store.Deployment{}, promotionNotPaired, nil
	}
	source, err := s.store.GetAppByID(ctx, *app.PromoteFromAppID)
	if errors.Is(err, store.ErrNotFound) {
		return store.App{}, store.Deployment{}, promotionNotPaired, nil
	}
	if err != nil {
		return store.App{}, store.Deployment{}, "", err
	}
	if reason := pairingProblem(app, source); reason != "" {
		return source, store.Deployment{}, reason, nil
	}
	if source.CurrentDeploymentID == nil {
		return source, store.Deployment{}, promotionNoSourceRelease, nil
	}
	d, err := s.store.GetDeployment(ctx, *source.CurrentDeploymentID)
	if errors.Is(err, store.ErrNotFound) {
		return source, store.Deployment{}, promotionNoSourceRelease, nil
	}
	if err != nil {
		return store.App{}, store.Deployment{}, "", err
	}
	return source, d, "", nil
}

// applyPromoteFrom validates and sets the promote_from field of an app
// input. It runs after the other fields are applied, so a request may
// make the app production and pair it at once.
func (s *Server) applyPromoteFrom(ctx context.Context, in appInput, app *store.App, errs fieldErrors) error {
	if in.PromoteFrom == nil {
		return nil
	}
	slug := strings.TrimSpace(*in.PromoteFrom)
	if slug == "" {
		app.PromoteFromAppID = nil
		return nil
	}
	if slug == app.Slug {
		errs.add("promote_from", "cannot be the app itself")
		return nil
	}
	source, err := s.store.GetApp(ctx, slug)
	if errors.Is(err, store.ErrNotFound) {
		errs.add("promote_from", "no such app")
		return nil
	}
	if err != nil {
		return err
	}
	switch pairingProblem(*app, source) {
	case promotionNotProduction:
		errs.add("promote_from", "only a production app can promote from another app")
	case promotionSourceNotStaging:
		errs.add("promote_from", "must be a staging app")
	case promotionRepositoryMismatch:
		errs.add("promote_from", "must deploy the same repository")
	default:
		app.PromoteFromAppID = &source.ID
	}
	return nil
}

// promoteFromSlug returns the slug of the app's promotion source, or "".
func (s *Server) promoteFromSlug(ctx context.Context, app store.App) (string, error) {
	if app.PromoteFromAppID == nil {
		return "", nil
	}
	source, err := s.store.GetAppByID(ctx, *app.PromoteFromAppID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return source.Slug, nil
}

// handlePromote queues a promotion of {"promote_from_deployment": id}.
func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request, app store.App, sourceID int64) {
	if s.promoter == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "promotion is not available")
		return
	}
	_, current, reason, err := s.promotionSource(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if reason != "" {
		writeJSON(w, http.StatusConflict, errorBody{Error: errorDetail{
			Code:    "promotion_unavailable",
			Message: "this app cannot promote a release: " + reason,
			Details: map[string]any{"reason": reason},
		}})
		return
	}
	if current.ID != sourceID {
		writeError(w, http.StatusConflict, "promotion_stale",
			"the deployment is not the current release of the source app any more")
		return
	}
	d, err := s.promoter.EnqueuePromotion(r.Context(), app, current)
	if s.writeEnqueueError(w, r, err) {
		return
	}
	writeJSON(w, http.StatusAccepted, s.deploymentToJSON(r.Context(), app, d))
}

type promotionCommitJSON struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message"`
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
}

type promotionImageJSON struct {
	Service       string   `json:"service"`
	Reuse         bool     `json:"reuse"`
	Reason        string   `json:"reason"`
	DifferingArgs []string `json:"differing_args"`
}

type promotionJSON struct {
	Status            string          `json:"status"`
	Reason            string          `json:"reason"`
	SourceApp         string          `json:"source_app"`
	SourceDeployment  *deploymentJSON `json:"source_deployment"`
	CurrentDeployment *deploymentJSON `json:"current_deployment"`
	Commits           struct {
		Status string                `json:"status"`
		List   []promotionCommitJSON `json:"list"`
	} `json:"commits"`
	Env struct {
		OnlyInSource []string `json:"only_in_source"`
		OnlyInTarget []string `json:"only_in_target"`
	} `json:"env"`
	Images struct {
		Status   string               `json:"status"`
		Services []promotionImageJSON `json:"services"`
	} `json:"images"`
}

// handlePromotionPreview returns what a promotion would deploy and how it
// differs from production, for the confirmation (ARCHITECTURE 9.7). It
// reads keys and hashes only, never values.
func (s *Server) handlePromotionPreview(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	out := promotionJSON{Status: "ready"}
	out.Commits.Status, out.Commits.List = "unknown", []promotionCommitJSON{}
	out.Env.OnlyInSource, out.Env.OnlyInTarget = []string{}, []string{}
	out.Images.Status, out.Images.Services = "unavailable", []promotionImageJSON{}

	source, sourceDeployment, reason, err := s.promotionSource(r.Context(), app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out.SourceApp = source.Slug
	switch reason {
	case "":
	case promotionNotPaired:
		out.Status = "not_paired"
		writeJSON(w, http.StatusOK, out)
		return
	default:
		out.Status, out.Reason = "unavailable", reason
		writeJSON(w, http.StatusOK, out)
		return
	}

	src := s.deploymentToJSON(r.Context(), source, sourceDeployment)
	out.SourceDeployment = &src
	var current *store.Deployment
	if app.CurrentDeploymentID != nil {
		d, err := s.store.GetDeployment(r.Context(), *app.CurrentDeploymentID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, err)
			return
		}
		if err == nil {
			current = &d
			j := s.deploymentToJSON(r.Context(), app, d)
			out.CurrentDeployment = &j
		}
	}

	onlySource, onlyTarget, err := s.envKeyDifference(r.Context(), source, app)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out.Env.OnlyInSource, out.Env.OnlyInTarget = onlySource, onlyTarget

	if current != nil {
		if list, ok := s.commitsBetween(r.Context(), app, sourceDeployment.CommitSHA, current.CommitSHA); ok {
			out.Commits.Status, out.Commits.List = "ok", list
		}
	}

	if s.promoter != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		decisions, err := s.promoter.PreviewPromotionImages(ctx, app, source, sourceDeployment)
		if err != nil {
			// The deployment decides again; the preview only informs.
			s.log.Warn("previewing image reuse failed", "app", app.Slug, "error", err)
		} else {
			out.Images.Status = "checked"
			for _, d := range decisions {
				args := d.DifferingArgs
				if args == nil {
					args = []string{}
				}
				out.Images.Services = append(out.Images.Services, promotionImageJSON{
					Service: d.Service, Reuse: d.Reuse, Reason: d.Reason, DifferingArgs: args,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// envKeyDifference lists the variable keys only one of the apps has.
func (s *Server) envKeyDifference(ctx context.Context, source, target store.App) ([]string, []string, error) {
	keys := func(app store.App) (map[string]bool, error) {
		rows, err := s.store.ListEnvVars(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, row := range rows {
			set[row.Key] = true
		}
		return set, nil
	}
	a, err := keys(source)
	if err != nil {
		return nil, nil, err
	}
	b, err := keys(target)
	if err != nil {
		return nil, nil, err
	}
	only := func(x, y map[string]bool) []string {
		out := []string{}
		for k := range x {
			if !y[k] {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return out
	}
	return only(a, b), only(b, a), nil
}

// commitsBetween lists the commits from head back to (excluding) base
// through the GitHub App. ok is false when that is not known: no GitHub
// App, a repository it cannot read, or base not within the limit.
func (s *Server) commitsBetween(ctx context.Context, app store.App, head, base string) ([]promotionCommitJSON, bool) {
	list := []promotionCommitJSON{}
	if head == base {
		return list, true
	}
	if s.auth.GitHub == nil || app.RepoURL != "" || app.RepoFullName == "" {
		return nil, false
	}
	owner, repo, err := github.SplitFullName(app.RepoFullName)
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, githubTimeout)
	defer cancel()
	commits, err := s.auth.GitHub.ListCommits(ctx, owner, repo, head, promotionCommitLimit)
	if err != nil {
		s.log.Warn("listing commits for a promotion failed", "app", app.Slug, "error", err)
		return nil, false
	}
	for _, c := range commits {
		if c.SHA == base {
			return list, true
		}
		subject, _, _ := strings.Cut(c.Message, "\n")
		list = append(list, promotionCommitJSON{SHA: c.SHA, Message: subject, Author: c.Author, Date: c.Date})
	}
	return nil, false
}
