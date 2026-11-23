package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

// newSourceRepo creates a local repository with two commits and returns its
// file:// URL and the commit SHAs.
func newSourceRepo(t *testing.T) (repoURL, first, second string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "--quiet", "--initial-branch", "main")
	// Let a client fetch any commit by SHA, as GitHub does.
	git("config", "uploadpack.allowAnySHA1InWant", "true")
	write("compose.yaml", "services: {}\n")
	git("add", ".")
	git("commit", "--quiet", "-m", "First commit")
	first = git("rev-parse", "HEAD")
	write("README.md", "hello\n")
	git("add", ".")
	git("commit", "--quiet", "-m", "Second commit\n\nBody text.")
	second = git("rev-parse", "HEAD")
	git("tag", "v1", first)
	git("tag", "-a", "v2", "-m", "annotated", second)
	git("branch", "feature/login", first)
	return "file://" + dir, first, second
}

func TestCheckoutRealGit(t *testing.T) {
	repoURL, first, second := newSourceRepo(t)
	f := Fetcher{Runner: runner.Exec{}, AllowFileURLs: true}
	ctx := context.Background()

	var log bytes.Buffer
	dir := filepath.Join(t.TempDir(), "releases", "1")
	commit, err := f.Checkout(ctx, repoURL, second, "main", dir, nil, &log)
	if err != nil {
		t.Fatalf("Checkout: %v\n%s", err, log.String())
	}
	if commit.SHA != second || commit.Message != "Second commit" {
		t.Errorf("commit = %+v", commit)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Errorf("README.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git must be removed from the release, stat err = %v", err)
	}

	// An older commit of the branch can be fetched by SHA.
	old := filepath.Join(t.TempDir(), "old")
	commit, err = f.Checkout(ctx, repoURL, first, "main", old, nil, &log)
	if err != nil {
		t.Fatalf("Checkout(first): %v\n%s", err, log.String())
	}
	if commit.Message != "First commit" {
		t.Errorf("commit = %+v", commit)
	}
	if _, err := os.Stat(filepath.Join(old, "README.md")); !os.IsNotExist(err) {
		t.Error("the first commit must not contain README.md")
	}
	// The history that the check read is gone with the repository.
	if _, err := os.Stat(filepath.Join(old, ".git")); !os.IsNotExist(err) {
		t.Errorf(".git must be removed from the release, stat err = %v", err)
	}
}

