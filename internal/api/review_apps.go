package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// Review apps (ARCHITECTURE 9.9).

// PullRequests reads the pull requests of a repository through the GitHub
// App.
type PullRequests interface {
	ListPullRequests(ctx context.Context, owner, repo string) ([]github.PullRequest, error)
	GetPullRequest(ctx context.Context, owner, repo string, number int) (github.PullRequest, error)
}

// HostVerifier checks that a hostname resolves to the host's public IP
// only, as custom domains are verified (ARCHITECTURE 8.1).
type HostVerifier interface {
	Verify(ctx context.Context, hostname string) error
}

// ReviewWatcher copies the parent's data into review apps; Kick makes it
// look at a new one at once.
type ReviewWatcher interface {
	Kick()
}

// maxSlugLength is the longest slug slugPattern accepts.
const maxSlugLength = 40

// reviewJSON is the "review" field of a review app's JSON.
type reviewJSON struct {
	Parent         string `json:"parent"`
	PRNumber       int    `json:"pr_number"`
	PRTitle        string `json:"pr_title"`
	PRURL          string `json:"pr_url"`
	PRState        string `json:"pr_state"`
	HeadBranch     string `json:"head_branch"`
	DataCopy       string `json:"data_copy"`
	DataCopyDetail string `json:"data_copy_detail"`
	// DataCopyStatus is the status of the newest copy of the parent's
	// database into the review app, or "".
	DataCopyStatus string `json:"data_copy_status"`
}

// reviewSummaryJSON is the "review_apps" field of an app that is not a
// review app.
type reviewSummaryJSON struct {
	Count     int `json:"count"`
	Failed    int `json:"failed"`
	Deploying int `json:"deploying"`
}

// reviewToJSON describes a review app, or returns nil for other apps.
func (s *Server) reviewToJSON(ctx context.Context, app store.App) (*reviewJSON, error) {
	r, err := s.store.GetReviewApp(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	parent, err := s.store.GetAppByID(ctx, r.ParentAppID)
	if err != nil {
		return nil, err
	}
	copyStatus, err := s.store.LatestCopyStatus(ctx, app.ID, parent.ID)
	if err != nil {
		return nil, err
	}
	return &reviewJSON{
		Parent:         parent.Slug,
		PRNumber:       r.PRNumber,
		PRTitle:        r.PRTitle,
		PRURL:          "https://github.com/" + app.RepoFullName + "/pull/" + strconv.Itoa(r.PRNumber),
		PRState:        r.PRState,
		HeadBranch:     r.HeadBranch,
		DataCopy:       r.DataCopy,
		DataCopyDetail: r.DataCopyDetail,
		DataCopyStatus: copyStatus,
	}, nil
}

// reviewParentProblem says why an app cannot have review apps, or "".
func (s *Server) reviewParentProblem(ctx context.Context, app store.App) (string, error) {
	if _, err := s.store.GetReviewApp(ctx, app.ID); err == nil {
		return "review_app", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	switch {
	case app.Environment != store.EnvironmentStaging:
		return "not_staging", nil
	case !app.UsesGitHubApp():
		return "no_github_app", nil
	}
	return "", nil
}

// loadReviewParent loads {slug} as the parent of review apps, writing the
// refusal when it cannot be one.
func (s *Server) loadReviewParent(w http.ResponseWriter, r *http.Request) (store.App, bool) {
	parent, ok := s.loadApp(w, r)
	if !ok {
		return store.App{}, false
	}
	reason, err := s.reviewParentProblem(r.Context(), parent)
	if err != nil {
		s.internalError(w, r, err)
		return store.App{}, false
	}
	if reason != "" {
		writeJSON(w, http.StatusUnprocessableEntity, errorBody{Error: errorDetail{
			Code:    "not_review_parent",
			Message: "review apps need a staging app that is fetched through the GitHub App and is not a review app itself",
			Details: map[string]any{"reason": reason},
		}})
		return store.App{}, false
	}
	return parent, true
}

// writePullRequestError maps the errors of reading pull requests.
func (s *Server) writePullRequestError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, github.ErrUnauthorized):
		writeError(w, http.StatusBadGateway, "github_permission_missing",
			"GitHub refused to list pull requests: give the GitHub App the Pull requests: Read-only permission and accept it on the installation")
	case errors.Is(err, github.ErrNotFound):
		writeError(w, http.StatusNotFound, "pull_request_not_found", "the pull request was not found")
	default:
		s.writeGitHubError(w, r, err)
	}
}

