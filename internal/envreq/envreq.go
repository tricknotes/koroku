// Package envreq reads the environment variables an app's repository says
// it needs, from the Compose file's ${VAR} references or from a
// .env.example file, so the console can compare them with the saved
// variables (ARCHITECTURE 9.4). Only keys and descriptions are handled;
// values never pass through this package.
package envreq

import (
	"context"
	"errors"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/github"
)

// Sources an app can take its list of required variables from.
const (
	SourceCompose = "compose"
	SourceDotenv  = "dotenv"
	SourceOff     = "off"
)

// DefaultExamplePath is read in dotenv mode when the app sets no path.
const DefaultExamplePath = ".env.example"

// ValidSource reports whether s is a known source.
func ValidSource(s string) bool {
	return s == SourceCompose || s == SourceDotenv || s == SourceOff
}

// Statuses of a Result.
const (
	StatusOK          = "ok"
	StatusOff         = "off"
	StatusUnavailable = "unavailable"
)

// Reasons a Result is unavailable.
const (
	ReasonFileNotFound    = "file_not_found"
	ReasonRepoUnavailable = "repo_unavailable"
	ReasonUnsupportedRepo = "unsupported_repo"
	ReasonNotConfigured   = "not_configured"
	ReasonRefNotFound     = "ref_not_found"
	ReasonTooLarge        = "too_large"
	ReasonRateLimited     = "rate_limited"
	ReasonGitHubError     = "github_error"
)

// Variable is one variable the repository asks for.
type Variable struct {
	Key      string
	Required bool
	// Default is the Compose default (after :- or -), nil when none.
	Default     *string
	Description string
}

// Result is what a repository asks for at one commit.
type Result struct {
	Status string
	Reason string
	// Path is the file that was read (or looked for).
	Path      string
	Variables []Variable
}

// Unavailable builds a Result for a reason.
func Unavailable(reason, filePath string) Result {
	return Result{Status: StatusUnavailable, Reason: reason, Path: filePath}
}

// Files is the part of the GitHub client the reader needs.
type Files interface {
	ListDir(ctx context.Context, owner, repo, dir, ref string) ([]string, error)
	ReadFile(ctx context.Context, owner, repo, path, ref string) ([]byte, error)
}

// Request names what to read.
type Request struct {
	Owner, Repo string
	// SHA is the full commit ID; results are cached by it.
	SHA    string
	Source string
	// ComposePath is the app's configured Compose file, empty to detect.
	ComposePath string
	// ExamplePath is the .env.example path, empty for the default.
	ExamplePath string
}

// cacheSize bounds the number of remembered commits.
const cacheSize = 64

// Reader fetches and parses requirement files through the GitHub contents
// API. Results for a commit never change, so successful ones are cached
// without expiry (failures are not cached).
type Reader struct {
	files Files

	mu    sync.Mutex
	cache map[Request]Result
	order []Request
}

// NewReader returns a Reader backed by files.
func NewReader(files Files) *Reader {
	return &Reader{files: files, cache: map[Request]Result{}}
}

// Read returns the requirements for req. Failures are reported in the
// Result, not as an error, so the caller can still render the page.
func (r *Reader) Read(ctx context.Context, req Request) Result {
	if req.Source == SourceOff {
		return Result{Status: StatusOff}
	}
	if r.files == nil {
		return Unavailable(ReasonNotConfigured, "")
	}
	r.mu.Lock()
	cached, ok := r.cache[req]
	r.mu.Unlock()
	if ok {
		return cloneResult(cached)
	}
	res := r.fetch(ctx, req)
	if res.Status == StatusOK || res.Reason == ReasonFileNotFound {
		// A missing file at a commit stays missing, so it is cached too.
		r.remember(req, res)
	}
	return cloneResult(res)
}

func (r *Reader) remember(req Request, res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.cache[req]; ok {
		return
	}
	if len(r.order) >= cacheSize {
		delete(r.cache, r.order[0])
		r.order = r.order[1:]
	}
	r.cache[req] = res
	r.order = append(r.order, req)
}

