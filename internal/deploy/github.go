package deploy

import (
	"context"
	"errors"

	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// GitHubAPI is the part of the GitHub App client the deploy engine needs.
type GitHubAPI interface {
	RepoToken(ctx context.Context, owner, repo string) (github.Token, error)
	ResolveCommit(ctx context.Context, owner, repo, ref string) (github.Commit, error)
}

// GitHubTokens mints installation tokens for apps backed by a GitHub
// repository. The token is valid for an hour and is only handed to git.
type GitHubTokens struct {
	Client GitHubAPI
}

// RepoToken implements TokenSource.
func (g GitHubTokens) RepoToken(ctx context.Context, app store.App) (string, error) {
	owner, repo, ok := githubRepo(app)
	if !ok {
		return "", nil
	}
	token, err := g.Client.RepoToken(ctx, owner, repo)
	if errors.Is(err, github.ErrNotFound) {
		// The App is not installed on this repository. It may still be a
		// public repository, so let git try without credentials.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return token.Value(), nil
}

// githubRepo returns the owner and name when the app is fetched from
// github.com by its full name (and not from an explicit URL).
func githubRepo(app store.App) (owner, repo string, ok bool) {
	if app.RepoURL != "" || app.RepoFullName == "" {
		return "", "", false
	}
	owner, repo, err := github.SplitFullName(app.RepoFullName)
	if err != nil {
		return "", "", false
	}
	return owner, repo, true
}

// GitHubRefResolver resolves refs through the GitHub API, which also yields
// the commit message. Apps with an explicit repository URL, and repositories
// the App cannot see, fall back to git ls-remote.
type GitHubRefResolver struct {
	Client   GitHubAPI
	Fallback GitRefResolver
}

// ResolveRef returns the commit a ref points at.
func (g GitHubRefResolver) ResolveRef(ctx context.Context, app store.App, ref string) (string, string, error) {
	owner, repo, ok := githubRepo(app)
	if !ok {
		return g.Fallback.ResolveRef(ctx, app, ref)
	}
	commit, err := g.Client.ResolveCommit(ctx, owner, repo, ref)
	if errors.Is(err, github.ErrNotFound) {
		return g.Fallback.ResolveRef(ctx, app, ref)
	}
	if err != nil {
		return "", "", err
	}
	return commit.SHA, firstLine(commit.Message), nil
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
