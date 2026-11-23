package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestApp(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		if !f.requireAppJWT(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":          testAppID,
			"slug":        "koroku-test",
			"client_id":   testClientID,
			"html_url":    "https://github.com/apps/koroku-test",
			"permissions": map[string]string{"contents": "read", "metadata": "read"},
			"owner":       map[string]string{"login": "octo", "type": "User"},
		})
	})
	app, err := f.client().App(context.Background())
	if err != nil {
		t.Fatalf("App: %v", err)
	}
	if app.ID != testAppID || app.Slug != "koroku-test" || app.ClientID != testClientID {
		t.Errorf("app = %+v", app)
	}
	if app.Owner != "octo" || app.OwnerIsOrg {
		t.Errorf("owner = %q (org %v)", app.Owner, app.OwnerIsOrg)
	}
	if app.Permissions["contents"] != "read" || app.Permissions["metadata"] != "read" {
		t.Errorf("permissions = %v", app.Permissions)
	}
}

func TestAppNeedsCredentials(t *testing.T) {
	c, err := NewAnonymous("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.App(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
	if _, err := c.Installations(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Errorf("err = %v, want ErrNotConfigured", err)
	}
}

func TestInstallations(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("GET /app/installations", func(w http.ResponseWriter, r *http.Request) {
		if !f.requireAppJWT(w, r) {
			return
		}
		writeJSON(w, http.StatusOK, []map[string]any{
			{"id": 7, "account": map[string]any{"id": 1001, "login": "octocat"}, "repository_selection": "selected"},
		})
	})
	list, err := f.client().Installations(context.Background())
	if err != nil {
		t.Fatalf("Installations: %v", err)
	}
	if len(list) != 1 || list[0].ID != 7 || list[0].Account != "octocat" || list[0].AccountID != 1001 || list[0].RepositorySelection != "selected" {
		t.Errorf("installations = %+v", list)
	}
}

func TestUserByID(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("GET /user/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the user lookup sent credentials")
		}
		if r.PathValue("id") != "583231" {
			writeError(w, http.StatusNotFound, "Not Found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": 583231, "login": "octocat"})
	})
	c := f.client()
	user, err := c.UserByID(context.Background(), 583231)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if user.ID != 583231 || user.Login != "octocat" {
		t.Errorf("user = %+v", user)
	}
	if _, err := c.UserByID(context.Background(), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing user: err = %v, want ErrNotFound", err)
	}
}

func TestUserByLogin(t *testing.T) {
	f := newFakeGitHub(t)
	f.mux.HandleFunc("GET /users/{login}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("the user lookup sent credentials")
		}
		switch r.PathValue("login") {
		case "octocat":
			writeJSON(w, http.StatusOK, map[string]any{"id": 583231, "login": "octocat", "type": "User"})
		case "octo-org":
			writeJSON(w, http.StatusOK, map[string]any{"id": 9919, "login": "octo-org", "type": "Organization"})
		default:
			writeError(w, http.StatusNotFound, "Not Found")
		}
	})
	c := f.client()
	user, err := c.UserByLogin(context.Background(), "octocat")
	if err != nil {
		t.Fatalf("UserByLogin: %v", err)
	}
	if user.ID != 583231 || user.Login != "octocat" {
		t.Errorf("user = %+v", user)
	}
	for _, login := range []string{"nobody", "octo-org"} {
		if _, err := c.UserByLogin(context.Background(), login); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", login, err)
		}
	}
	for _, login := range []string{"", "-octo", "octo-", "oc--to", "../user", strings.Repeat("a", 40)} {
		if _, err := c.UserByLogin(context.Background(), login); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%q: err = %v, want ErrInvalidArgument", login, err)
		}
	}
}

func TestAppSettingsAndInstallURLs(t *testing.T) {
	user := App{Slug: "koroku-octo", HTMLURL: "https://github.com/apps/koroku-octo", Owner: "octo"}
	if got := user.SettingsURL(); got != "https://github.com/settings/apps/koroku-octo" {
		t.Errorf("user settings = %q", got)
	}
	org := App{Slug: "koroku", HTMLURL: "https://github.com/apps/koroku", Owner: "acme", OwnerIsOrg: true}
	if got := org.SettingsURL(); got != "https://github.com/organizations/acme/settings/apps/koroku" {
		t.Errorf("org settings = %q", got)
	}
	if got := org.InstallURL(); got != "https://github.com/apps/koroku/installations/new" {
		t.Errorf("install = %q", got)
	}
	if got := (App{Slug: "x", HTMLURL: "https://example.com/other"}).SettingsURL(); got != "" {
		t.Errorf("unexpected html_url: settings = %q", got)
	}
}
