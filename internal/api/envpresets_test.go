package api

import (
	"bytes"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/github"
	"github.com/tricknotes/koroku/internal/secrets"
)

type envPresetsOut struct {
	Status      string
	Reason      string
	ComposePath string `json:"compose_path"`
	Frameworks  []string
	Presets     []envPresetJSON
	Skipped     []struct{ Key, Service string }
}

func TestEnvPresets(t *testing.T) {
	var logs bytes.Buffer
	gh := &fakeGitHub{files: map[string]string{
		"Gemfile":      "gem \"rails\", \"~> 8.0\"\n",
		"compose.yaml": "services:\n  web:\n    environment:\n      RACK_ENV: production\n",
	}}
	h := newTestServer(t, func(o *Options) {
		o.Auth.GitHub = gh
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	}).Handler()

	rec := request(t, h, http.MethodGet, "/api/repos/octo/blog/env-presets?ref=main", "")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d, cache = %q, body = %s", rec.Code, rec.Header().Get("Cache-Control"), rec.Body)
	}
	got := decode[envPresetsOut](t, rec)
	if got.Status != "ok" || got.ComposePath != "compose.yaml" || strings.Join(got.Frameworks, ",") != "rails" {
		t.Errorf("header = %+v", got)
	}
	if len(got.Presets) != 2 {
		t.Fatalf("presets = %+v", got.Presets)
	}
	env, secret := got.Presets[0], got.Presets[1]
	if env != (envPresetJSON{Key: "RAILS_ENV", Value: "production", Framework: "rails", File: "Gemfile", Match: "rails"}) {
		t.Errorf("RAILS_ENV = %+v", env)
	}
	if secret.Key != "SECRET_KEY_BASE" || !secret.Generated || !regexp.MustCompile(`^[0-9a-f]{128}$`).MatchString(secret.Value) {
		t.Errorf("SECRET_KEY_BASE = %+v", secret)
	}
	if len(got.Skipped) != 1 || got.Skipped[0].Key != "RACK_ENV" || got.Skipped[0].Service != "web" {
		t.Errorf("skipped = %+v", got.Skipped)
	}
	if strings.Contains(logs.String(), secret.Value) {
		t.Error("the generated secret reached the log")
	}

	// Every request generates a new secret.
	again := decode[envPresetsOut](t, request(t, h, http.MethodGet, "/api/repos/octo/blog/env-presets", ""))
	if again.Presets[1].Value == secret.Value {
		t.Error("the secret was reused")
	}

	for _, bad := range []string{"?ref=..%2Fx", "?ref=-x", "?path=..%2Fcompose.yaml", "?path=%2Fetc%2Fpasswd"} {
		if rec := request(t, h, http.MethodGet, "/api/repos/octo/blog/env-presets"+bad, ""); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d", bad, rec.Code)
		}
	}

	gh.err = github.ErrRateLimited
	limited := decode[envPresetsOut](t, request(t, h, http.MethodGet, "/api/repos/octo/blog/env-presets", ""))
	if limited.Status != "unavailable" || limited.Reason != "rate_limited" || len(limited.Presets) != 0 {
		t.Errorf("rate limited = %+v", limited)
	}
}

func TestEnvPresetsWithoutGitHub(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	got := decode[envPresetsOut](t, request(t, h, http.MethodGet, "/api/repos/octo/blog/env-presets", ""))
	if got.Status != "unavailable" || got.Reason != "not_configured" || got.Presets == nil || got.Frameworks == nil {
		t.Errorf("got %+v", got)
	}
}

func TestCreateAppWithEnv(t *testing.T) {
	srv := newTestServer(t, nil)
	h := srv.Handler()
	rec := request(t, h, http.MethodPost, "/api/apps",
		`{"slug": "blog", "repo_full_name": "octo/blog", "web_port": 3000,
		  "env": [{"key": "RAILS_ENV", "value": "production"}, {"key": "SECRET_KEY_BASE", "value": "s3cr3t-value"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cr3t-value") {
		t.Error("the response carries a value")
	}
	app, err := srv.store.GetApp(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := srv.store.ListEnvVars(t.Context(), app.ID)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if bytes.Contains(rows[1].ValueEncrypted, []byte("s3cr3t-value")) {
		t.Error("the value is stored in plain text")
	}
	plain, err := srv.box.Open(rows[1].ValueEncrypted, secrets.EnvVarContext(app.ID, "SECRET_KEY_BASE"))
	if err != nil || string(plain) != "s3cr3t-value" {
		t.Errorf("sealed value = %q, %v", plain, err)
	}
	list := request(t, h, http.MethodGet, "/api/apps/blog/env", "")
	if !strings.Contains(list.Body.String(), `"key":"SECRET_KEY_BASE","value":"s3cr3t-value"`) {
		t.Errorf("env = %s", list.Body)
	}
}

func TestCreateAppWithInvalidEnvCreatesNothing(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	for name, env := range map[string]string{
		"bad key":   `[{"key": "1BAD", "value": "x"}]`,
		"reserved":  `[{"key": "KOROKU_COMMIT_SHA", "value": "x"}]`,
		"duplicate": `[{"key": "A", "value": "x"}, {"key": "A", "value": "y"}]`,
		"too long":  `[{"key": "A", "value": "` + strings.Repeat("x", maxEnvValueLen+1) + `"}]`,
	} {
		rec := request(t, h, http.MethodPost, "/api/apps",
			`{"slug": "blog", "repo_full_name": "octo/blog", "web_port": 3000, "env": `+env+`}`)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"env":`) {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), "xxxx") {
			t.Errorf("%s: the error echoes the value", name)
		}
	}
	if rec := request(t, h, http.MethodGet, "/api/apps/blog", ""); rec.Code != http.StatusNotFound {
		t.Errorf("an app was created: %d", rec.Code)
	}
	// PATCH does not take variables.
	createApp(t, h, "blog")
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog", `{"env": []}`); rec.Code != http.StatusBadRequest {
		t.Errorf("PATCH with env: %d %s", rec.Code, rec.Body)
	}
}

func TestCreateAppWithEnvNeedsRecentLogin(t *testing.T) {
	srv, clk := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")
	origin := map[string]string{"Origin": consoleURL}
	clk.Advance(ReauthWindow)

	withEnv := `{"slug":"blog","web_port":3000,"repo_full_name":"octo/blog","env":[{"key":"SECRET_KEY_BASE","value":"x"}]}`
	rec := b.do(http.MethodPost, "/api/apps", withEnv, origin)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "reauth_required") {
		t.Fatalf("create with env after the window: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(http.MethodGet, "/api/apps/blog", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("the app was created without its variables: %d", rec.Code)
	}
	// Without variables creating needs no recent login.
	for _, body := range []string{
		`{"slug":"blog","web_port":3000,"repo_full_name":"octo/blog","env":[]}`,
		`{"slug":"shop","web_port":3000,"repo_full_name":"octo/shop"}`,
	} {
		if rec := b.do(http.MethodPost, "/api/apps", body, origin); rec.Code != http.StatusCreated {
			t.Errorf("create %s: %d %s", body, rec.Code, rec.Body)
		}
	}
}