// addForeignCommits gives the repository of newSourceRepo two commits that
// main does not contain: one on the branch release, and one that only
// refs/pull/1/head reaches, as a hosting service keeps the head of a pull
// request from a fork.
func addForeignCommits(t *testing.T, repoURL string) (release, pull string) {
	t.Helper()
	dir := strings.TrimPrefix(repoURL, "file://")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commitOn := func(branch, file string) string {
		t.Helper()
		git("checkout", "--quiet", "-b", branch, "main")
		if err := os.WriteFile(filepath.Join(dir, file), []byte(branch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", ".")
		git("commit", "--quiet", "-m", "Commit on "+branch)
		sha := git("rev-parse", "HEAD")
		git("checkout", "--quiet", "main")
		return sha
	}
	release = commitOn("release", "RELEASE")
	pull = commitOn("from-fork", "FORK")
	git("update-ref", "refs/pull/1/head", pull)
	git("branch", "--quiet", "-D", "from-fork")
	return release, pull
}

func TestCheckoutOnlyTakesCommitsTheRefReaches(t *testing.T) {
	for _, filter := range []string{"true", "false"} {
		t.Run("uploadpack.allowFilter="+filter, func(t *testing.T) {
			repoURL, first, second := newSourceRepo(t)
			release, pull := addForeignCommits(t, repoURL)
			cmd := exec.Command("git", "config", "uploadpack.allowFilter", filter)
			cmd.Dir = strings.TrimPrefix(repoURL, "file://")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git config: %v\n%s", err, out)
			}
			f := Fetcher{Runner: runner.Exec{}, AllowFileURLs: true}
			ctx := context.Background()

			refused := []struct {
				name, sha, ref string
				refMissing     bool
			}{
				{"the head of a pull request, from the default branch", pull, "main", false},
				{"the head of a pull request, from another branch", pull, "release", false},
				{"the head of a pull request, from a tag", pull, "v2", false},
				{"the head of a pull request, from its own ref", pull, "pull/1/head", true},
				{"the head of a pull request, from its full ref", pull, "refs/pull/1/head", true},
				{"a commit of another branch", release, "main", false},
				{"a commit newer than the tag", second, "v1", false},
				{"a commit from a branch that does not exist", second, "no-such-branch", true},
			}
			for _, c := range refused {
				var log bytes.Buffer
				dir := filepath.Join(t.TempDir(), "release")
				_, err := f.Checkout(ctx, repoURL, c.sha, c.ref, dir, nil, &log)
				var notReachable *NotReachableError
				if !errors.As(err, &notReachable) {
					t.Errorf("%s: err = %v, want a NotReachableError\n%s", c.name, err, log.String())
					continue
				}
				if notReachable.RefMissing != c.refMissing || notReachable.Ref != c.ref || notReachable.SHA != c.sha {
					t.Errorf("%s: error = %+v", c.name, notReachable)
				}
				// Nothing of the commit reached the release directory.
				entries, _ := os.ReadDir(dir)
				for _, e := range entries {
					if e.Name() != ".git" {
						t.Errorf("%s: %s was checked out", c.name, e.Name())
					}
				}
				// The repository the check filled is gone: it names the URL
				// in its configuration.
				if _, err := os.Stat(filepath.Join(dir, ".git", "config")); err == nil && !c.refMissing {
					t.Errorf("%s: the repository of the check was left behind", c.name)
				}
				// The check itself does not bring the commit in either: git
				// would fetch a missing commit on demand if it were named.
				probe := t.TempDir()
				if err := f.Runner.Run(ctx, runner.Cmd{Name: "git", Args: []string{"init", "--quiet"}, Dir: probe, Env: f.env(nil)}); err != nil {
					t.Fatal(err)
				}
				if _, err := f.checkReachable(ctx, repoURL, c.sha, c.ref, probe, f.env(nil), &log); !errors.As(err, &notReachable) {
					t.Errorf("%s: checkReachable = %v", c.name, err)
				}
				if c.sha == pull && hasObject(t, probe, pull) {
					t.Errorf("%s: the refused commit was fetched", c.name)
				}
				if c.ref == "main" && !hasObject(t, probe, second) {
					t.Errorf("%s: the history of main was not fetched, so the test cannot tell", c.name)
				}
			}

			accepted := []struct {
				name, sha, ref, file string
			}{
				{"the tip of the default branch", second, "main", "README.md"},
				{"a commit behind the tip", first, "main", "compose.yaml"},
				{"the tip of another branch", release, "release", "RELEASE"},
				{"a commit behind the tip of another branch", second, "release", "README.md"},
				{"a branch with a slash", first, "feature/login", "compose.yaml"},
				{"a lightweight tag", first, "v1", "compose.yaml"},
				{"an annotated tag", second, "v2", "README.md"},
				{"a commit behind an annotated tag", first, "v2", "compose.yaml"},
			}
			for _, c := range accepted {
				var log bytes.Buffer
				dir := filepath.Join(t.TempDir(), "release")
				commit, err := f.Checkout(ctx, repoURL, c.sha, c.ref, dir, nil, &log)
				if err != nil || commit.SHA != c.sha {
					t.Errorf("%s: Checkout = %+v, %v\n%s", c.name, commit, err, log.String())
					continue
				}
				if _, err := os.Stat(filepath.Join(dir, c.file)); err != nil {
					t.Errorf("%s: %s missing: %v", c.name, c.file, err)
				}
				if _, err := os.Stat(filepath.Join(dir, ".git")); !os.IsNotExist(err) {
					t.Errorf("%s: .git must be removed from the release, stat err = %v", c.name, err)
				}
			}
		})
	}
}

// hasObject reports whether the repository in dir holds the object, loose
// or packed, without letting git fetch it on demand.
func hasObject(t *testing.T, dir, sha string) bool {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, ".git", "objects", sha[:2], sha[2:])); err == nil {
		return true
	}
	packs, _ := filepath.Glob(filepath.Join(dir, ".git", "objects", "pack", "*.idx"))
	for _, idx := range packs {
		cmd := exec.Command("git", "show-index")
		in, err := os.Open(idx)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdin = in
		out, err := cmd.Output()
		_ = in.Close()
		if err != nil {
			t.Fatalf("git show-index: %v", err)
		}
		if strings.Contains(string(out), sha) {
			return true
		}
	}
	return false
}

func TestNotReachableErrorSaysWhatToDo(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, err := range []error{
		&NotReachableError{SHA: sha, Ref: "main"},
		&NotReachableError{SHA: sha, Ref: "main", RefMissing: true},
	} {
		msg := err.Error()
		for _, want := range []string{"aaaaaaaa", `"main"`, "deploy from a branch or tag that contains the commit"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%q lacks %q", msg, want)
			}
		}
	}
}