func cloneResult(res Result) Result {
	res.Variables = slices.Clone(res.Variables)
	return res
}

func (r *Reader) fetch(ctx context.Context, req Request) Result {
	// List the root first: when it is not found the App cannot see the
	// repository, which a missing file alone could not tell apart.
	root, err := r.files.ListDir(ctx, req.Owner, req.Repo, "", req.SHA)
	if err != nil {
		if errors.Is(err, github.ErrNotFound) {
			return Unavailable(ReasonRepoUnavailable, "")
		}
		return Unavailable(reasonFor(err), "")
	}
	filePath := ""
	switch req.Source {
	case SourceCompose:
		filePath = req.ComposePath
		if filePath == "" {
			for _, name := range compose.DefaultFiles() {
				if slices.Contains(root, name) {
					filePath = name
					break
				}
			}
			if filePath == "" {
				return Unavailable(ReasonFileNotFound, "")
			}
		}
	case SourceDotenv:
		filePath = req.ExamplePath
		if filePath == "" {
			filePath = DefaultExamplePath
		}
	default:
		return Unavailable(ReasonGitHubError, "")
	}
	filePath = path.Clean(filePath)
	content, err := r.files.ReadFile(ctx, req.Owner, req.Repo, filePath, req.SHA)
	if err != nil {
		if errors.Is(err, github.ErrNotFound) {
			return Unavailable(ReasonFileNotFound, filePath)
		}
		return Unavailable(reasonFor(err), filePath)
	}
	var vars []Variable
	if req.Source == SourceCompose {
		for _, v := range compose.Variables(content) {
			vars = append(vars, Variable{Key: v.Name, Required: v.Required, Default: v.Default, Description: v.Message})
		}
	} else {
		vars = ParseExample(content)
	}
	if vars == nil {
		vars = []Variable{}
	}
	return Result{Status: StatusOK, Path: filePath, Variables: vars}
}

// reasonFor maps a GitHub client error to a reason.
func reasonFor(err error) string {
	switch {
	case errors.Is(err, github.ErrNotFound):
		return ReasonFileNotFound
	case errors.Is(err, github.ErrTooLarge):
		return ReasonTooLarge
	case errors.Is(err, github.ErrRateLimited):
		return ReasonRateLimited
	default:
		return ReasonGitHubError
	}
}

// ParseExample reads the keys of a .env.example file, sorted by key. A
// run of comment lines directly above a key becomes its description; a
// blank line ends the run. Values are ignored (they are placeholders),
// and every key counts as required: the format has no way to say
// otherwise (ARCHITECTURE 9.4).
func ParseExample(content []byte) []Variable {
	byKey := map[string]*Variable{}
	var comments []string
	for _, line := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			comments = nil
			continue
		case strings.HasPrefix(trimmed, "#"):
			comments = append(comments, strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
			continue
		}
		trimmed = strings.TrimPrefix(trimmed, "export ")
		key, _, ok := strings.Cut(trimmed, "=")
		key = strings.TrimSpace(key)
		description := strings.TrimSpace(strings.Join(comments, " "))
		comments = nil
		if !ok || !validKey(key) {
			continue
		}
		if _, seen := byKey[key]; !seen {
			byKey[key] = &Variable{Key: key, Required: true, Description: description}
		}
	}
	out := make([]Variable, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !letter && !(i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// Compare marks which variables are saved and lists saved keys the
// repository does not mention, both sorted by key.
func Compare(vars []Variable, saved []string) (set map[string]bool, unlisted []string) {
	savedSet := make(map[string]bool, len(saved))
	for _, k := range saved {
		savedSet[k] = true
	}
	set = make(map[string]bool, len(vars))
	listed := make(map[string]bool, len(vars))
	for _, v := range vars {
		listed[v.Key] = true
		set[v.Key] = savedSet[v.Key]
	}
	unlisted = []string{}
	for _, k := range saved {
		if !listed[k] {
			unlisted = append(unlisted, k)
		}
	}
	sort.Strings(unlisted)
	return set, unlisted
}
