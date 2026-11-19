package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// defaultCommitLimit is used by ListCommits when the caller passes no limit.
const defaultCommitLimit = 30

// Repo is a repository the app is installed on.
type Repo struct {
	ID            int64
	FullName      string
	Owner         string
	Name          string
	Private       bool
	DefaultBranch string
	// InstallationID is the installation that grants access to the repository.
	InstallationID int64
}

// Branch is a branch and the commit it points at.
type Branch struct {
	Name string
	SHA  string
}

// Commit is one commit of a repository.
type Commit struct {
	// SHA is the full 40 character commit ID.
	SHA string
	// Message is the complete commit message.
	Message string
	// Author is the name recorded as the git author.
	Author string
	// Date is the commit date (the author date when GitHub reports no commit date).
	Date time.Time
}

// apiCommit is the part of GitHub's commit object this package reads.
type apiCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

func (a apiCommit) commit() Commit {
	date := a.Commit.Committer.Date
	if date.IsZero() {
		date = a.Commit.Author.Date
	}
	return Commit{SHA: a.SHA, Message: a.Commit.Message, Author: a.Commit.Author.Name, Date: date}
}

// repoCall prepares a GET on a path below /repos/{owner}/{repo}, authenticated
// with the repository's installation token, or unauthenticated on an
// anonymous client.
func (c *Client) repoCall(ctx context.Context, owner, repo, suffix string, query url.Values) (call, error) {
	if c.anonymous() {
		// Public repositories only; GitHub answers 404 for the rest.
		return call{method: http.MethodGet, path: repoPath(owner, repo) + suffix, query: query}, nil
	}
	tok, err := c.RepoToken(ctx, owner, repo)
	if err != nil {
		return call{}, err
	}
	return call{
		method: http.MethodGet,
		path:   repoPath(owner, repo) + suffix,
		query:  query,
		bearer: tok.Value(),
	}, nil
}

// ListRepos returns every repository of every active installation of the app,
// sorted by full name. At most maxPages pages of 100 are read per installation.
func (c *Client) ListRepos(ctx context.Context) ([]Repo, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return nil, fmt.Errorf("github: list installations: %w", err)
	}
	type installation struct {
		ID          int64      `json:"id"`
		SuspendedAt *time.Time `json:"suspended_at"`
	}
	installations, err := collect(ctx, c, call{
		method: http.MethodGet,
		path:   "/app/installations",
		bearer: jwt,
	}, 0, func(p *[]installation) []installation { return *p })
	if err != nil {
		return nil, fmt.Errorf("github: list installations: %w", err)
	}

	type apiRepo struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
		Owner    struct {
			Login string `json:"login"`
		} `json:"owner"`
		DefaultBranch string `json:"default_branch"`
	}
	type repoPage struct {
		Repositories []apiRepo `json:"repositories"`
	}

	var repos []Repo
	for _, inst := range installations {
		if inst.SuspendedAt != nil {
			// GitHub refuses to issue tokens for suspended installations.
			continue
		}
		tok, err := c.InstallationToken(ctx, inst.ID)
		if err != nil {
			return nil, err
		}
		page, err := collect(ctx, c, call{
			method: http.MethodGet,
			path:   "/installation/repositories",
			bearer: tok.Value(),
		}, 0, func(p *repoPage) []apiRepo { return p.Repositories })
		if err != nil {
			return nil, fmt.Errorf("github: list repositories of installation %d: %w", inst.ID, err)
		}
		for _, r := range page {
			repos = append(repos, Repo{
				ID:             r.ID,
				FullName:       r.FullName,
				Owner:          r.Owner.Login,
				Name:           r.Name,
				Private:        r.Private,
				DefaultBranch:  r.DefaultBranch,
				InstallationID: inst.ID,
			})
			c.rememberInstallation(r.Owner.Login, r.Name, inst.ID)
		}
	}
	sort.SliceStable(repos, func(i, j int) bool {
		return strings.ToLower(repos[i].FullName) < strings.ToLower(repos[j].FullName)
	})
	return repos, nil
}

// ListBranches returns the branches of a repository. At most maxPages pages of
// 100 are read.
func (c *Client) ListBranches(ctx context.Context, owner, repo string) ([]Branch, error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	in, err := c.repoCall(ctx, owner, repo, "/branches", nil)
	if err != nil {
		return nil, err
	}
	type apiBranch struct {
		Name   string `json:"name"`
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	items, err := collect(ctx, c, in, 0, func(p *[]apiBranch) []apiBranch { return *p })
	if err != nil {
		return nil, fmt.Errorf("github: list branches of %s/%s: %w", owner, repo, err)
	}
	branches := make([]Branch, len(items))
	for i, b := range items {
		branches[i] = Branch{Name: b.Name, SHA: b.Commit.SHA}
	}
	return branches, nil
}

// ListCommits returns the newest commits reachable from ref (a branch, tag or
// SHA; empty means the default branch). limit <= 0 means 30; the result never
// exceeds maxPages pages of 100.
func (c *Client) ListCommits(ctx context.Context, owner, repo, ref string, limit int) ([]Commit, error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	query := url.Values{}
	if ref != "" {
		if err := validateRef(ref); err != nil {
			return nil, err
		}
		query.Set("sha", ref)
	}
	if limit <= 0 {
		limit = defaultCommitLimit
	}
	in, err := c.repoCall(ctx, owner, repo, "/commits", query)
	if err != nil {
		return nil, err
	}
	items, err := collect(ctx, c, in, limit, func(p *[]apiCommit) []apiCommit { return *p })
	if err != nil {
		return nil, fmt.Errorf("github: list commits of %s/%s: %w", owner, repo, err)
	}
	commits := make([]Commit, len(items))
	for i, item := range items {
		commits[i] = item.commit()
	}
	return commits, nil
}

// ResolveCommit turns a branch name, tag or (abbreviated) SHA into the commit
// it denotes. An unknown ref yields ErrNotFound.
func (c *Client) ResolveCommit(ctx context.Context, owner, repo, ref string) (Commit, error) {
	if err := validateRepo(owner, repo); err != nil {
		return Commit{}, err
	}
	if err := validateRef(ref); err != nil {
		return Commit{}, err
	}
	// The ref is one path segment: a "/" in a branch name is sent as %2F.
	in, err := c.repoCall(ctx, owner, repo, "/commits/"+url.PathEscape(ref), nil)
	if err != nil {
		return Commit{}, err
	}
	var item apiCommit
	if _, err := c.do(ctx, in, &item); err != nil {
		// GitHub answers an unknown ref with 422 "No commit found for SHA".
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity {
			apiErr.notFound = true
		}
		return Commit{}, fmt.Errorf("github: resolve %q in %s/%s: %w", ref, owner, repo, err)
	}
	if item.SHA == "" {
		return Commit{}, fmt.Errorf("github: resolve %q in %s/%s: response has no commit sha", ref, owner, repo)
	}
	return item.commit(), nil
}