func (s *Server) pullRequestsClient(w http.ResponseWriter) (PullRequests, bool) {
	if s.pulls == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "the GitHub App is not configured")
		return nil, false
	}
	return s.pulls, true
}

type pullRequestJSON struct {
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	HeadBranch string    `json:"head_branch"`
	HeadSHA    string    `json:"head_sha"`
	Author     string    `json:"author"`
	URL        string    `json:"url"`
	UpdatedAt  time.Time `json:"updated_at"`
	FromFork   bool      `json:"from_fork"`
	// ReviewApp is the slug of the pull request's review app, or null.
	ReviewApp *string `json:"review_app"`
}

func (s *Server) handleListPullRequests(w http.ResponseWriter, r *http.Request) {
	parent, ok := s.loadReviewParent(w, r)
	if !ok {
		return
	}
	pulls, ok := s.pullRequestsClient(w)
	if !ok {
		return
	}
	owner, repo, _ := strings.Cut(parent.RepoFullName, "/")
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()
	prs, err := pulls.ListPullRequests(ctx, owner, repo)
	if err != nil {
		s.writePullRequestError(w, r, err)
		return
	}
	existing, err := s.reviewSlugsByPR(r.Context(), parent.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]pullRequestJSON, 0, len(prs))
	for _, pr := range prs {
		j := pullRequestJSON{
			Number: pr.Number, Title: pr.Title, HeadBranch: pr.HeadBranch, HeadSHA: pr.HeadSHA,
			Author: pr.Author, URL: pr.HTMLURL, UpdatedAt: pr.UpdatedAt, FromFork: pr.FromFork,
		}
		if slug, ok := existing[pr.Number]; ok {
			j.ReviewApp = &slug
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"pull_requests": out})
}

// reviewSlugsByPR maps the pull requests of a parent's review apps to their
// slugs.
func (s *Server) reviewSlugsByPR(ctx context.Context, parentID int64) (map[int]string, error) {
	reviews, err := s.store.ListReviewApps(ctx, parentID)
	if err != nil {
		return nil, err
	}
	out := make(map[int]string, len(reviews))
	for _, rv := range reviews {
		app, err := s.store.GetAppByID(ctx, rv.AppID)
		if err != nil {
			return nil, err
		}
		out[rv.PRNumber] = app.Slug
	}
	return out, nil
}

