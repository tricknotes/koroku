package api

import (
	"context"
	"net/http"

	"github.com/tricknotes/koroku/internal/github"
)

// gitHubAppInfo is the part of the GitHub client the system screen uses to
// show the App and where it is installed. The client in production has it;
// fakes in tests may not.
type gitHubAppInfo interface {
	App(ctx context.Context) (github.App, error)
	Installations(ctx context.Context) ([]github.Installation, error)
}

type gitHubAppJSON struct {
	Slug        string `json:"slug"`
	HTMLURL     string `json:"html_url"`
	SettingsURL string `json:"settings_url"`
	InstallURL  string `json:"install_url"`
}

type gitHubInstallationJSON struct {
	Account             string `json:"account"`
	RepositorySelection string `json:"repository_selection"`
	// Used: koroku lists and fetches this installation's repositories
	// (ARCHITECTURE 5).
	Used bool `json:"used"`
}

type gitHubStatusJSON struct {
	Configured    bool                     `json:"configured"`
	App           *gitHubAppJSON           `json:"app"`
	Installations []gitHubInstallationJSON `json:"installations"`
	// InstallationAccounts is [github] installation_accounts as configured.
	InstallationAccounts []string `json:"installation_accounts"`
}

// handleGitHubApp describes the GitHub App and its installations, so that
// the console can show which accounts are used and link to the pages that
// widen them (ARCHITECTURE 5).
func (s *Server) handleGitHubApp(w http.ResponseWriter, r *http.Request) {
	out := gitHubStatusJSON{
		Installations:        []gitHubInstallationJSON{},
		InstallationAccounts: append([]string{}, s.cfg.GitHub.InstallationAccounts...),
	}
	info, ok := s.auth.GitHub.(gitHubAppInfo)
	if s.auth.GitHub == nil || !ok {
		writeJSON(w, http.StatusOK, out)
		return
	}
	out.Configured = true
	ctx, cancel := context.WithTimeout(r.Context(), githubTimeout)
	defer cancel()
	app, err := info.App(ctx)
	if err != nil {
		s.writeGitHubError(w, r, err)
		return
	}
	installs, err := info.Installations(ctx)
	if err != nil {
		s.writeGitHubError(w, r, err)
		return
	}
	out.App = &gitHubAppJSON{
		Slug:        app.Slug,
		HTMLURL:     app.HTMLURL,
		SettingsURL: app.SettingsURL(),
		InstallURL:  app.InstallURL(),
	}
	for _, in := range installs {
		out.Installations = append(out.Installations, gitHubInstallationJSON{
			Account:             in.Account,
			RepositorySelection: in.RepositorySelection,
			Used:                in.Allowed,
		})
	}
	writeJSON(w, http.StatusOK, out)
}
