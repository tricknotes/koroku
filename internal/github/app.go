package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// App describes the GitHub App the client authenticates as. `koroku check`
// compares it with the configuration.
type App struct {
	ID       int64
	Slug     string
	ClientID string
	HTMLURL  string
	// Owner is the login of the account that registered the App, and
	// OwnerIsOrg whether that account is an organization.
	Owner      string
	OwnerIsOrg bool
	// Permissions maps a permission name (contents, metadata, ...) to its
	// level (read, write, admin).
	Permissions map[string]string
}

// Installation is one account the App is installed on.
type Installation struct {
	ID int64
	// Account is the login of the user or organization.
	Account string
	// RepositorySelection is "all" or "selected".
	RepositorySelection string
	// Allowed says whether the client uses this installation
	// (Config.AccountAllowed).
	Allowed bool
}

// App returns the App behind the configured app ID and private key. A key that
// does not belong to the app ID fails with ErrUnauthorized.
func (c *Client) App(ctx context.Context) (App, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return App{}, err
	}
	var resp struct {
		ID          int64             `json:"id"`
		Slug        string            `json:"slug"`
		ClientID    string            `json:"client_id"`
		HTMLURL     string            `json:"html_url"`
		Permissions map[string]string `json:"permissions"`
		Owner       struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"owner"`
	}
	if _, err := c.do(ctx, call{method: http.MethodGet, path: "/app", bearer: jwt}, &resp); err != nil {
		return App{}, fmt.Errorf("github: app: %w", err)
	}
	if resp.ID <= 0 {
		return App{}, errors.New("github: app: response has no id")
	}
	return App{
		ID:          resp.ID,
		Slug:        resp.Slug,
		ClientID:    resp.ClientID,
		HTMLURL:     resp.HTMLURL,
		Owner:       resp.Owner.Login,
		OwnerIsOrg:  resp.Owner.Type == "Organization",
		Permissions: resp.Permissions,
	}, nil
}

// SettingsURL is the App's settings page on GitHub, where its permissions
// and (under Advanced) whether any account may install it are changed. It
// is derived from HTMLURL (https://github.com/apps/<slug>); "" when that is
// not of this form.
func (a App) SettingsURL() string {
	base, ok := strings.CutSuffix(a.HTMLURL, "/apps/"+a.Slug)
	if !ok || a.Slug == "" || a.Owner == "" {
		return ""
	}
	if a.OwnerIsOrg {
		return base + "/organizations/" + url.PathEscape(a.Owner) + "/settings/apps/" + url.PathEscape(a.Slug)
	}
	return base + "/settings/apps/" + url.PathEscape(a.Slug)
}

// InstallURL is the page that installs the App on another account.
func (a App) InstallURL() string {
	if a.HTMLURL == "" {
		return ""
	}
	return a.HTMLURL + "/installations/new"
}

// Installations lists the accounts the App is installed on.
func (c *Client) Installations(ctx context.Context) ([]Installation, error) {
	jwt, err := c.appJWT()
	if err != nil {
		return nil, err
	}
	type apiInstallation struct {
		ID      int64 `json:"id"`
		Account struct {
			ID    int64  `json:"id"`
			Login string `json:"login"`
		} `json:"account"`
		RepositorySelection string `json:"repository_selection"`
	}
	// The endpoint returns a bare JSON array per page.
	list, err := collect(ctx, c, call{method: http.MethodGet, path: "/app/installations", bearer: jwt}, 0,
		func(page *[]apiInstallation) []apiInstallation { return *page })
	if err != nil {
		return nil, fmt.Errorf("github: installations: %w", err)
	}
	out := make([]Installation, 0, len(list))
	for _, in := range list {
		out = append(out, Installation{
			ID:                  in.ID,
			Account:             in.Account.Login,
			RepositorySelection: in.RepositorySelection,
			Allowed:             c.accountAllowed(in.Account.ID, in.Account.Login),
		})
	}
	return out, nil
}

// UserByID looks up a public account by its numeric ID. It needs no
// credentials, so it also works on an anonymous client.
func (c *Client) UserByID(ctx context.Context, id int64) (User, error) {
	if id <= 0 {
		return User{}, fmt.Errorf("%w: user id %d", ErrInvalidArgument, id)
	}
	var resp struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
	}
	if _, err := c.do(ctx, call{method: http.MethodGet, path: "/user/" + strconv.FormatInt(id, 10)}, &resp); err != nil {
		return User{}, fmt.Errorf("github: user %d: %w", id, err)
	}
	return User{ID: resp.ID, Login: resp.Login, AvatarURL: resp.AvatarURL}, nil
}
