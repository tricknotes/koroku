package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"unicode/utf8"
)

// Deployment states GitHub accepts for a deployment status.
const (
	DeploymentQueued     = "queued"
	DeploymentInProgress = "in_progress"
	DeploymentSuccess    = "success"
	DeploymentFailure    = "failure"
	DeploymentError      = "error"
	// DeploymentInactive retires a deployment that succeeded: GitHub stops
	// showing it as the one that is live in its environment.
	DeploymentInactive = "inactive"
)

// maxDeploymentDescription is GitHub's limit on a deployment's or a status's
// description, in characters.
const maxDeploymentDescription = 140

// NewDeployment describes a deployment to record on GitHub.
type NewDeployment struct {
	// SHA is the full commit ID that is deployed.
	SHA string
	// Environment names the environment on GitHub (koroku uses the app slug).
	Environment string
	// Description is a short text; longer ones are cut to GitHub's limit.
	Description string
	// Production marks the environment as one that end users reach.
	Production bool
	// Transient marks an environment that will go away (a review app,
	// ARCHITECTURE 9.9).
	Transient bool
}

// DeploymentStatus is one status update of a GitHub deployment.
type DeploymentStatus struct {
	// State is one of the Deployment* constants.
	State string
	// Description is a short text; longer ones are cut to GitHub's limit.
	Description string
	// EnvironmentURL is where the deployed environment is reached. Optional.
	EnvironmentURL string
	// LogURL points at the deployment's output. Optional.
	LogURL string
}

// CreateDeployment records a deployment of a commit on GitHub and returns its
// ID. It authenticates with the repository's installation token, so the App
// needs the Deployments: write permission (403 otherwise, ErrUnauthorized).
//
// The deployment never merges branches (auto_merge is off) and is not held
// back by commit statuses (required_contexts is empty): it records what
// koroku already decided to deploy.
func (c *Client) CreateDeployment(ctx context.Context, owner, repo string, in NewDeployment) (int64, error) {
	if err := validateRepo(owner, repo); err != nil {
		return 0, err
	}
	if in.SHA == "" || in.Environment == "" {
		return 0, fmt.Errorf("%w: a deployment needs a commit and an environment", ErrInvalidArgument)
	}
	if c.anonymous() {
		return 0, ErrNotConfigured
	}
	tok, err := c.RepoToken(ctx, owner, repo)
	if err != nil {
		return 0, err
	}
	var resp struct {
		ID      int64  `json:"id"`
		Message string `json:"message"`
	}
	_, err = c.do(ctx, call{
		method: http.MethodPost,
		path:   repoPath(owner, repo) + "/deployments",
		bearer: tok.Value(),
		body: map[string]any{
			"ref":                    in.SHA,
			"task":                   "deploy",
			"auto_merge":             false,
			"required_contexts":      []string{},
			"environment":            in.Environment,
			"description":            truncateDescription(in.Description),
			"transient_environment":  in.Transient,
			"production_environment": in.Production,
		},
	}, &resp)
	if err != nil {
		return 0, fmt.Errorf("github: create deployment in %s/%s: %w", owner, repo, err)
	}
	if resp.ID <= 0 {
		// 202 answers a request that merged a branch instead; with
		// auto_merge off it should not happen.
		return 0, fmt.Errorf("github: create deployment in %s/%s: %w", owner, repo,
			errors.New("response has no deployment id"))
	}
	return resp.ID, nil
}

