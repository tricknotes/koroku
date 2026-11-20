package api

import (
	"context"
	"errors"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/envreq"
	"github.com/tricknotes/koroku/internal/github"
)

const githubTimeout = 30 * time.Second

// githubClient returns the client or writes 503 when GitHub is not configured.
func (s *Server) githubClient(w http.ResponseWriter) (GitHub, bool) {
	if s.auth.GitHub == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "the GitHub App is not configured")
		return nil, false
	}
	return s.auth.GitHub, true
}

// writeGitHubError maps client errors to API errors without passing
// upstream messages through.
func (s *Server) writeGitHubError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, github.ErrNotFound):
		writeError(w, http.StatusNotFound, "repo_not_found", "the repository, ref or file was not found (is the GitHub App installed on it?)")
	case errors.Is(err, github.ErrInvalidArgument):
		writeError(w, http.StatusUnprocessableEntity, "validation_failed", "invalid repository, ref or path")
	case errors.Is(err, github.ErrRateLimited):
		writeError(w, http.StatusServiceUnavailable, "github_rate_limited", "GitHub is rate limiting requests; try again later")
	case errors.Is(err, github.ErrTooLarge):
		writeError(w, http.StatusUnprocessableEntity, "file_too_large", "the file is too large to read")
	default:
		s.log.Error("github request failed", "path", r.URL.Path, "error", err)
		writeError(w, http.StatusBadGateway, "github_error", "the request to GitHub failed")
	}
}

func (s *Server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	gh, ok := s.githubClient(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()
	repos, err := gh.ListRepos(ctx)
	if err != nil {
		s.writeGitHubError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(repos))
	for _, repo := range repos {
		out = append(out, map[string]any{
			"id":             repo.ID,
			"full_name":      repo.FullName,
			"private":        repo.Private,
			"default_branch": repo.DefaultBranch,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": out})
}

func (s *Server) handleListBranches(w http.ResponseWriter, r *http.Request) {
	gh, ok := s.githubClient(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()
	branches, err := gh.ListBranches(ctx, r.PathValue("owner"), r.PathValue("repo"))
	if err != nil {
		s.writeGitHubError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(branches))
	for _, b := range branches {
		out = append(out, map[string]any{"name": b.Name, "sha": b.SHA})
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": out})
}

func (s *Server) handleListCommits(w http.ResponseWriter, r *http.Request) {
	gh, ok := s.githubClient(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()
	commits, err := gh.ListCommits(ctx, r.PathValue("owner"), r.PathValue("repo"), r.URL.Query().Get("ref"), 30)
	if err != nil {
		s.writeGitHubError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(commits))
	for _, c := range commits {
		// The subject line is enough for a picker.
		subject, _, _ := strings.Cut(c.Message, "\n")
		out = append(out, map[string]any{"sha": c.SHA, "message": subject, "author": c.Author, "date": c.Date})
	}
	writeJSON(w, http.StatusOK, map[string]any{"commits": out})
}

// handleDetectCompose finds Compose files in the repository root and reads
// service names and ports from one of them to prefill the create-app form.
func (s *Server) handleDetectCompose(w http.ResponseWriter, r *http.Request) {
	owner, repo := r.PathValue("owner"), r.PathValue("repo")
	ref := r.URL.Query().Get("ref")
	requested := strings.TrimSpace(r.URL.Query().Get("path"))
	if requested != "" {
		clean := path.Clean(requested)
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
			writeValidationError(w, fieldErrors{"path": "must be a path inside the repository"})
			return
		}
		requested = clean
	}
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()

	// Through the App when it can see the repository; a public repository
	// it cannot see (not installed, or installed on an account koroku does
	// not use) is read anonymously.
	var reader envreq.Files
	var names []string
	var err error
	if s.auth.GitHub != nil {
		reader = s.auth.GitHub
		names, err = reader.ListDir(ctx, owner, repo, "", ref)
	}
	if (reader == nil || errors.Is(err, github.ErrNotFound)) && s.publicFiles != nil {
		reader = s.publicFiles
		names, err = reader.ListDir(ctx, owner, repo, "", ref)
	}
	if reader == nil {
		writeError(w, http.StatusServiceUnavailable, "not_configured", "the GitHub App is not configured")
		return
	}
	if err != nil {
		s.writeGitHubError(w, r, err)
		return
	}
	files := []string{}
	for _, candidate := range compose.DefaultFiles() {
		if slices.Contains(names, candidate) {
			files = append(files, candidate)
		}
	}
	chosen := requested
	if chosen == "" && len(files) > 0 {
		chosen = files[0]
	}
	services := []map[string]any{}
	if chosen != "" {
		content, err := reader.ReadFile(ctx, owner, repo, chosen, ref)
		if err != nil {
			s.writeGitHubError(w, r, err)
			return
		}
		for _, hint := range compose.ScanServices(content) {
			ports := hint.Ports
			if ports == nil {
				ports = []int{}
			}
			services = append(services, map[string]any{"name": hint.Name, "ports": ports})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "path": chosen, "services": services})
}
