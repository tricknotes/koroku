// Package github is koroku's GitHub App client. It covers the two things the
// control plane needs from GitHub: signing the operator in through the web
// application flow, and reading repositories (listing, commit lookup, file
// contents, installation access tokens for git fetch).
//
// The package never logs. Credentials (the private key, app JWTs, the client
// secret, user and installation tokens) are kept out of every error it returns.
package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
)

const (
	defaultAPIBaseURL = "https://api.github.com"
	defaultWebBaseURL = "https://github.com"
	// defaultTimeout bounds one HTTP exchange when the caller does not supply a client.
	defaultTimeout = 30 * time.Second

	acceptAPI  = "application/vnd.github+json"
	apiVersion = "2022-11-28"
	userAgent  = "koroku"

	// maxResponseBytes caps how much of a response body is read into memory.
	maxResponseBytes = 8 << 20
	// perPage is the page size requested from list endpoints (GitHub's maximum).
	perPage = 100
	// maxPages caps how many pages of one listing are followed.
	maxPages = 20
	// maxErrorMessage caps the length of a GitHub error message kept in an APIError.
	maxErrorMessage = 300

	redacted = "[REDACTED]"
)

// Config holds everything the client needs. It is filled by the caller from
// the koroku configuration; this package reads no files and no environment.
type Config struct {
	// AppID is the numeric ID of the GitHub App.
	AppID int64
	// PrivateKey signs the app JWTs. See ParsePrivateKey.
	PrivateKey *rsa.PrivateKey
	// ClientID and ClientSecret belong to the same GitHub App and are used for login.
	ClientID     string
	ClientSecret string
	// APIBaseURL is the REST API root. Defaults to https://api.github.com.
	APIBaseURL string
	// WebBaseURL is the site that serves the OAuth endpoints. Defaults to https://github.com.
	WebBaseURL string
	// HTTP is the client used for every request. Defaults to a client with a timeout.
	HTTP *http.Client
	// Clock supplies the time for JWT claims and token expiry. Defaults to clock.System.
	Clock clock.Clock
	// AccountAllowed decides which installations the client uses, by the
	// account (user or organization) they are on. An App that can be
	// installed on any account can be installed by anyone; repositories of
	// installations it refuses are not listed, and reading them behaves as
	// if the App were not installed there. Nil allows every installation.
	AccountAllowed func(accountID int64, login string) bool
}

// String renders the configuration without its secrets.
func (c Config) String() string {
	return fmt.Sprintf("github.Config{AppID:%d ClientID:%q APIBaseURL:%q WebBaseURL:%q}",
		c.AppID, c.ClientID, c.APIBaseURL, c.WebBaseURL)
}

// GoString keeps %#v from printing the secrets.
func (c Config) GoString() string { return c.String() }

// LogValue keeps structured logs from printing the secrets.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("app_id", c.AppID),
		slog.String("client_id", c.ClientID),
		slog.String("api_base_url", c.APIBaseURL),
		slog.String("web_base_url", c.WebBaseURL),
	)
}

// Client talks to GitHub as one GitHub App. It is safe for concurrent use.
type Client struct {
	appID        int64
	key          *rsa.PrivateKey
	clientID     string
	clientSecret string
	apiBase      string
	webBase      string
	http         *http.Client
	clock        clock.Clock
	allowed      func(accountID int64, login string) bool

	mu            sync.Mutex
	tokens        map[int64]*tokenSlot
	installations map[string]installationEntry
}

// New validates cfg, applies defaults and returns a client.
func New(cfg Config) (*Client, error) {
	if cfg.AppID <= 0 {
		return nil, errors.New("github: app id must be positive")
	}
	if cfg.PrivateKey == nil {
		return nil, errors.New("github: private key is required")
	}
	if cfg.ClientID == "" || cfg.ClientSecret == "" {
		return nil, errors.New("github: client id and client secret are required")
	}
	apiBase, err := normalizeBaseURL(cfg.APIBaseURL, defaultAPIBaseURL)
	if err != nil {
		return nil, fmt.Errorf("github: api base url: %w", err)
	}
	webBase, err := normalizeBaseURL(cfg.WebBaseURL, defaultWebBaseURL)
	if err != nil {
		return nil, fmt.Errorf("github: web base url: %w", err)
	}
	httpClient := cfg.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	clk := cfg.Clock
	if clk == nil {
		clk = clock.System{}
	}
	return &Client{
		appID:         cfg.AppID,
		key:           cfg.PrivateKey,
		clientID:      cfg.ClientID,
		clientSecret:  cfg.ClientSecret,
		apiBase:       apiBase,
		webBase:       webBase,
		http:          httpClient,
		clock:         clk,
		allowed:       cfg.AccountAllowed,
		tokens:        make(map[int64]*tokenSlot),
		installations: make(map[string]installationEntry),
	}, nil
}

// accountAllowed applies Config.AccountAllowed.
func (c *Client) accountAllowed(id int64, login string) bool {
	return c.allowed == nil || c.allowed(id, login)
}

