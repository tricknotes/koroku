package deploy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

func TestGitHubRepoSelection(t *testing.T) {
	cases := []struct {
		app   store.App
		owner string
		repo  string
		ok    bool
	}{
		{store.App{RepoFullName: "octo/blog"}, "octo", "blog", true},
		// An explicit URL means "fetch exactly this", without App credentials.
		{store.App{RepoFullName: "octo/blog", RepoURL: "https://example.com/blog.git"}, "", "", false},
		{store.App{RepoURL: "https://example.com/blog.git"}, "", "", false},
		{store.App{RepoFullName: "not a repo"}, "", "", false},
		{store.App{}, "", "", false},
	}
	for _, tc := range cases {
		owner, repo, ok := githubRepo(tc.app)
		if owner != tc.owner || repo != tc.repo || ok != tc.ok {
			t.Errorf("githubRepo(%+v) = %q, %q, %v", tc.app, owner, repo, ok)
		}
	}
}

func TestGitRefResolverUsesLsRemote(t *testing.T) {
	sha := "1111111111111111111111111111111111111111"
	fake := &runner.Fake{Handler: func(_ context.Context, cmd runner.Cmd) (string, error) {
		return sha + "\trefs/heads/main\n", nil
	}}
	resolver := GitRefResolver{Fetcher: gitrepo.Fetcher{Runner: fake}}

	got, message, err := resolver.ResolveRef(context.Background(), store.App{RepoFullName: "octo/blog"}, "main")
	if err != nil || got != sha || message != "" {
		t.Fatalf("ResolveRef = %q, %q, %v", got, message, err)
	}
	call := fake.Calls()[0]
	if call.Name != "git" || call.Args[0] != "ls-remote" || call.Args[2] != "https://github.com/octo/blog.git" {
		t.Errorf("call = %v", call.Args)
	}
	if _, _, err := resolver.ResolveRef(context.Background(), store.App{}, "main"); err == nil {
		t.Error("an app without a repository must fail")
	}

	failing := GitRefResolver{Fetcher: gitrepo.Fetcher{Runner: fake}, Tokens: failingTokens{}}
	if _, _, err := failing.ResolveRef(context.Background(), store.App{RepoFullName: "octo/blog"}, "main"); err == nil {
		t.Error("a token error must be reported")
	}
}

type failingTokens struct{}

func (failingTokens) RepoToken(context.Context, store.App) (string, error) {
	return "", errors.New("token service down")
}

func TestFirstLine(t *testing.T) {
	for in, want := range map[string]string{"Subject\n\nBody": "Subject", "Only": "Only", "": "", "\nx": ""} {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q", in, got)
		}
	}
}

type fakeGitHubAPI struct {
	token     string
	tokenErr  error
	commit    github.Commit
	commitErr error
	calls     []string
}

func (f *fakeGitHubAPI) RepoToken(_ context.Context, owner, repo string) (github.Token, error) {
	f.calls = append(f.calls, "token:"+owner+"/"+repo)
	if f.tokenErr != nil {
		return github.Token{}, f.tokenErr
	}
	return github.NewToken(f.token, time.Now().Add(time.Hour)), nil
}

func (f *fakeGitHubAPI) ResolveCommit(_ context.Context, owner, repo, ref string) (github.Commit, error) {
	f.calls = append(f.calls, "resolve:"+owner+"/"+repo+"@"+ref)
	return f.commit, f.commitErr
}

func TestGitHubTokens(t *testing.T) {
	ctx := context.Background()
	api := &fakeGitHubAPI{token: "ghs_exampleInstallationToken123"}
	tokens := GitHubTokens{Client: api}

	got, err := tokens.RepoToken(ctx, store.App{RepoFullName: "octo/private"})
	if err != nil || got != "ghs_exampleInstallationToken123" {
		t.Fatalf("RepoToken = %q, %v", got, err)
	}
	// No token is requested for an explicit URL.
	api.calls = nil
	got, err = tokens.RepoToken(ctx, store.App{RepoURL: "https://example.com/x.git"})
	if err != nil || got != "" || len(api.calls) != 0 {
		t.Errorf("explicit URL: %q, %v, calls %v", got, err, api.calls)
	}
	// Not installed on the repository: try anonymously (public repository).
	api.tokenErr = github.ErrNotFound
	got, err = tokens.RepoToken(ctx, store.App{RepoFullName: "someone/public"})
	if err != nil || got != "" {
		t.Errorf("not installed: %q, %v", got, err)
	}
	// Other failures stop the deploy instead of silently going anonymous.
	api.tokenErr = github.ErrRateLimited
	if _, err := tokens.RepoToken(ctx, store.App{RepoFullName: "octo/private"}); !errors.Is(err, github.ErrRateLimited) {
		t.Errorf("err = %v", err)
	}
}

func TestGitHubRefResolver(t *testing.T) {
	ctx := context.Background()
	sha := "2222222222222222222222222222222222222222"
	lsRemoteSHA := "3333333333333333333333333333333333333333"
	git := &runner.Fake{Handler: func(context.Context, runner.Cmd) (string, error) {
		return lsRemoteSHA + "\trefs/heads/main\n", nil
	}}
	api := &fakeGitHubAPI{commit: github.Commit{SHA: sha, Message: "Fix login\n\nDetails."}}
	resolver := GitHubRefResolver{Client: api, Fallback: GitRefResolver{Fetcher: gitrepo.Fetcher{Runner: git}}}

	got, message, err := resolver.ResolveRef(ctx, store.App{RepoFullName: "octo/blog"}, "main")
	if err != nil || got != sha || message != "Fix login" {
		t.Fatalf("ResolveRef = %q, %q, %v", got, message, err)
	}
	if len(git.Calls()) != 0 {
		t.Error("git must not be used when the API answers")
	}

	// Explicit URL and repositories the App cannot see fall back to git.
	got, _, err = resolver.ResolveRef(ctx, store.App{RepoURL: "https://example.com/x.git"}, "main")
	if err != nil || got != lsRemoteSHA {
		t.Errorf("explicit URL: %q, %v", got, err)
	}
	api.commitErr = github.ErrNotFound
	got, _, err = resolver.ResolveRef(ctx, store.App{RepoFullName: "someone/public"}, "main")
	if err != nil || got != lsRemoteSHA {
		t.Errorf("not visible to the App: %q, %v", got, err)
	}
	api.commitErr = github.ErrRateLimited
	if _, _, err := resolver.ResolveRef(ctx, store.App{RepoFullName: "octo/blog"}, "main"); !errors.Is(err, github.ErrRateLimited) {
		t.Errorf("err = %v", err)
	}
}