func (s *Server) handleListReviewApps(w http.ResponseWriter, r *http.Request) {
	parent, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	reviews, err := s.store.ListReviewApps(r.Context(), parent.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := make([]appJSON, 0, len(reviews))
	for _, rv := range reviews {
		app, err := s.store.GetAppByID(r.Context(), rv.AppID)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		j, err := s.appToJSON(r, app)
		if err != nil {
			s.internalError(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"review_apps": out})
}

// createdReviewJSON is the answer to creating a review app.
type createdReviewJSON struct {
	appJSON
	// DNSWarning is set when the review app's hostname does not resolve to
	// the host yet (no wildcard record).
	DNSWarning bool `json:"dns_warning"`
	// DeployError is set when the first deployment could not be queued;
	// the app was created all the same.
	DeployError string `json:"deploy_error,omitempty"`
}

func (s *Server) handleCreateReviewApp(w http.ResponseWriter, r *http.Request) {
	parent, ok := s.loadReviewParent(w, r)
	if !ok {
		return
	}
	if !parent.ReviewAppsAllowed {
		// The owner decides which apps hand their variables and data to
		// pull requests.
		writeError(w, http.StatusConflict, "review_apps_not_allowed",
			"the owner has not allowed review apps of this app")
		return
	}
	var in struct {
		PRNumber int   `json:"pr_number"`
		CopyData *bool `json:"copy_data"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.PRNumber < 1 {
		writeValidationError(w, fieldErrors{"pr_number": "is required"})
		return
	}
	pulls, ok := s.pullRequestsClient(w)
	if !ok {
		return
	}
	existing, err := s.reviewSlugsByPR(r.Context(), parent.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if _, taken := existing[in.PRNumber]; taken {
		writeError(w, http.StatusConflict, "review_app_exists", "the pull request already has a review app")
		return
	}
	// The variables and the data of the parent are copied (ARCHITECTURE
	// 10.3).
	if !s.requireRecentAuth(w, r) {
		return
	}
	owner, repo, _ := strings.Cut(parent.RepoFullName, "/")
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()
	pr, err := pulls.GetPullRequest(ctx, owner, repo, in.PRNumber)
	if err != nil {
		s.writePullRequestError(w, r, err)
		return
	}
	switch {
	case pr.State != store.PRStateOpen:
		writeError(w, http.StatusUnprocessableEntity, "pull_request_not_open", "the pull request is not open")
		return
	case pr.FromFork:
		// The parent's variables and data would reach the fork's code.
		writeError(w, http.StatusUnprocessableEntity, "pull_request_from_fork", "pull requests from forks cannot have review apps")
		return
	case !branchPattern.MatchString(pr.HeadBranch) || strings.Contains(pr.HeadBranch, "..") || len(pr.HeadBranch) > 255:
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "the head branch of the pull request has an unsupported name")
		return
	}

	app := s.reviewAppSettings(parent, pr)
	review := store.ReviewApp{ParentAppID: parent.ID, PRNumber: pr.Number, PRTitle: pr.Title,
		HeadBranch: pr.HeadBranch, PRState: store.PRStateOpen}
	if in.CopyData == nil || *in.CopyData {
		switch {
		case parent.DumpHookService == "" || parent.DumpHookCommand == "":
			review.DataCopy, review.DataCopyDetail = store.DataCopySkipped, "no_dump_hook"
		case parent.LoadHookService == "" || parent.LoadHookCommand == "":
			review.DataCopy, review.DataCopyDetail = store.DataCopySkipped, "no_load_hook"
		default:
			review.DataCopy = store.DataCopyWaiting
		}
	}
	env, err := s.store.ListEnvVars(r.Context(), parent.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	registry, err := s.store.GetRegistryCredential(r.Context(), parent.ID)
	hasRegistry := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, err)
		return
	}
	in2 := store.NewReviewApp{
		Review: review,
		// The values are sealed for the new app's ID (ARCHITECTURE 6.1, 10).
		Env: func(appID int64) ([]store.EnvVar, error) {
			out := make([]store.EnvVar, 0, len(env))
			for _, v := range env {
				plain, err := s.box.Open(v.ValueEncrypted, secrets.EnvVarContext(parent.ID, v.Key))
				if err != nil {
					return nil, fmt.Errorf("environment variable %s: %w", v.Key, err)
				}
				sealed, err := s.box.Seal(plain, secrets.EnvVarContext(appID, v.Key))
				if err != nil {
					return nil, err
				}
				out = append(out, store.EnvVar{Key: v.Key, ValueEncrypted: sealed})
			}
			return out, nil
		},
		Registry: func(appID int64) (*store.RegistryCredential, error) {
			if !hasRegistry {
				return nil, nil
			}
			plain, err := s.box.Open(registry.TokenEncrypted, secrets.RegistryTokenContext(parent.ID))
			if err != nil {
				return nil, fmt.Errorf("registry token: %w", err)
			}
			sealed, err := s.box.Seal(plain, secrets.RegistryTokenContext(appID))
			if err != nil {
				return nil, err
			}
			return &store.RegistryCredential{Registry: registry.Registry, Username: registry.Username, TokenEncrypted: sealed}, nil
		},
	}
	var created store.App
	for _, slug := range reviewSlugs(parent.Slug, pr.Number) {
		if s.validateSlug(slug) != "" {
			continue
		}
		in2.App = app
		in2.App.Slug = slug
		created, err = s.store.CreateReviewApp(r.Context(), in2, s.cfg.Deploy.PortRangeStart, s.cfg.Deploy.PortRangeEnd, s.clock.Now())
		if !errors.Is(err, store.ErrConflict) {
			break
		}
	}
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "slug_taken", "no free slug for the review app")
		return
	case errors.Is(err, store.ErrReviewAppExists):
		writeError(w, http.StatusConflict, "review_app_exists", "the pull request already has a review app")
		return
	case errors.Is(err, store.ErrNoFreePort):
		writeError(w, http.StatusConflict, "no_free_port", "the host port range is exhausted")
		return
	case err != nil:
		s.internalError(w, r, err)
		return
	}
	envKeys := make([]string, 0, len(env))
	for _, v := range env {
		envKeys = append(envKeys, v.Key)
	}
	s.recordActivity(r, created.ID, actAppCreated, map[string]any{
		"environment": created.Environment, "env_added": envKeys,
		"review": map[string]any{"parent": parent.Slug, "pr_number": pr.Number},
	})
	s.recordActivity(r, parent.ID, actReviewAppCreated, map[string]any{"slug": created.Slug, "pr_number": pr.Number})
	s.syncRoutes(r, "review app created")

	out := createdReviewJSON{}
	if s.deployer != nil {
		message := pr.Title
		if s.refs != nil {
			if _, msg, err := s.refs.ResolveRef(ctx, created, pr.HeadSHA); err == nil && msg != "" {
				message = msg
			}
		}
		if _, err := s.deployer.Enqueue(r.Context(), created, pr.HeadBranch, pr.HeadSHA, message); err != nil {
			// The app stays; the user deploys it again from its page.
			s.log.Warn("queueing the first deployment of a review app failed", "app", created.Slug, "error", err)
			out.DeployError = deployRefusal(err)
		}
	}
	if s.reviewWatcher != nil {
		s.reviewWatcher.Kick()
	}
	if s.hostVerifier != nil {
		verifyCtx, cancelVerify := context.WithTimeout(r.Context(), 10*time.Second)
		if err := s.hostVerifier.Verify(verifyCtx, created.Slug+"."+s.cfg.BaseDomain); err != nil {
			out.DNSWarning = true
		}
		cancelVerify()
	}
	j, err := s.appToJSON(r, created)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out.appJSON = j
	writeJSON(w, http.StatusCreated, out)
}

// reviewAppSettings derives a review app from its parent (the table of
// ARCHITECTURE 9.9). The slug is set by the caller.
func (s *Server) reviewAppSettings(parent store.App, pr github.PullRequest) store.App {
	return store.App{
		RepoID:                parent.RepoID,
		RepoFullName:          parent.RepoFullName,
		DefaultBranch:         pr.HeadBranch,
		ComposePath:           parent.ComposePath,
		ComposeSource:         parent.ComposeSource,
		ComposeContent:        parent.ComposeContent,
		WebService:            parent.WebService,
		WebPort:               parent.WebPort,
		HealthcheckPath:       parent.HealthcheckPath,
		Environment:           store.EnvironmentStaging,
		RelaxPolicy:           parent.RelaxPolicy,
		MemoryLimit:           parent.MemoryLimit,
		ServiceMemoryLimits:   parent.ServiceMemoryLimits,
		BackupExcludeVolumes:  parent.BackupExcludeVolumes,
		DumpHookService:       parent.DumpHookService,
		DumpHookCommand:       parent.DumpHookCommand,
		LoadHookService:       parent.LoadHookService,
		LoadHookCommand:       parent.LoadHookCommand,
		PostLoadService:       parent.PostLoadService,
		PostLoadCommand:       parent.PostLoadCommand,
		EnvSource:             parent.EnvSource,
		EnvExamplePath:        parent.EnvExamplePath,
		AccessProtection:      true,
		ProtectionExemptPaths: parent.ProtectionExemptPaths,
		AutoDeploy:            true,
		AutoDeployBranch:      pr.HeadBranch,
		AutoDeployWorkflow:    parent.AutoDeployWorkflow,
		IdleSleep:             s.idleSleepAvailable(),
		IdleSleepMinutes:      parent.IdleSleepMinutes,
	}
}

// reviewSlugs lists the slugs a review app may get, in order:
// <parent>-pr-<number>, the parent part cut to fit, then -2 to -9.
func reviewSlugs(parent string, number int) []string {
	suffix := "-pr-" + strconv.Itoa(number)
	base := parent
	if len(base)+len(suffix) > maxSlugLength {
		base = strings.TrimRight(base[:max(0, maxSlugLength-len(suffix))], "-")
	}
	out := []string{base + suffix}
	for i := 2; i <= 9; i++ {
		extra := "-" + strconv.Itoa(i)
		b := base
		if len(b)+len(suffix)+len(extra) > maxSlugLength {
			b = strings.TrimRight(b[:max(0, maxSlugLength-len(suffix)-len(extra))], "-")
		}
		out = append(out, b+suffix+extra)
	}
	return out
}

// deployRefusal words why a deployment could not be queued, for the
// creation answer.
func deployRefusal(err error) string {
	var diskFull *deploy.DiskFullError
	if errors.As(err, &diskFull) {
		return diskFull.Error()
	}
	return "the deployment could not be queued"
}