// NewAnonymous returns a client that sends no credentials. It can read
// public repositories (ListDir, ReadFile, within GitHub's limit of 60
// unauthenticated requests an hour per IP); everything that needs the App
// fails with ErrNotConfigured. apiBaseURL defaults to https://api.github.com
// and httpClient to one with a timeout.
func NewAnonymous(apiBaseURL string, httpClient *http.Client) (*Client, error) {
	apiBase, err := normalizeBaseURL(apiBaseURL, defaultAPIBaseURL)
	if err != nil {
		return nil, fmt.Errorf("github: api base url: %w", err)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		apiBase:       apiBase,
		webBase:       defaultWebBaseURL,
		http:          httpClient,
		clock:         clock.System{},
		tokens:        make(map[int64]*tokenSlot),
		installations: make(map[string]installationEntry),
	}, nil
}

// anonymous reports whether the client has no App credentials.
func (c *Client) anonymous() bool { return c.key == nil }

// String identifies the client without exposing its credentials.
func (c *Client) String() string {
	if c == nil {
		return "github.Client(nil)"
	}
	if c.anonymous() {
		return "github.Client{anonymous}"
	}
	return "github.Client{AppID:" + strconv.FormatInt(c.appID, 10) + "}"
}

// GoString keeps %#v from printing the credentials.
func (c *Client) GoString() string { return c.String() }

// LogValue keeps structured logs from printing the credentials.
func (c *Client) LogValue() slog.Value { return slog.StringValue(c.String()) }

// normalizeBaseURL checks raw (or fallback when raw is empty) and returns it
// without a trailing slash.
func normalizeBaseURL(raw, fallback string) (string, error) {
	if raw == "" {
		raw = fallback
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("not a valid URL")
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("must be an http(s) URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("must not contain credentials, a query or a fragment")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

// call describes one HTTP exchange with GitHub.
type call struct {
	method string
	// web sends the request to the web site (OAuth endpoints) instead of the REST API.
	web bool
	// path starts with "/" and is already percent-encoded.
	path  string
	query url.Values
	// bearer is sent as the Authorization header. It is never put into errors.
	bearer string
	// form, when set, is sent as an application/x-www-form-urlencoded body.
	form url.Values
	// secrets lists further values to scrub from error messages.
	secrets []string
}

// do performs in and decodes a 2xx JSON response into out (when out is not nil).
// Any other status becomes an *APIError. The response headers are returned for
// pagination.
func (c *Client) do(ctx context.Context, in call, out any) (http.Header, error) {
	target := c.apiBase + in.path
	if in.web {
		target = c.webBase + in.path
	}
	if len(in.query) > 0 {
		target += "?" + in.query.Encode()
	}
	var body io.Reader
	if in.form != nil {
		body = strings.NewReader(in.form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, in.method, target, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	if in.web {
		req.Header.Set("Accept", "application/json")
	} else {
		req.Header.Set("Accept", acceptAPI)
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
	}
	if in.form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if in.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+in.bearer)
	}

	// Transport errors carry the method and URL only; credentials travel in
	// headers and bodies, never in the URL.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response of %s %s: %w", in.method, in.path, err)
	}
	if len(data) > maxResponseBytes {
		return nil, fmt.Errorf("response of %s %s exceeds %d bytes", in.method, in.path, maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, newAPIError(resp, data, append([]string{in.bearer}, in.secrets...))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			// The decoder's own message is dropped: it may quote parts of a
			// body that holds a token.
			return nil, fmt.Errorf("decode response of %s %s: invalid JSON", in.method, in.path)
		}
	}
	return resp.Header, nil
}

// collect follows a paginated listing and gathers its items. P is the JSON
// shape of one page and items extracts the elements from it. limit > 0 stops
// early; otherwise pages are followed until GitHub reports no next page or
// maxPages is reached.
func collect[P, T any](ctx context.Context, c *Client, in call, limit int, items func(*P) []T) ([]T, error) {
	query := url.Values{}
	for k, v := range in.query {
		query[k] = v
	}
	size := perPage
	if limit > 0 && limit < size {
		size = limit
	}
	query.Set("per_page", strconv.Itoa(size))
	in.query = query

	var all []T
	for page := 1; page <= maxPages; page++ {
		query.Set("page", strconv.Itoa(page))
		var p P
		header, err := c.do(ctx, in, &p)
		if err != nil {
			return nil, err
		}
		all = append(all, items(&p)...)
		if limit > 0 && len(all) >= limit {
			return all[:limit], nil
		}
		if !hasNextPage(header) {
			break
		}
	}
	return all, nil
}

// hasNextPage reports whether the Link header announces a further page. The
// link target itself is not followed; collect builds the next request, so a
// response can never redirect credentials to another host.
func hasNextPage(h http.Header) bool {
	for _, v := range h.Values("Link") {
		for _, part := range strings.Split(v, ",") {
			if strings.Contains(part, `rel="next"`) {
				return true
			}
		}
	}
	return false
}
