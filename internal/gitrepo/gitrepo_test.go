package gitrepo

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	commit, err := f.Checkout(ctx, repoURL, second, dir, nil, &log)
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

	// An older commit that is not a branch tip can be fetched by SHA.
	old := filepath.Join(t.TempDir(), "old")
	commit, err = f.Checkout(ctx, repoURL, first, old, nil, &log)
	if err != nil {
		t.Fatalf("Checkout(first): %v\n%s", err, log.String())
	}
	if commit.Message != "First commit" {
		t.Errorf("commit = %+v", commit)
	}
	if _, err := os.Stat(filepath.Join(old, "README.md")); !os.IsNotExist(err) {
		t.Error("the first commit must not contain README.md")
	}
}

func TestCheckoutRejectsBadInput(t *testing.T) {
	repoURL, _, second := newSourceRepo(t)
	f := Fetcher{Runner: runner.Exec{}, AllowFileURLs: true}
	ctx := context.Background()

	if _, err := f.Checkout(ctx, repoURL, "main", t.TempDir(), nil, nil); err == nil {
		t.Error("a branch name must not be accepted as a SHA")
	}
	if _, err := f.Checkout(ctx, repoURL, strings.Repeat("0", 40), filepath.Join(t.TempDir(), "x"), nil, nil); err == nil {
		t.Error("an unknown commit must fail")
	}
	occupied := t.TempDir()
	if err := os.WriteFile(filepath.Join(occupied, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Checkout(ctx, repoURL, second, occupied, nil, nil); err == nil {
		t.Error("a non-empty release directory must be rejected")
	}
	strict := Fetcher{Runner: runner.Exec{}}
	if _, err := strict.Checkout(ctx, repoURL, second, filepath.Join(t.TempDir(), "y"), nil, nil); err == nil {
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
	fake := &runner.Fake{Handler: func(_ context.Context, cmd runner.Cmd) (string, error) {
		if len(cmd.Args) > 0 && cmd.Args[0] == "log" {
			return sha + "\nSubject\n", nil
		}
		return "", nil
	}}
	f := Fetcher{Runner: fake}
	dir := filepath.Join(t.TempDir(), "release")
	var log bytes.Buffer
	if _, err := f.Checkout(context.Background(), "https://github.com/octo/private.git", sha, dir, &Auth{Token: token}, &log); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 4 {
		t.Fatalf("calls = %d, want 4", len(calls))
	}
	fetch := calls[1]
	want := []string{"fetch", "--quiet", "--depth", "1", "--no-tags", "--", "https://github.com/octo/private.git", sha}
	if strings.Join(fetch.Args, " ") != strings.Join(want, " ") {
		t.Errorf("fetch args = %v", fetch.Args)
	}
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
		if len(cmd.Args) > 0 && cmd.Args[0] == "log" {
			return strings.Repeat("b", 40) + "\nOther\n", nil
		}
		return "", nil
	}}
	_, err := Fetcher{Runner: fake}.Checkout(context.Background(), "https://github.com/octo/app.git",
		requested, filepath.Join(t.TempDir(), "r"), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "was requested") {
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
