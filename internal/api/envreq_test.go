package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/github"
)

type envReqOut struct {
	Source    string
	Status    string
	Reason    string
	Ref       string
	CommitSHA string `json:"commit_sha"`
	Path      string
	Variables []struct {
		Key         string
		Required    bool
		Default     *string
		Description string
		Set         bool
	}
	Unlisted []string
}

func newEnvReqServer(t *testing.T, files map[string]string) (http.Handler, *fakeGitHub) {
	t.Helper()
	gh := &fakeGitHub{files: files}
	srv := newTestServer(t, func(o *Options) { o.Auth.GitHub = gh })
	return srv.Handler(), gh
}

func TestEnvRequirementsCompose(t *testing.T) {
	h, _ := newEnvReqServer(t, map[string]string{
		"compose.yaml": "services:\n  web:\n    environment:\n      A: ${SECRET_KEY_BASE:?Rails secret}\n      B: ${LOG_LEVEL:-info}\n      C: ${SMTP_HOST}\n",
	})
	createApp(t, h, "blog")
	for _, kv := range []string{"SECRET_KEY_BASE", "OLD_FLAG"} {
		if rec := request(t, h, http.MethodPut, "/api/apps/blog/env/"+kv, `{"value": "super-secret-value"}`); rec.Code != http.StatusOK {
			t.Fatalf("put %s: %d %s", kv, rec.Code, rec.Body)
		}
	}
	rec := request(t, h, http.MethodGet, "/api/apps/blog/env/requirements", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "super-secret-value") {
		t.Fatal("response contains a value")
	}
	got := decode[envReqOut](t, rec)
	if got.Source != "compose" || got.Status != "ok" || got.Ref != "main" || got.CommitSHA != testSHA || got.Path != "compose.yaml" {
		t.Errorf("header = %+v", got)
	}
	if len(got.Variables) != 3 {
		t.Fatalf("variables = %+v", got.Variables)
	}
	log, secret, smtp := got.Variables[0], got.Variables[1], got.Variables[2]
	if log.Key != "LOG_LEVEL" || log.Required || log.Default == nil || *log.Default != "info" || log.Set {
		t.Errorf("LOG_LEVEL = %+v", log)
	}
	if secret.Key != "SECRET_KEY_BASE" || !secret.Required || secret.Description != "Rails secret" || !secret.Set {
		t.Errorf("SECRET_KEY_BASE = %+v", secret)
	}
	if smtp.Key != "SMTP_HOST" || !smtp.Required || smtp.Default != nil || smtp.Set {
		t.Errorf("SMTP_HOST = %+v", smtp)
	}
	if len(got.Unlisted) != 1 || got.Unlisted[0] != "OLD_FLAG" {
		t.Errorf("unlisted = %v", got.Unlisted)
	}

	bySHA := decode[envReqOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env/requirements?sha="+strings.ToUpper(testSHA), ""))
	if bySHA.CommitSHA != testSHA || bySHA.Status != "ok" {
		t.Errorf("by sha = %+v", bySHA)
	}
	unknown := decode[envReqOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env/requirements?ref=gone", ""))
	if unknown.Status != "unavailable" || unknown.Reason != "ref_not_found" || len(unknown.Variables) != 0 {
		t.Errorf("unknown ref = %+v", unknown)
	}
	for _, bad := range []string{"?sha=abc", "?ref=..%2Fx", "?ref=-x"} {
		if rec := request(t, h, http.MethodGet, "/api/apps/blog/env/requirements"+bad, ""); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d", bad, rec.Code)
		}
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/missing/env/requirements", ""); rec.Code != http.StatusNotFound {
		t.Errorf("missing app status = %d", rec.Code)
	}
}

func TestEnvRequirementsDotenvAndSettings(t *testing.T) {
	h, _ := newEnvReqServer(t, map[string]string{
		"compose.yaml":       "services: {}\n",
		"config/env.example": "# Outgoing mail\nSMTP_HOST=smtp.example.com\n",
	})
	createApp(t, h, "blog")

	rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"env_source": "dotenv"}`)
	if rec.Code != http.StatusOK || decode[appJSON](t, rec).EnvSource != "dotenv" {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	missing := decode[envReqOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env/requirements", ""))
	if missing.Status != "unavailable" || missing.Reason != "file_not_found" || missing.Path != ".env.example" {
		t.Errorf("default path = %+v", missing)
	}

	request(t, h, http.MethodPatch, "/api/apps/blog", `{"env_example_path": "config/env.example"}`)
	got := decode[envReqOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env/requirements", ""))
	if got.Status != "ok" || len(got.Variables) != 1 || got.Variables[0].Key != "SMTP_HOST" ||
		!got.Variables[0].Required || got.Variables[0].Description != "Outgoing mail" {
		t.Errorf("dotenv = %+v", got)
	}

	request(t, h, http.MethodPatch, "/api/apps/blog", `{"env_source": "off"}`)
	off := decode[envReqOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env/requirements", ""))
	if off.Status != "off" || off.CommitSHA != "" {
		t.Errorf("off = %+v", off)
	}

	for body, field := range map[string]string{
		`{"env_source": "yaml"}`:           "env_source",
		`{"env_example_path": "../x"}`:     "env_example_path",
		`{"env_example_path": "/etc/env"}`: "env_example_path",
	} {
		rec := request(t, h, http.MethodPatch, "/api/apps/blog", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), field) {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
}

func TestEnvRequirementsUnavailable(t *testing.T) {
	// Without a GitHub App nor anonymous access nothing can be read.
	h := newTestServer(t, nil).Handler()
	createApp(t, h, "blog")
	got := decode[envReqOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env/requirements", ""))
	if got.Status != "unavailable" || got.Reason != "not_configured" {
		t.Errorf("no GitHub = %+v", got)
	}

	// Apps with a URL outside github.com are not read through the API.
	h2, _ := newEnvReqServer(t, map[string]string{"compose.yaml": "x: ${A}\n"})
	rec := request(t, h2, http.MethodPost, "/api/apps", `{"slug": "shop", "repo_url": "https://example.com/git/shop.git", "web_port": 3000}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	url := decode[envReqOut](t, request(t, h2, http.MethodGet, "/api/apps/shop/env/requirements", ""))
	if url.Reason != "unsupported_repo" {
		t.Errorf("repo_url = %+v", url)
	}

	// No Compose file at the commit.
	h3, _ := newEnvReqServer(t, map[string]string{"README.md": "hi"})
	createApp(t, h3, "blog")
	none := decode[envReqOut](t, request(t, h3, http.MethodGet, "/api/apps/blog/env/requirements", ""))
	if none.Reason != "file_not_found" {
		t.Errorf("no compose = %+v", none)
	}
}

