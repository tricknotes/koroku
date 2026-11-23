// Package gitrepo fetches one commit of a repository into a release
// directory, after checking that a branch or tag of the repository reaches
// it. Credentials are handed to git through the environment only, so they
// never reach .git/config, the process list or the deploy log.
package gitrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

// Commit identifies what was checked out.
type Commit struct {
	SHA     string
	Message string // subject line
}

// Auth carries a short-lived token used as the HTTP password.
type Auth struct {
	Token string
}

// Fetcher runs git through a Runner.
type Fetcher struct {
	Runner runner.Runner
	// AllowFileURLs permits file:// repositories. It exists for tests and
	// local development and must stay off in production.
	AllowFileURLs bool
}

var (
	shaPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/@+-]*$`)
)

// IsFullSHA reports whether s is a complete SHA-1 or SHA-256 object name.
func IsFullSHA(s string) bool { return shaPattern.MatchString(s) }

// ValidateRef rejects ref names that git could mistake for options or that
// contain characters with special meaning in refspecs.
func ValidateRef(ref string) error {
	if !refPattern.MatchString(ref) || strings.Contains(ref, "..") || len(ref) > 255 {
		return fmt.Errorf("invalid ref %q", ref)
	}
	return nil
}

// ValidateURL accepts https:// repository URLs without embedded credentials
// (and file:// when allowFile is set). Other transports, in particular
// ext::, can run arbitrary commands.
func ValidateURL(raw string, allowFile bool) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("repository URL is not a valid URL")
	}
	switch u.Scheme {
	case "https":
		if u.Host == "" {
			return errors.New("repository URL has no host")
		}
		if u.User != nil {
			return errors.New("repository URL must not contain credentials")
		}
	case "file":
		if !allowFile {
			return errors.New("file:// repository URLs are not allowed")
		}
		if u.Host != "" || !filepath.IsAbs(u.Path) {
			return errors.New("file:// repository URL must be an absolute path")
		}
	default:
		return fmt.Errorf("repository URL scheme %q is not allowed (use https)", u.Scheme)
	}
	return nil
}

// env returns the environment that isolates git from host configuration and
// carries the credentials.
func (f Fetcher) env(auth *Auth) []string {
	config := [][2]string{
		{"credential.helper", ""},
		{"protocol.ext.allow", "never"},
		{"advice.detachedHead", "false"},
	}
	if f.AllowFileURLs {
		config = append(config, [2]string{"protocol.file.allow", "always"})
	} else {
		config = append(config, [2]string{"protocol.file.allow", "never"})
	}
	if auth != nil && auth.Token != "" {
		basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + auth.Token))
		config = append(config, [2]string{"http.extraheader", "Authorization: Basic " + basic})
	}
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(config)),
	}
	for i, kv := range config {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return env
}

// SecretsFor returns the strings derived from auth that must be masked in logs.
func SecretsFor(auth *Auth) []string {
	if auth == nil || auth.Token == "" {
		return nil
	}
	return []string{
		auth.Token,
		base64.StdEncoding.EncodeToString([]byte("x-access-token:" + auth.Token)),
	}
}

// reachabilityTimeout bounds the check that a ref reaches a commit. It
// downloads the commits of the ref (no files), which takes seconds for an
// ordinary repository; a history too large to read in this time fails the
// deployment instead of holding the single deploy worker.
const reachabilityTimeout = 5 * time.Minute

// NotReachableError says that a commit must not be deployed because the
// branch or tag it was to be deployed from does not reach it.
type NotReachableError struct {
	SHA string
	Ref string
	// RefMissing is set when the repository has no branch or tag named Ref.
	RefMissing bool
}

func (e *NotReachableError) Error() string {
	short := e.SHA
	if len(short) > 8 {
		short = short[:8]
	}
	if e.RefMissing {
		return fmt.Sprintf("the repository has no branch or tag named %q to deploy commit %s from; "+
			"deploy from a branch or tag that contains the commit", e.Ref, short)
	}
	return fmt.Sprintf("commit %s is not reachable from %q: a deployment only takes commits on its branch or tag "+
		"(the commit may have been removed by a force-push, or belong to another branch or to a pull request); "+
		"deploy from a branch or tag that contains the commit", short, e.Ref)
}

// Checkout fetches exactly one commit (depth 1) into dir, which must not
// exist yet or be empty, after checking that the branch or tag ref of the
// repository reaches it. A hosting service hands out every commit that any
// of its refs reaches, including the heads of pull requests from forks, so
// the SHA alone does not say that someone who can write to the repository
// put the commit there. A commit that fails the check is not fetched, and
// the error is a *NotReachableError.
//
// The .git directory is removed afterwards: the release is a plain source
// tree and keeps no trace of the remote.
func (f Fetcher) Checkout(ctx context.Context, repoURL, sha, ref, dir string, auth *Auth, log io.Writer) (Commit, error) {
	if err := ValidateURL(repoURL, f.AllowFileURLs); err != nil {
		return Commit{}, err
	}
	if !IsFullSHA(sha) {
		return Commit{}, fmt.Errorf("commit %q is not a full SHA", sha)
	}
	if err := ValidateRef(ref); err != nil {
		return Commit{}, err
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return Commit{}, fmt.Errorf("release directory %s is not empty", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Commit{}, fmt.Errorf("create release directory: %w", err)
	}
	env := f.env(auth)
	run := func(args ...string) error {
		return f.Runner.Run(ctx, runner.Cmd{Name: "git", Args: args, Dir: dir, Env: env, Stdout: log, Stderr: log})
	}
	if err := run("init", "--quiet"); err != nil {
		return Commit{}, fmt.Errorf("git init: %w", err)
	}
	fetchedHistory, err := f.checkReachable(ctx, repoURL, sha, ref, dir, env, log)
	if fetchedHistory {
		// The check made the repository a partial clone of the URL: git
		// wrote the URL to .git/config, and would fetch the files of the
		// commit one by one. Whatever the check found, that repository
		// goes; the checkout starts in a new one.
		if rmErr := os.RemoveAll(filepath.Join(dir, ".git")); rmErr != nil && err == nil {
			err = fmt.Errorf("remove .git: %w", rmErr)
		}
	}
	if err != nil {
		return Commit{}, err
	}
	if fetchedHistory {
		if err := run("init", "--quiet"); err != nil {
			return Commit{}, fmt.Errorf("git init: %w", err)
		}
	}
	// Fetching straight from the URL keeps the remote out of .git/config.
	if err := run("fetch", "--quiet", "--depth", "1", "--no-tags", "--", repoURL, sha); err != nil {
		return Commit{}, fmt.Errorf("git fetch: %w", err)
	}
	if err := run("checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		return Commit{}, fmt.Errorf("git checkout: %w", err)
	}
	out, err := runner.Output(ctx, f.Runner, runner.Cmd{
		Name: "git", Args: []string{"log", "-1", "--format=%H%n%s"}, Dir: dir, Env: env,
	})
	if err != nil {
		return Commit{}, fmt.Errorf("git log: %w", err)
	}
	head, subject, _ := strings.Cut(strings.TrimRight(string(out), "\n"), "\n")
	if head != sha {
		return Commit{}, fmt.Errorf("checked out %s but %s was requested", head, sha)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		return Commit{}, fmt.Errorf("remove .git: %w", err)
	}
	return Commit{SHA: head, Message: subject}, nil
}

// checkReachable returns nil when the branch or tag ref of the repository
// reaches sha, and a *NotReachableError when it does not. Only refs/heads
// and refs/tags are read: the other namespaces of a hosting service (the
// heads of pull requests, for one) take commits from people who cannot
// write to the repository.
//
// The tip of the ref is compared first, which settles the usual deployment
// of a branch head with one ls-remote. For an older commit, the commits of
// the ref are fetched (without trees and blobs) into the repository in dir;
// fetchedHistory reports that this happened.
func (f Fetcher) checkReachable(ctx context.Context, repoURL, sha, ref, dir string, env []string, log io.Writer) (fetchedHistory bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, reachabilityTimeout)
	defer cancel()
	fail := func(what string, err error) error {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("checking that %q reaches commit %s took longer than %s", ref, sha, reachabilityTimeout)
		}
		return fmt.Errorf("%s: %w", what, err)
	}
	run := func(stdout io.Writer, args ...string) error {
		return f.Runner.Run(ctx, runner.Cmd{Name: "git", Args: args, Dir: dir, Env: env, Stdout: stdout, Stderr: log})
	}

	branch, tag := "refs/heads/"+ref, "refs/tags/"+ref
	var listed bytes.Buffer
	if err := run(&listed, "ls-remote", "--", repoURL, branch, tag, tag+"^{}"); err != nil {
		return false, fail("git ls-remote", err)
	}
	tips := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(listed.String()), "\n") {
		tip, name, ok := strings.Cut(line, "\t")
		if ok && IsFullSHA(tip) {
			tips[name] = tip
		}
	}
	var names []string
	for _, name := range []string{branch, tag} {
		if _, ok := tips[name]; ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return false, &NotReachableError{SHA: sha, Ref: ref, RefMissing: true}
	}
	// tag^{} is the commit an annotated tag points at.
	if tips[branch] == sha || tips[tag] == sha || tips[tag+"^{}"] == sha {
		return false, nil
	}

	for _, name := range names {
		// tree:0 leaves out every tree and blob: the commits are all the
		// check reads. A server without partial clone sends everything.
		if err := run(log, "fetch", "--quiet", "--no-tags", "--filter=tree:0", "--", repoURL, name); err != nil {
			return true, fail("git fetch "+name, err)
		}
		// The commits are listed and searched here instead of asking git
		// about sha: naming a commit that the repository does not have
		// makes git fetch it on demand.
		found := &lineFinder{want: sha}
		if err := run(found, "rev-list", "FETCH_HEAD"); err != nil {
			return true, fail("git rev-list", err)
		}
		if found.done() {
			return true, nil
		}
	}
	return true, &NotReachableError{SHA: sha, Ref: ref}
}

// lineFinder is a writer that looks for a line equal to want.
type lineFinder struct {
	want  string
	rest  []byte
	found bool
}

func (l *lineFinder) Write(p []byte) (int, error) {
	n := len(p)
	if l.found {
		return n, nil
	}
	l.rest = append(l.rest, p...)
	for {
		i := bytes.IndexByte(l.rest, '\n')
		if i < 0 {
			return n, nil
		}
		if string(l.rest[:i]) == l.want {
			l.found, l.rest = true, nil
			return n, nil
		}
		l.rest = l.rest[i+1:]
	}
}

// done reports whether want was written, with or without a final newline.
func (l *lineFinder) done() bool {
	return l.found || string(l.rest) == l.want
}

// ResolveRef turns a branch or tag name into a commit SHA with git
// ls-remote. A full SHA is returned unchanged without contacting the remote.
func (f Fetcher) ResolveRef(ctx context.Context, repoURL, ref string, auth *Auth) (string, error) {
	if IsFullSHA(ref) {
		return ref, nil
	}
	if err := ValidateURL(repoURL, f.AllowFileURLs); err != nil {
		return "", err
	}
	if err := ValidateRef(ref); err != nil {
		return "", err
	}
	var stderr bytes.Buffer
	out, err := runner.Output(ctx, f.Runner, runner.Cmd{
		Name:   "git",
		Args:   []string{"ls-remote", "--", repoURL, "refs/heads/" + ref, "refs/tags/" + ref, "refs/tags/" + ref + "^{}"},
		Env:    f.env(auth),
		Stderr: &stderr, // discarded: it may echo the URL and remote messages
	})
	if err != nil {
		return "", fmt.Errorf("git ls-remote: %w", err)
	}
	found := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		sha, name, ok := strings.Cut(line, "\t")
		if ok && IsFullSHA(sha) {
			found[name] = sha
		}
	}
	// A branch wins over a tag of the same name; a peeled tag over the tag object.
	for _, name := range []string{"refs/heads/" + ref, "refs/tags/" + ref + "^{}", "refs/tags/" + ref} {
		if sha, ok := found[name]; ok {
			return sha, nil
		}
	}
	return "", fmt.Errorf("ref %q was not found in the repository", ref)
}
