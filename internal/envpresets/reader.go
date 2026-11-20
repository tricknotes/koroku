package envpresets

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/tricknotes/koroku/internal/github"
)

// Statuses and reasons of a Result. The reasons are the words of the
// variable comparison (ARCHITECTURE 9.4).
const (
	StatusOK          = "ok"
	StatusUnavailable = "unavailable"

	ReasonRepoUnavailable = "repo_unavailable"
	ReasonRateLimited     = "rate_limited"
	ReasonGitHubError     = "github_error"
	ReasonNotConfigured   = "not_configured"
)

// Files is the part of the GitHub client the reader needs.
type Files interface {
	ListDir(ctx context.Context, owner, repo, dir, ref string) ([]string, error)
	ReadFile(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}

// Request names what to read.
type Request struct {
	Owner, Repo string
	// Ref is a branch, tag or commit; empty means the default branch.
	Ref string
	// ComposePath is the Compose file the create screen chose, empty to
	// detect.
	ComposePath string
}

// Result is a Detection or the reason there is none.
type Result struct {
	Status    string
	Reason    string
	Detection Detection
}

// Reader reads repositories through the GitHub contents API: with the
// App's installation token when it can see the repository, otherwise
// without credentials (public repositories only). Branches move, so
// nothing is cached.
type Reader struct {
	app    Files
	public Files
}

// NewReader returns a Reader that reads through app (nil when the GitHub
// App is not configured) and falls back to public (nil to disable).
func NewReader(app, public Files) *Reader {
	return &Reader{app: app, public: public}
}

// Read detects the presets of req. Failures are reported in the Result.
func (r *Reader) Read(ctx context.Context, req Request) Result {
	if r.app == nil && r.public == nil {
		return Result{Status: StatusUnavailable, Reason: ReasonNotConfigured}
	}
	// As in envreq: the App first, and an anonymous request only when the
	// App cannot see the repository, to spare the hourly anonymous limit.
	var files Files
	var root []string
	err := github.ErrNotFound
	for _, candidate := range []Files{r.app, r.public} {
		if candidate == nil || !errors.Is(err, github.ErrNotFound) {
			continue
		}
		files = candidate
		root, err = files.ListDir(ctx, req.Owner, req.Repo, "", req.Ref)
	}
	if err != nil {
		return Result{Status: StatusUnavailable, Reason: reasonFor(err)}
	}
	read := func(name string) ([]byte, error) {
		content, err := files.ReadFile(ctx, req.Owner, req.Repo, name, req.Ref)
		if errors.Is(err, github.ErrNotFound) || errors.Is(err, github.ErrTooLarge) {
			return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
		return content, err
	}
	detection, err := Detect(root, read, req.ComposePath)
	if err != nil {
		return Result{Status: StatusUnavailable, Reason: reasonFor(err)}
	}
	return Result{Status: StatusOK, Detection: detection}
}

func reasonFor(err error) string {
	switch {
	case errors.Is(err, github.ErrNotFound):
		return ReasonRepoUnavailable
	case errors.Is(err, github.ErrRateLimited):
		return ReasonRateLimited
	default:
		return ReasonGitHubError
	}
}