// CreateDeploymentStatus adds a status to a deployment of the repository.
//
// auto_inactive asks GitHub to retire the older deployments of the
// environment when one succeeds, but GitHub leaves production and transient
// environments alone and does not retire reliably elsewhere, so the caller
// retires them itself (LiveDeployments, DeploymentInactive).
func (c *Client) CreateDeploymentStatus(ctx context.Context, owner, repo string, id int64, in DeploymentStatus) error {
	if err := validateRepo(owner, repo); err != nil {
		return err
	}
	if id <= 0 {
		return fmt.Errorf("%w: deployment id %d", ErrInvalidArgument, id)
	}
	switch in.State {
	case DeploymentQueued, DeploymentInProgress, DeploymentSuccess, DeploymentFailure, DeploymentError,
		DeploymentInactive:
	default:
		return fmt.Errorf("%w: deployment state %q", ErrInvalidArgument, in.State)
	}
	if c.anonymous() {
		return ErrNotConfigured
	}
	tok, err := c.RepoToken(ctx, owner, repo)
	if err != nil {
		return err
	}
	body := map[string]any{
		"state":         in.State,
		"description":   truncateDescription(in.Description),
		"auto_inactive": true,
	}
	if in.EnvironmentURL != "" {
		body["environment_url"] = in.EnvironmentURL
	}
	if in.LogURL != "" {
		body["log_url"] = in.LogURL
	}
	_, err = c.do(ctx, call{
		method: http.MethodPost,
		path:   repoPath(owner, repo) + "/deployments/" + strconv.FormatInt(id, 10) + "/statuses",
		bearer: tok.Value(),
		body:   body,
	}, nil)
	if err != nil {
		return fmt.Errorf("github: update deployment %d in %s/%s: %w", id, owner, repo, err)
	}
	return nil
}

// maxLiveDeployments is how many of an environment's newest deployments
// LiveDeployments looks at (one page of the GraphQL API).
const maxLiveDeployments = 100

// liveDeploymentsQuery reads the latest status of an environment's newest
// deployments in one request. The REST API lists deployments without their
// status, which would cost one more request per deployment.
const liveDeploymentsQuery = `query($owner: String!, $name: String!, $environments: [String!], $first: Int!) {
  repository(owner: $owner, name: $name) {
    deployments(environments: $environments, first: $first, orderBy: {field: CREATED_AT, direction: DESC}) {
      nodes { databaseId latestStatus { state } }
    }
  }
}`

// LiveDeployments returns the IDs of the deployments GitHub shows as live in
// an environment: those whose latest status is success. It looks at the
// environment's newest maxLiveDeployments deployments, whoever created them,
// and returns them newest first. The App needs the Deployments permission.
func (c *Client) LiveDeployments(ctx context.Context, owner, repo, environment string) ([]int64, error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	if environment == "" {
		return nil, fmt.Errorf("%w: an environment is required", ErrInvalidArgument)
	}
	if c.anonymous() {
		return nil, ErrNotConfigured
	}
	tok, err := c.RepoToken(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Repository *struct {
				Deployments struct {
					Nodes []struct {
						DatabaseID   int64 `json:"databaseId"`
						LatestStatus *struct {
							State string `json:"state"`
						} `json:"latestStatus"`
					} `json:"nodes"`
				} `json:"deployments"`
			} `json:"repository"`
		} `json:"data"`
		Errors []graphQLError `json:"errors"`
	}
	_, err = c.do(ctx, call{
		method: http.MethodPost,
		path:   "/graphql",
		bearer: tok.Value(),
		body: map[string]any{
			"query": liveDeploymentsQuery,
			"variables": map[string]any{
				"owner": owner, "name": repo,
				"environments": []string{environment},
				"first":        maxLiveDeployments,
			},
		},
	}, &resp)
	if err == nil && len(resp.Errors) > 0 {
		err = resp.Errors[0].apiError([]string{tok.Value()})
	}
	if err == nil && resp.Data.Repository == nil {
		err = ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("github: list live deployments of %s in %s/%s: %w", environment, owner, repo, err)
	}
	var ids []int64
	for _, n := range resp.Data.Repository.Deployments.Nodes {
		if n.DatabaseID > 0 && n.LatestStatus != nil && n.LatestStatus.State == "SUCCESS" {
			ids = append(ids, n.DatabaseID)
		}
	}
	return ids, nil
}

// truncateDescription cuts s to GitHub's limit without splitting a character.
func truncateDescription(s string) string {
	if utf8.RuneCountInString(s) <= maxDeploymentDescription {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxDeploymentDescription-3]) + "..."
}
