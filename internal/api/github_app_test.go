package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tricknotes/koroku/internal/github"
)

func TestGitHubAppStatus(t *testing.T) {
	gh := defaultGitHub()
	gh.app = github.App{Slug: "koroku-octo", HTMLURL: "https://github.com/apps/koroku-octo", Owner: "octo"}
	gh.installs = []github.Installation{
		{ID: 1, Account: "octo", RepositorySelection: "selected", Allowed: true},
		{ID: 2, Account: "my-org", RepositorySelection: "all", Allowed: false},
	}
	srv, _ := newAuthServer(t, gh)
	srv.cfg.GitHub.InstallationAccounts = []string{"other-org"}
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")

	rec := b.do(http.MethodGet, "/api/system/github", "", nil)
	want := `{"configured":true,"app":{"slug":"koroku-octo","html_url":"https://github.com/apps/koroku-octo",` +
		`"settings_url":"https://github.com/settings/apps/koroku-octo","install_url":"https://github.com/apps/koroku-octo/installations/new"},` +
		`"installations":[{"account":"octo","repository_selection":"selected","used":true},` +
		`{"account":"my-org","repository_selection":"all","used":false}],"installation_accounts":["other-org"]}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Errorf("status = %d %s", rec.Code, rec.Body)
	}

	gh.err = github.ErrRateLimited
	if rec := b.do(http.MethodGet, "/api/system/github", "", nil); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GitHub failing: %d %s", rec.Code, rec.Body)
	}
}

func TestGitHubAppStatusWithoutApp(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	rec := request(t, h, http.MethodGet, "/api/system/github", "")
	if strings.TrimSpace(rec.Body.String()) != `{"configured":false,"app":null,"installations":[],"installation_accounts":[]}` {
		t.Errorf("no App: %d %s", rec.Code, rec.Body)
	}
}