func TestLineFinder(t *testing.T) {
	sha := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	cases := []struct {
		name   string
		chunks []string
		want   bool
	}{
		{"empty", nil, false},
		{"only line", []string{sha + "\n"}, true},
		{"last line without newline", []string{other + "\n", sha}, true},
		{"split across writes", []string{other + "\n" + sha[:10], sha[10:] + "\n" + other + "\n"}, true},
		{"absent", []string{other + "\n", other + "\n"}, false},
		{"prefix of a longer line", []string{sha + "0\n"}, false},
		{"suffix of a longer line", []string{"0" + sha + "\n"}, false},
	}
	for _, c := range cases {
		l := &lineFinder{want: sha}
		for _, chunk := range c.chunks {
			if n, err := l.Write([]byte(chunk)); n != len(chunk) || err != nil {
				t.Errorf("%s: Write = %d, %v", c.name, n, err)
			}
		}
		if l.done() != c.want {
			t.Errorf("%s: done = %v, want %v", c.name, l.done(), c.want)
		}
	}
}

func TestCheckoutRejectsBadInput(t *testing.T) {
	repoURL, _, second := newSourceRepo(t)
	f := Fetcher{Runner: runner.Exec{}, AllowFileURLs: true}
	ctx := context.Background()

	if _, err := f.Checkout(ctx, repoURL, "main", "main", t.TempDir(), nil, nil); err == nil {
		t.Error("a branch name must not be accepted as a SHA")
	}
	if _, err := f.Checkout(ctx, repoURL, strings.Repeat("0", 40), "main", filepath.Join(t.TempDir(), "x"), nil, nil); err == nil {
		t.Error("an unknown commit must fail")
	}
	for _, bad := range []string{"", "--upload-pack=touch /tmp/x", "-x", "a..b", "a b", "main:refs/heads/x", "+main", "ma*n", "main^"} {
		if _, err := f.Checkout(ctx, repoURL, second, bad, filepath.Join(t.TempDir(), "z"), nil, nil); err == nil {
			t.Errorf("Checkout from ref %q must fail", bad)
		}
	}
	occupied := t.TempDir()
	if err := os.WriteFile(filepath.Join(occupied, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Checkout(ctx, repoURL, second, "main", occupied, nil, nil); err == nil {
		t.Error("a non-empty release directory must be rejected")
	}
	strict := Fetcher{Runner: runner.Exec{}}
	if _, err := strict.Checkout(ctx, repoURL, second, "main", filepath.Join(t.TempDir(), "y"), nil, nil); err == nil {
		t.Error("file:// must be rejected unless explicitly allowed")
	}
}

func TestResolveRefRealGit(t *testing.T) {
	repoURL, first, second := newSourceRepo(t)
	f := Fetcher{Runner: runner.Exec{}, AllowFileURLs: true}
	ctx := context.Background()

	cases := map[string]string{
		"main":          second,
		"feature/login": first,
		"v1":            first,
		"v2":            second, // annotated tag resolves to the commit, not the tag object
		first:           first,
	}
	for ref, want := range cases {
		got, err := f.ResolveRef(ctx, repoURL, ref, nil)
		if err != nil || got != want {
			t.Errorf("ResolveRef(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	if _, err := f.ResolveRef(ctx, repoURL, "no-such-branch", nil); err == nil {
		t.Error("an unknown ref must fail")
	}
	for _, bad := range []string{"--upload-pack=touch /tmp/x", "-x", "a..b", "a b", ""} {
		if _, err := f.ResolveRef(ctx, repoURL, bad, nil); err == nil {
			t.Errorf("ResolveRef(%q) must fail", bad)
		}
	}
}

func TestValidateURL(t *testing.T) {
	valid := []string{"https://github.com/octo/app.git", "https://example.com/git/app"}
	for _, u := range valid {
		if err := ValidateURL(u, false); err != nil {
			t.Errorf("ValidateURL(%q) = %v", u, err)
		}
	}
	invalid := []string{
		"", "octo/app", "git@github.com:octo/app.git", "ssh://git@github.com/octo/app.git",
		"http://github.com/octo/app.git", "ext::sh -c 'touch /tmp/x'", "https://user:pass@github.com/octo/app.git",
		"https://token@github.com/octo/app.git", "https:///octo/app.git", "file:///srv/repo", "-https://x",
	}
	for _, u := range invalid {
		if err := ValidateURL(u, false); err == nil {
			t.Errorf("ValidateURL(%q) must fail", u)
		}
	}
	if err := ValidateURL("file:///srv/repo", true); err != nil {
		t.Errorf("file URL with allowFile: %v", err)
	}
	if err := ValidateURL("file://host/srv/repo", true); err == nil {
		t.Error("a file URL with a host must fail")
	}
	err := ValidateURL("https://user:hunter2secret@github.com/octo/app.git", false)
	if err == nil || strings.Contains(err.Error(), "hunter2secret") {
		t.Errorf("the error must not echo credentials: %v", err)
	}
}

func TestTokenIsPassedOnlyThroughEnvironment(t *testing.T) {
	const token = "ghs_exampleInstallationToken123"
	sha := strings.Repeat("a", 40)
	tip := strings.Repeat("b", 40)
	// The commit is behind the tip of main, so the check reads the history.
	fake := &runner.Fake{Handler: func(_ context.Context, cmd runner.Cmd) (string, error) {
		switch cmd.Args[0] {
		case "ls-remote":
			return tip + "\trefs/heads/main\n", nil
		case "rev-list":
			return tip + "\n" + sha + "\n", nil
		case "log":
			return sha + "\nSubject\n", nil
		}
		return "", nil
	}}
	f := Fetcher{Runner: fake}
	dir := filepath.Join(t.TempDir(), "release")
	var log bytes.Buffer
	const repoURL = "https://github.com/octo/private.git"
	if _, err := f.Checkout(context.Background(), repoURL, sha, "main", dir, &Auth{Token: token}, &log); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	calls := fake.Calls()
	wantCalls := [][]string{
		{"init", "--quiet"},
		{"ls-remote", "--", repoURL, "refs/heads/main", "refs/tags/main", "refs/tags/main^{}"},
		{"fetch", "--quiet", "--no-tags", "--filter=tree:0", "--", repoURL, "refs/heads/main"},
		{"rev-list", "FETCH_HEAD"},
		{"init", "--quiet"},
		{"fetch", "--quiet", "--depth", "1", "--no-tags", "--", repoURL, sha},
		{"checkout", "--quiet", "--detach", "FETCH_HEAD"},
		{"log", "-1", "--format=%H%n%s"},
	}
	if len(calls) != len(wantCalls) {
		t.Fatalf("calls = %v, want %d", calls, len(wantCalls))
	}
	for i, want := range wantCalls {
		if strings.Join(calls[i].Args, " ") != strings.Join(want, " ") {
			t.Errorf("call %d args = %v, want %v", i, calls[i].Args, want)
		}
	}
	fetch := calls[5]
	secrets := SecretsFor(&Auth{Token: token})
	if len(secrets) != 2 {
		t.Fatalf("SecretsFor = %v", secrets)
	}
	for _, call := range calls {
		for _, arg := range call.Args {
			for _, s := range secrets {
				if strings.Contains(arg, s) {
					t.Errorf("token leaked into argv: %v", call.Args)
				}
			}
		}
		if call.Dir != dir {
			t.Errorf("git ran in %q, want %q", call.Dir, dir)
		}
	}
	// The calls that reach the remote for the check carry the token the
	// same way.
	for _, i := range []int{1, 2} {
		if !strings.Contains(strings.Join(calls[i].Env, "\n"), "Authorization: Basic "+secrets[1]) {
			t.Errorf("call %d (%s) was not given the token", i, calls[i].Args[0])
		}
	}
	env := strings.Join(fetch.Env, "\n")
	if !strings.Contains(env, "GIT_CONFIG_KEY_4=http.extraheader") || !strings.Contains(env, "Authorization: Basic "+secrets[1]) {
		t.Errorf("auth header not configured through the environment:\n%s", env)
	}
	for _, want := range []string{"GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "protocol.ext.allow", "GIT_CONFIG_VALUE_3=never"} {
		if !strings.Contains(env, want) {
			t.Errorf("environment lacks %q", want)
		}
	}
	if SecretsFor(nil) != nil || SecretsFor(&Auth{}) != nil {
		t.Error("no secrets expected without a token")
	}
}

func TestCheckoutDetectsWrongCommit(t *testing.T) {
	requested := strings.Repeat("a", 40)
	fake := &runner.Fake{Handler: func(_ context.Context, cmd runner.Cmd) (string, error) {
		switch cmd.Args[0] {
		case "ls-remote":
			return requested + "\trefs/heads/main\n", nil
		case "log":
			return strings.Repeat("b", 40) + "\nOther\n", nil
		}
		return "", nil
	}}
	_, err := Fetcher{Runner: fake}.Checkout(context.Background(), "https://github.com/octo/app.git",
		requested, "main", filepath.Join(t.TempDir(), "r"), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "was requested") {
		t.Errorf("err = %v", err)
	}
}

// A commit that the ref does not reach is refused before git is asked for
// it: nothing after the check runs.
func TestCheckoutStopsAtAnUnreachableCommit(t *testing.T) {
	requested := strings.Repeat("a", 40)
	tip := strings.Repeat("b", 40)
	cases := map[string]struct {
		lsRemote string
		want     []string
		missing  bool
	}{
		"no such branch or tag": {"", []string{"init", "ls-remote"}, true},
		// A ref of another namespace that ends in the same name is not the branch.
		"only a ref elsewhere": {requested + "\trefs/pull/refs/heads/main\n", []string{"init", "ls-remote"}, true},
		"not in the history":   {tip + "\trefs/heads/main\n", []string{"init", "ls-remote", "fetch", "rev-list"}, false},
		"not in the history of the branch or the tag": {
			tip + "\trefs/heads/main\n" + tip + "\trefs/tags/main\n",
			[]string{"init", "ls-remote", "fetch", "rev-list", "fetch", "rev-list"}, false,
		},
	}
	for name, c := range cases {
		fake := &runner.Fake{Handler: func(_ context.Context, cmd runner.Cmd) (string, error) {
			switch cmd.Args[0] {
			case "ls-remote":
				return c.lsRemote, nil
			case "rev-list":
				return tip + "\n", nil
			}
			return "", nil
		}}
		_, err := Fetcher{Runner: fake}.Checkout(context.Background(), "https://github.com/octo/app.git",
			requested, "main", filepath.Join(t.TempDir(), "r"), nil, nil)
		var notReachable *NotReachableError
		if !errors.As(err, &notReachable) || notReachable.RefMissing != c.missing {
			t.Errorf("%s: err = %v", name, err)
		}
		var got []string
		for _, call := range fake.Calls() {
			got = append(got, call.Args[0])
			if slices.Contains(call.Args, requested) {
				t.Errorf("%s: git was asked for the commit: %v", name, call.Args)
			}
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s: git ran %v, want %v", name, got, c.want)
		}
	}
}

func TestCheckReachableReportsItsTimeout(t *testing.T) {
	fake := &runner.Fake{Handler: func(context.Context, runner.Cmd) (string, error) {
		return "", &runner.ExitError{Cmd: "git", Code: 128}
	}}
	// The check runs under its own deadline inside the caller's; a command
	// that fails once it has passed is reported as the check taking too long.
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := Fetcher{Runner: fake}.checkReachable(ctx, "https://github.com/octo/app.git",
		strings.Repeat("a", 40), "main", t.TempDir(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "took longer than") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveRefPassesTokenThroughEnvironmentOnly(t *testing.T) {
	const token = "ghs_exampleInstallationToken123"
	sha := strings.Repeat("c", 40)
	fake := &runner.Fake{Handler: func(context.Context, runner.Cmd) (string, error) {
		return sha + "\trefs/heads/main\n", nil
	}}
	got, err := Fetcher{Runner: fake}.ResolveRef(context.Background(), "https://github.com/octo/private.git", "main", &Auth{Token: token})
	if err != nil || got != sha {
		t.Fatalf("ResolveRef = %q, %v", got, err)
	}
	call := fake.Calls()[0]
	for _, arg := range call.Args {
		if strings.Contains(arg, token) {
			t.Errorf("token in argv: %v", call.Args)
		}
	}
	if !strings.Contains(strings.Join(call.Env, "\n"), "http.extraheader") {
		t.Error("the token was not passed through the environment")
	}
	// Without a token no auth header is configured at all.
	fake.Reset()
	_, _ = Fetcher{Runner: fake}.ResolveRef(context.Background(), "https://github.com/octo/public.git", "main", nil)
	if strings.Contains(strings.Join(fake.Calls()[0].Env, "\n"), "http.extraheader") {
		t.Error("an auth header was configured without a token")
	}
}

func TestResolveRefFailureDoesNotLeakRemoteOutput(t *testing.T) {
	fake := &runner.Fake{Handler: func(_ context.Context, cmd runner.Cmd) (string, error) {
		_, _ = cmd.Stderr.Write([]byte("fatal: Authentication failed for 'https://x-access-token:ghs_secret@github.com/octo/private.git'\n"))
		return "", &runner.ExitError{Cmd: "git", Code: 128}
	}}
	_, err := Fetcher{Runner: fake}.ResolveRef(context.Background(), "https://github.com/octo/private.git", "main", &Auth{Token: "ghs_secret"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "ghs_secret") || strings.Contains(err.Error(), "Authentication failed") {
		t.Errorf("git's stderr leaked into the error: %v", err)
	}
}
