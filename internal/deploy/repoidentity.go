package deploy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/store"
)

// repoLookupTimeout bounds the request that reads the repository's ID.
const repoLookupTimeout = 30 * time.Second

// confirmRepository makes sure, before anything is fetched, that the name
// the app fetches by still denotes the repository the app was set up with
// (ARCHITECTURE 5). A name is not an identity: after a repository is
// deleted, or its account gives the name up, the same name can denote
// another repository, whose code would otherwise run with this app's
// variables and data. The repository's numeric ID does not change, so the
// app's repo_id is compared with the ID GitHub reports for the name.
//
// authenticated says whether the GitHub App has a token for the name. When
// it has none (the App is not installed on the repository, or only on an
// account koroku does not use), git fetches the repository without
// credentials, which works for a public one. That is exactly where a name
// lands that another account took over, so an app with a known ID is
// confirmed there too, by reading the public repository without
// credentials. An app without an ID is fetched unconfirmed in that case and
// records none: its repository was never one the App vouched for.
//
// The check covers apps fetched from GitHub by full name; apps with an
// explicit URL are fetched as they are.
func (e *Engine) confirmRepository(ctx context.Context, app store.App, authenticated bool, log *deployLog) error {
	if e.opts.Repos == nil {
		return nil
	}
	owner, repo, ok := githubRepo(app)
	if !ok {
		return nil
	}
	name := app.RepoFullName
	want, err := e.expectedRepoID(ctx, app)
	if err != nil {
		return err
	}
	if !authenticated && want == nil {
		log.Printf("The GitHub App cannot see %s: fetching it as a public repository, without confirming which repository has that name.", name)
		return nil
	}

	lookupCtx, cancel := context.WithTimeout(ctx, repoLookupTimeout)
	found, err := e.opts.Repos.Repository(lookupCtx, owner, repo, !authenticated)
	cancel()
	switch {
	case errors.Is(err, github.ErrNotFound) && want == nil:
		// Nothing to compare with and nothing to record. git reports what
		// is wrong with the repository.
		return nil
	case errors.Is(err, github.ErrNotFound) && !authenticated:
		return fmt.Errorf("fetch source: cannot confirm that %s is the repository this app was set up with (repository %d): "+
			"the GitHub App cannot see a repository of that name and no public one has it. Nothing was fetched", name, *want)
	case errors.Is(err, github.ErrNotFound):
		return fmt.Errorf("fetch source: cannot confirm that %s is the repository this app was set up with (repository %d): "+
			"GitHub shows koroku no repository of that name. Nothing was fetched", name, *want)
	case err != nil:
		// Like a failure to get the token: the deployment fails instead of
		// going on unchecked.
		return fmt.Errorf("fetch source: cannot confirm that %s is the repository this app was set up with: %w. "+
			"Nothing was fetched; deploy again once GitHub answers", name, err)
	}

	inherited := want != nil && app.RepoID == nil
	if want == nil {
		// Trust on first use: the first ID read for the name is recorded
		// and every later fetch is held to it.
		recorded, err := e.opts.Store.RecordAppRepoID(ctx, app.ID, found.ID)
		if err != nil {
			return err
		}
		if recorded == found.ID {
			log.Printf("Recorded %s as repository %d: from now on the app is fetched from this repository only.", name, found.ID)
			e.opts.Logger.Info("recorded the repository id of an app", "app", app.Slug, "repository", name, "repo_id", found.ID)
		}
		want = &recorded
	}
	if *want != found.ID {
		return fmt.Errorf("fetch source: %s is not the repository this app was set up with: "+
			"GitHub has repository %d under that name, the app was set up with repository %d. Nothing was fetched. "+
			"If the app should deploy the repository that has the name now, the owner sets the app's repository again (repo_id %d)",
			name, found.ID, *want, found.ID)
	}
	if inherited {
		// A review app that was held to its parent's ID keeps it.
		if _, err := e.opts.Store.RecordAppRepoID(ctx, app.ID, found.ID); err != nil {
			return err
		}
	}
	if authenticated {
		log.Printf("Confirmed that %s is repository %d, the one the app was set up with.", name, found.ID)
	} else {
		log.Printf("Confirmed that %s is repository %d, the one the app was set up with. The GitHub App cannot see it: fetching it as a public repository.", name, found.ID)
	}
	if found.FullName != "" && !strings.EqualFold(found.FullName, name) {
		// A renamed or transferred repository: GitHub redirects the old
		// name. The settings are the owner's to change.
		log.Printf("GitHub calls this repository %s now. The app's settings still name %s, which keeps working while GitHub redirects the old name.", found.FullName, name)
	}
	return nil
}

// expectedRepoID returns the repository ID the app is held to, or nil when
// none is known yet. A review app without its own takes the ID of its
// parent: it deploys the parent's repository, so it does not trust a first
// reading of its own while the parent's repository is known.
func (e *Engine) expectedRepoID(ctx context.Context, app store.App) (*int64, error) {
	if app.RepoID != nil {
		return app.RepoID, nil
	}
	review, err := e.opts.Store.GetReviewApp(ctx, app.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	parent, err := e.opts.Store.GetAppByID(ctx, review.ParentAppID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if parent.RepoID == nil || !parent.UsesGitHubApp() || !strings.EqualFold(parent.RepoFullName, app.RepoFullName) {
		return nil, nil
	}
	return parent.RepoID, nil
}
