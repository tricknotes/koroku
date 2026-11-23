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
		{ID: 1, Account: "octo", AccountID: 1001, RepositorySelection: "selected", Allowed: true},
		{ID: 2, Account: "my-org", AccountID: 2002, RepositorySelection: "all", Allowed: true},
		{ID: 3, Account: "other-org", AccountID: 3003, RepositorySelection: "all", Allowed: true},
		{ID: 4, Account: "stranger", AccountID: 9009, RepositorySelection: "all", Allowed: false},
	}
	srv, _ := newAuthServer(t, gh)
	srv.cfg.GitHub.InstallationAccountIDs = []int64{2002}
	srv.cfg.GitHub.InstallationAccounts = []string{"Other-Org"}
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")

	// other-org is used through its login alone, which login_only flags:
	// its ID is what the operator should list instead.
	rec := b.do(http.MethodGet, "/api/system/github", "", nil)
	want := `{"configured":true,"app":{"slug":"koroku-octo","html_url":"https://github.com/apps/koroku-octo",` +
		`"settings_url":"https://github.com/settings/apps/koroku-octo","install_url":"https://github.com/apps/koroku-octo/installations/new"},` +
		`"installations":[{"account":"octo","account_id":1001,"repository_selection":"selected","used":true,"login_only":false},` +
		`{"account":"my-org","account_id":2002,"repository_selection":"all","used":true,"login_only":false},` +
		`{"account":"other-org","account_id":3003,"repository_selection":"all","used":true,"login_only":true},` +
		`{"account":"stranger","account_id":9009,"repository_selection":"all","used":false,"login_only":false}],` +
		`"installation_account_ids":[2002],"installation_accounts":["Other-Org"]}`
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
	if strings.TrimSpace(rec.Body.String()) != `{"configured":false,"app":null,"installations":[],"installation_account_ids":[],"installation_accounts":[]}` {
		t.Errorf("no App: %d %s", rec.Code, rec.Body)
	}
}
