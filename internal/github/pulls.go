package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// maxPullRequests caps how many open pull requests ListPullRequests reads.
const maxPullRequests = 100

// PullRequest is one pull request of a repository (ARCHITECTURE 9.9).
type PullRequest struct {
	Number int
	Title  string
	// State is "open", "closed" or "merged" (a closed one that was merged).
	State string
	// HeadBranch and HeadSHA are the branch and commit the pull request
	// proposes.
	HeadBranch string
	HeadSHA    string
	// FromFork is set when the head branch is not in the repository itself
	// (a fork, or a fork that was deleted).
	FromFork  bool
	Author    string
	HTMLURL   string
	UpdatedAt time.Time
}

type apiPullRequest struct {
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	State    string     `json:"state"`
	MergedAt *time.Time `json:"merged_at"`
	HTMLURL  string     `json:"html_url"`
	User     struct {
		Login string `json:"login"`
	} `json:"user"`
	UpdatedAt time.Time `json:"updated_at"`
	Head      struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Repo *struct {
			ID int64 `json:"id"`
		} `json:"repo"`
	} `json:"base"`
}

func (p apiPullRequest) pullRequest() PullRequest {
	state := p.State
	if state == "closed" && p.MergedAt != nil {
		state = "merged"
	}
	fork := p.Head.Repo == nil || p.Base.Repo == nil || p.Head.Repo.ID != p.Base.Repo.ID
	return PullRequest{
		Number: p.Number, Title: p.Title, State: state,
		HeadBranch: p.Head.Ref, HeadSHA: p.Head.SHA, FromFork: fork,
		Author: p.User.Login, HTMLURL: p.HTMLURL, UpdatedAt: p.UpdatedAt,
	}
}

// ListPullRequests returns the open pull requests of a repository, most
// recently updated first. It needs the App's pull_requests: read
// permission; without it GitHub answers 403 (ErrUnauthorized).
func (c *Client) ListPullRequests(ctx context.Context, owner, repo string) ([]PullRequest, error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	in, err := c.repoCall(ctx, owner, repo, "/pulls", url.Values{
		"state": {"open"}, "sort": {"updated"}, "direction": {"desc"},
	})
	if err != nil {
		return nil, err
	}
	items, err := collect(ctx, c, in, maxPullRequests, func(p *[]apiPullRequest) []apiPullRequest { return *p })
	if err != nil {
		return nil, fmt.Errorf("github: list pull requests of %s/%s: %w", owner, repo, err)
	}
	out := make([]PullRequest, len(items))
	for i, item := range items {
		out[i] = item.pullRequest()
	}
	return out, nil
}

// GetPullRequest returns one pull request of a repository.
func (c *Client) GetPullRequest(ctx context.Context, owner, repo string, number int) (PullRequest, error) {
	if err := validateRepo(owner, repo); err != nil {
		return PullRequest{}, err
	}
	if number < 1 {
		return PullRequest{}, fmt.Errorf("%w: pull request number %d", ErrInvalidArgument, number)
	}
	in, err := c.repoCall(ctx, owner, repo, "/pulls/"+strconv.Itoa(number), nil)
	if err != nil {
		return PullRequest{}, err
	}
	var item apiPullRequest
	if _, err := c.do(ctx, in, &item); err != nil {
		return PullRequest{}, fmt.Errorf("github: get pull request %d of %s/%s: %w", number, owner, repo, err)
	}
	return item.pullRequest(), nil
}
