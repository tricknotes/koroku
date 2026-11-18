package deploy

import (
	"context"
	"errors"

	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/store"
)

// GitRefResolver resolves branches and tags with `git ls-remote`. It works
// for any repository git can reach; the commit message is not known until
// the commit is checked out.
type GitRefResolver struct {
	Fetcher gitrepo.Fetcher
	// Tokens is optional; without it only public repositories resolve.
	Tokens TokenSource
}

// ResolveRef returns the commit SHA that ref points at.
func (g GitRefResolver) ResolveRef(ctx context.Context, app store.App, ref string) (string, string, error) {
	repoURL := app.CloneURL()
	if repoURL == "" {
		return "", "", errors.New("the app has no repository configured")
	}
	var auth *gitrepo.Auth
	if g.Tokens != nil {
		token, err := g.Tokens.RepoToken(ctx, app)
		if err != nil {
			return "", "", err
		}
		if token != "" {
			auth = &gitrepo.Auth{Token: token}
		}
	}
	sha, err := g.Fetcher.ResolveRef(ctx, repoURL, ref, auth)
	return sha, "", err
}