// Without a GitHub App, or for a github.com URL, public repositories are
// read anonymously (ARCHITECTURE 9.4).
func TestEnvRequirementsPublicRepositories(t *testing.T) {
	public := &fakeGitHub{files: map[string]string{"compose.yaml": "x: ${A:?the A}\n"}}
	h := newTestServer(t, func(o *Options) { o.PublicFiles = public }).Handler()
	createApp(t, h, "blog")
	got := decode[envReqOut](t, request(t, h, http.MethodGet, "/api/apps/blog/env/requirements", ""))
	if got.Status != "ok" || len(got.Variables) != 1 || got.Variables[0].Key != "A" || !got.Variables[0].Required {
		t.Errorf("full name, no App = %+v", got)
	}

	for _, repoURL := range []string{"https://github.com/octo/shop.git", "https://github.com/octo/shop", "https://GitHub.com/octo/shop/"} {
		h := newTestServer(t, func(o *Options) { o.PublicFiles = public }).Handler()
		rec := request(t, h, http.MethodPost, "/api/apps", `{"slug": "shop", "repo_url": "`+repoURL+`", "web_port": 3000}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", repoURL, rec.Code, rec.Body)
		}
		got := decode[envReqOut](t, request(t, h, http.MethodGet, "/api/apps/shop/env/requirements", ""))
		if got.Status != "ok" || len(got.Variables) != 1 {
			t.Errorf("repo_url %s = %+v", repoURL, got)
		}
	}
}

// Compose detection on the create screen reads a public repository the App
// cannot see anonymously, with or without the App.
func TestDetectComposePublicRepositories(t *testing.T) {
	public := &fakeGitHub{files: map[string]string{
		"compose.yaml": "services:\n  web:\n    image: nginx\n    ports:\n      - \"8080:80\"\n",
	}}
	want := `{"files":["compose.yaml"],"path":"compose.yaml","services":[{"name":"web","ports":[80]}]}`

	// Without the App.
	h := newTestServer(t, func(o *Options) { o.PublicFiles = public }).Handler()
	rec := request(t, h, http.MethodGet, "/api/repos/oss/tool/compose?ref=main", "")
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("no App: %d %s", rec.Code, rec.Body)
	}

	// With an App that is not installed on the repository.
	gh := defaultGitHub()
	gh.err = github.ErrNotFound
	srv, _ := newAuthServer(t, gh)
	srv.publicFiles = public
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")
	rec = b.do(http.MethodGet, "/api/repos/oss/tool/compose?ref=main", "", nil)
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("App not installed: %d %s", rec.Code, rec.Body)
	}

	// Other App errors are not hidden by the fallback.
	gh.err = github.ErrRateLimited
	if rec := b.do(http.MethodGet, "/api/repos/oss/tool/compose", "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("rate limited: %d %s", rec.Code, rec.Body)
	}
}
