package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/envreq"
	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/store"
)

// envRequirementsTimeout bounds every GitHub call of one request.
const envRequirementsTimeout = 15 * time.Second

type envRequirementJSON struct {
	Key         string  `json:"key"`
	Required    bool    `json:"required"`
	Default     *string `json:"default"`
	Description string  `json:"description"`
	Set         bool    `json:"set"`
}

type envRequirementsJSON struct {
	Source    string               `json:"source"`
	Status    string               `json:"status"`
	Reason    string               `json:"reason"`
	Ref       string               `json:"ref"`
	CommitSHA string               `json:"commit_sha"`
	Path      string               `json:"path"`
	Variables []envRequirementJSON `json:"variables"`
	Unlisted  []string             `json:"unlisted"`
}

// handleEnvRequirements compares the variables the repository needs at a
// commit with the saved ones (ARCHITECTURE 9.4). Only keys are involved,
// so it answers without a recent login. Failures to read the repository
// are part of a 200 response so that the console keeps working.
func (s *Server) handleEnvRequirements(w http.ResponseWriter, r *http.Request) {
	app, ok := s.loadApp(w, r)
	if !ok {
		return
	}
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	sha := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sha")))
	errs := fieldErrors{}
	if sha != "" && !gitrepo.IsFullSHA(sha) {
		errs.add("sha", "must be a full commit SHA")
	}
	if ref != "" {
		if err := gitrepo.ValidateRef(ref); err != nil {
			errs.add("ref", "is not a valid branch or tag name")
		}
	}
	if len(errs) > 0 {
		writeValidationError(w, errs)
		return
	}
	if app.EnvSource == envreq.SourceCompose && app.ComposeInline() {
		// The file written in koroku is the same for every commit
		// (ARCHITECTURE 6.3).
		ref, sha = "", ""
	} else if sha == "" && ref == "" {
		ref = app.DefaultBranch
	}

	out := envRequirementsJSON{Source: app.EnvSource, Ref: ref, CommitSHA: sha}
	ctx, cancel := context.WithTimeout(r.Context(), envRequirementsTimeout)
	defer cancel()
	result := s.readEnvRequirements(ctx, app, &out)

	rows, err := s.store.ListEnvVars(r.Context(), app.ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	saved := make([]string, 0, len(rows))
	for _, row := range rows {
		saved = append(saved, row.Key)
	}
	out.Status, out.Reason, out.Path = result.Status, result.Reason, result.Path
	out.Variables = []envRequirementJSON{}
	out.Unlisted = []string{}
	if result.Status == envreq.StatusOK {
		set, unlisted := envreq.Compare(result.Variables, saved)
		for _, v := range result.Variables {
			out.Variables = append(out.Variables, envRequirementJSON{
				Key: v.Key, Required: v.Required, Default: v.Default, Description: v.Description, Set: set[v.Key],
			})
		}
		out.Unlisted = unlisted
	}
	writeJSON(w, http.StatusOK, out)
}

// readEnvRequirements resolves the commit (filling out.CommitSHA) and
// reads what the repository asks for there.
func (s *Server) readEnvRequirements(ctx context.Context, app store.App, out *envRequirementsJSON) envreq.Result {
	if app.EnvSource == envreq.SourceOff {
		return envreq.Result{Status: envreq.StatusOff}
	}
	if app.EnvSource == envreq.SourceCompose && app.ComposeInline() {
		// The file is in the database: nothing is read from the
		// repository, and no commit decides it (ARCHITECTURE 6.3).
		return envreq.FromCompose(compose.InlineFileName, []byte(app.ComposeContent))
	}
	owner, repo, ok := githubRepoOf(app)
	if !ok {
		return envreq.Unavailable(envreq.ReasonUnsupportedRepo, "")
	}
	if out.CommitSHA == "" {
		if s.refs == nil {
			return envreq.Unavailable(envreq.ReasonRefNotFound, "")
		}
		sha, _, err := s.refs.ResolveRef(ctx, app, out.Ref)
		if err != nil {
			s.log.Warn("resolving ref for env requirements failed", "app", app.Slug, "error", err)
			return envreq.Unavailable(envreq.ReasonRefNotFound, "")
		}
		out.CommitSHA = sha
	}
	result := s.envReqs.Read(ctx, envreq.Request{
		Owner: owner, Repo: repo, SHA: out.CommitSHA, Source: app.EnvSource,
		ComposePath: app.ComposePath, ExamplePath: app.EnvExamplePath,
	})
	if result.Reason == envreq.ReasonGitHubError {
		s.log.Warn("reading env requirements failed", "app", app.Slug)
	}
	return result
}

// githubRepoOf names the github.com repository an app is fetched from: its
// full name, or a repo_url of the form https://github.com/<owner>/<repo>
// (with or without .git). Other URLs cannot be read through the API.
func githubRepoOf(app store.App) (owner, repo string, ok bool) {
	fullName := app.RepoFullName
	if app.RepoURL != "" {
		u, err := url.Parse(app.RepoURL)
		if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", "", false
		}
		fullName = strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	}
	if fullName == "" {
		return "", "", false
	}
	owner, repo, err := github.SplitFullName(fullName)
	if err != nil {
		return "", "", false
	}
	return owner, repo, true
}
