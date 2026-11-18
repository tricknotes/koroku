package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/github"
)

const consoleURL = "https://console.dev.example.com"

// Cookie names as sent for an HTTPS console.
const (
	secureSessionCookie = "__Host-" + sessionCookie
	secureStateCookie   = "__Host-" + stateCookie
)

// fakeGitHub plays GitHub for login and repository listing.
type fakeGitHub struct {
	users       map[string]github.User // by user token
	codes       map[string]string      // code -> user token
	exchangeErr error
	redirectURI string
	exchanges   int

	repos    []github.Repo
	branches []github.Branch
	commits  []github.Commit
	files    map[string]string
	err      error
	lastRef  string
}

func (f *fakeGitHub) AuthorizeURL(state, redirectURI string) string {
	return "https://github.example/login/oauth/authorize?state=" + url.QueryEscape(state) +
		"&redirect_uri=" + url.QueryEscape(redirectURI)
}

func (f *fakeGitHub) ExchangeCode(_ context.Context, code, redirectURI string) (string, error) {
	f.exchanges++
	f.redirectURI = redirectURI
	if f.exchangeErr != nil {
		return "", f.exchangeErr
	}
	token, ok := f.codes[code]
	if !ok {
		return "", errors.New("bad_verification_code")
	}
	return token, nil
}

func (f *fakeGitHub) User(_ context.Context, token string) (github.User, error) {
	user, ok := f.users[token]
	if !ok {
		return github.User{}, github.ErrUnauthorized
	}
	return user, nil
}

func (f *fakeGitHub) ListRepos(context.Context) ([]github.Repo, error) { return f.repos, f.err }

func (f *fakeGitHub) ListBranches(context.Context, string, string) ([]github.Branch, error) {
	return f.branches, f.err
}

func (f *fakeGitHub) ListCommits(_ context.Context, _, _, ref string, _ int) ([]github.Commit, error) {
	f.lastRef = ref
	return f.commits, f.err
}

func (f *fakeGitHub) ListDir(context.Context, string, string, string, string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	var names []string
	for name := range f.files {
		if !strings.Contains(name, "/") {
			names = append(names, name)
		}
	}
	return names, nil
}

func (f *fakeGitHub) ReadFile(_ context.Context, _, _, path, _ string) ([]byte, error) {
	content, ok := f.files[path]
	if !ok {
		return nil, github.ErrNotFound
	}
	return []byte(content), nil
}

func newAuthServer(t *testing.T, gh *fakeGitHub) (*Server, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC))
	srv := newTestServer(t, func(o *Options) {
		o.Clock = clk
		o.Auth = AuthOptions{AllowedUserID: 42, ConsoleURL: consoleURL}
		if gh != nil {
			o.Auth.GitHub = gh
		}
	})
	return srv, clk
}

func defaultGitHub() *fakeGitHub {
	return &fakeGitHub{
		codes: map[string]string{"code-owner": "token-owner", "code-stranger": "token-stranger"},
		users: map[string]github.User{
			"token-owner":    {ID: 42, Login: "octo", AvatarURL: "https://example.com/octo.png"},
			"token-stranger": {ID: 7, Login: "stranger"},
		},
	}
}

// browser keeps cookies between requests like a real client.
type browser struct {
	t       *testing.T
	handler http.Handler
	cookies map[string]*http.Cookie
}

func newBrowser(t *testing.T, h http.Handler) *browser {
	return &browser{t: t, handler: h, cookies: map[string]*http.Cookie{}}
}

func (b *browser) do(method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	b.t.Helper()
	var req *http.Request
	if body != "" {
		req = localRequest(method, target, strings.NewReader(body))
	} else {
		req = localRequest(method, target, nil)
	}
	for _, c := range b.cookies {
		req.AddCookie(c)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	b.handler.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c
		}
	}
	return rec
}

// login walks the OAuth flow with the given code and returns the final response.
func (b *browser) login(code string) *httptest.ResponseRecorder {
	b.t.Helper()
	start := b.do(http.MethodGet, "/auth/github/login", "", nil)
	if start.Code != http.StatusSeeOther {
		b.t.Fatalf("login status = %d", start.Code)
	}
	location, err := url.Parse(start.Header().Get("Location"))
	if err != nil {
		b.t.Fatal(err)
	}
	state := location.Query().Get("state")
	if state == "" {
		b.t.Fatalf("no state in %s", location)
	}
	return b.do(http.MethodGet, "/auth/github/callback?code="+code+"&state="+url.QueryEscape(state), "", nil)
}

func TestAPIRequiresLogin(t *testing.T) {
	srv, _ := newAuthServer(t, defaultGitHub())
	h := srv.Handler()

	for _, target := range []string{"/api/apps", "/api/system", "/api/repos", "/api/apps/blog/env", "/api/deployments/1/logs", "/api/nope"} {
		rec := request(t, h, http.MethodGet, target, "")
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "unauthenticated") {
			t.Errorf("GET %s without a session: %d %s", target, rec.Code, rec.Body)
		}
	}
	// The session endpoint and the SPA are reachable so that the login page can render.
	session := request(t, h, http.MethodGet, "/api/session", "")
	if session.Code != http.StatusOK || strings.TrimSpace(session.Body.String()) != `{"authenticated":false}` {
		t.Errorf("session: %d %s", session.Code, session.Body)
	}
	if rec := request(t, h, http.MethodGet, "/", ""); rec.Code != http.StatusOK {
		t.Errorf("SPA status = %d", rec.Code)
	}
	// A made-up cookie is not a session.
	b := newBrowser(t, h)
	b.cookies[secureSessionCookie] = &http.Cookie{Name: secureSessionCookie, Value: "forged"}
	if rec := b.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged cookie status = %d", rec.Code)
	}
}

func TestLoginAsAllowedUser(t *testing.T) {
	gh := defaultGitHub()
	srv, _ := newAuthServer(t, gh)
	b := newBrowser(t, srv.Handler())

	final := b.login("code-owner")
	if final.Code != http.StatusSeeOther || final.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d -> %q", final.Code, final.Header().Get("Location"))
	}
	if gh.redirectURI != consoleURL+"/auth/github/callback" {
		t.Errorf("redirect URI = %q", gh.redirectURI)
	}
	cookie := b.cookies[secureSessionCookie]
	if cookie == nil {
		t.Fatal("no session cookie")
	}
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Errorf("cookie attributes = %+v", cookie)
	}
	if _, ok := b.cookies[secureStateCookie]; ok {
		t.Error("the state cookie must be cleared after the callback")
	}

	session := b.do(http.MethodGet, "/api/session", "", nil)
	want := `{"authenticated":true,"user":{"avatar_url":"https://example.com/octo.png","id":42,"login":"octo"}}`
	if strings.TrimSpace(session.Body.String()) != want {
		t.Errorf("session = %s", session.Body)
	}
	if rec := b.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusOK {
		t.Errorf("authenticated request status = %d", rec.Code)
	}

	// Neither the cookie value nor the GitHub token is stored.
	var hash string
	if err := srv.store.DB().QueryRow(`SELECT token_hash FROM sessions`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == cookie.Value || hash != hashToken(cookie.Value) {
		t.Error("the session must be stored as a hash of the cookie value")
	}
	var dump string
	_ = srv.store.DB().QueryRow(`SELECT group_concat(token_hash || login || avatar_url) FROM sessions`).Scan(&dump)
	if strings.Contains(dump, "token-owner") {
		t.Error("the GitHub token was stored")
	}
}

func TestLoginRejectsOtherUsers(t *testing.T) {
	srv, _ := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())

	final := b.login("code-stranger")
	if final.Header().Get("Location") != "/?login_error=not_allowed" {
		t.Fatalf("Location = %q", final.Header().Get("Location"))
	}
	if _, ok := b.cookies[secureSessionCookie]; ok {
		t.Error("a rejected user got a session cookie")
	}
	if rec := b.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rec.Code)
	}
	var count int
	_ = srv.store.DB().QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count)
	if count != 0 {
		t.Errorf("sessions = %d", count)
	}
}

func TestLoginWithoutAllowedUserConfiguredRejectsEveryone(t *testing.T) {
	gh := defaultGitHub()
	gh.users["token-zero"] = github.User{ID: 0, Login: "ghost"}
	gh.codes["code-zero"] = "token-zero"
	srv := newTestServer(t, func(o *Options) {
		o.Auth = AuthOptions{GitHub: gh, ConsoleURL: consoleURL} // AllowedUserID left at zero
	})
	b := newBrowser(t, srv.Handler())
	for _, code := range []string{"code-owner", "code-zero"} {
		if final := b.login(code); final.Header().Get("Location") != "/?login_error=not_allowed" {
			t.Errorf("code %s: Location = %q", code, final.Header().Get("Location"))
		}
	}
}

func TestCallbackStateAndErrors(t *testing.T) {
	gh := defaultGitHub()
	srv, _ := newAuthServer(t, gh)
	h := srv.Handler()

	cases := map[string]struct {
		prepare func(b *browser) string // returns the callback target
		want    string
	}{
		"no state cookie": {func(*browser) string { return "/auth/github/callback?code=code-owner&state=abc" }, "state_mismatch"},
		"wrong state": {func(b *browser) string {
			b.do(http.MethodGet, "/auth/github/login", "", nil)
			return "/auth/github/callback?code=code-owner&state=attacker"
		}, "state_mismatch"},
		"empty state": {func(b *browser) string {
			b.do(http.MethodGet, "/auth/github/login", "", nil)
			return "/auth/github/callback?code=code-owner"
		}, "state_mismatch"},
		"user declined": {func(b *browser) string {
			b.do(http.MethodGet, "/auth/github/login", "", nil)
			return "/auth/github/callback?error=access_denied&state=" + url.QueryEscape(b.cookies[secureStateCookie].Value)
		}, "github_error"},
		"bad code": {func(b *browser) string {
			b.do(http.MethodGet, "/auth/github/login", "", nil)
			return "/auth/github/callback?code=expired&state=" + url.QueryEscape(b.cookies[secureStateCookie].Value)
		}, "github_error"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			b := newBrowser(t, h)
			rec := b.do(http.MethodGet, tc.prepare(b), "", nil)
			if got := rec.Header().Get("Location"); got != "/?login_error="+tc.want {
				t.Errorf("Location = %q, want login_error=%s", got, tc.want)
			}
			if _, ok := b.cookies[secureSessionCookie]; ok {
				t.Error("a session was created")
			}
		})
	}

	// A state value is single use.
	b := newBrowser(t, h)
	b.do(http.MethodGet, "/auth/github/login", "", nil)
	state := b.cookies[secureStateCookie].Value
	b.do(http.MethodGet, "/auth/github/callback?code=code-owner&state="+url.QueryEscape(state), "", nil)
	replay := b.do(http.MethodGet, "/auth/github/callback?code=code-owner&state="+url.QueryEscape(state), "", nil)
	if replay.Header().Get("Location") != "/?login_error=state_mismatch" {
		t.Errorf("replayed state: Location = %q", replay.Header().Get("Location"))
	}
}

func TestLoginNotConfigured(t *testing.T) {
	srv, _ := newAuthServer(t, nil)
	h := srv.Handler()
	for _, target := range []string{"/auth/github/login", "/auth/github/callback?code=x&state=y"} {
		rec := request(t, h, http.MethodGet, target, "")
		if rec.Header().Get("Location") != "/?login_error=not_configured" {
			t.Errorf("%s: Location = %q", target, rec.Header().Get("Location"))
		}
	}
	// Without login the API stays locked rather than open.
	if rec := request(t, h, http.MethodGet, "/api/apps", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestSessionExpiresAndLogout(t *testing.T) {
	srv, clk := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")

	clk.Advance(SessionLifetime - time.Minute)
	if rec := b.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusOK {
		t.Errorf("just before expiry: %d", rec.Code)
	}
	clk.Advance(2 * time.Minute)
	if rec := b.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("after expiry: %d", rec.Code)
	}

	b.login("code-owner")
	token := b.cookies[secureSessionCookie].Value
	logout := b.do(http.MethodPost, "/auth/logout", "", map[string]string{"Origin": consoleURL})
	if logout.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, body = %s", logout.Code, logout.Body)
	}
	if _, ok := b.cookies[secureSessionCookie]; ok {
		t.Error("the session cookie was not cleared")
	}
	// The old cookie value is dead on the server too.
	b.cookies[secureSessionCookie] = &http.Cookie{Name: secureSessionCookie, Value: token}
	if rec := b.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a logged-out session still works: %d", rec.Code)
	}
}

func TestMutatingRequestsNeedTheConsoleOrigin(t *testing.T) {
	srv, _ := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")
	body := `{"slug": "blog", "repo_full_name": "octo/blog", "web_port": 3000}`

	for name, origin := range map[string]string{
		"missing":     "",
		"other site":  "https://evil.example",
		"http scheme": "http://console.dev.example.com",
		"suffix":      consoleURL + ".evil.example",
		"null":        "null",
	} {
		headers := map[string]string{}
		if origin != "" {
			headers["Origin"] = origin
		}
		rec := b.do(http.MethodPost, "/api/apps", body, headers)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "forbidden_origin") {
			t.Errorf("origin %s: %d %s", name, rec.Code, rec.Body)
		}
	}
	list := b.do(http.MethodGet, "/api/apps", "", nil)
	if !strings.Contains(list.Body.String(), `"apps":[]`) {
		t.Errorf("a rejected request created an app: %s", list.Body)
	}

	ok := b.do(http.MethodPost, "/api/apps", body, map[string]string{"Origin": consoleURL})
	if ok.Code != http.StatusCreated {
		t.Fatalf("same-origin POST: %d %s", ok.Code, ok.Body)
	}
	for _, method := range []string{http.MethodPatch, http.MethodPut, http.MethodDelete} {
		rec := b.do(method, "/api/apps/blog", `{}`, map[string]string{"Origin": "https://evil.example"})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s from another origin: %d", method, rec.Code)
		}
	}
	// Logout is protected the same way.
	if rec := b.do(http.MethodPost, "/auth/logout", "", nil); rec.Code != http.StatusForbidden {
		t.Errorf("logout without Origin: %d", rec.Code)
	}
	// Reads do not need an Origin header.
	if rec := b.do(http.MethodGet, "/api/apps/blog", "", nil); rec.Code != http.StatusOK {
		t.Errorf("GET status = %d", rec.Code)
	}
}

func TestChangingTheAllowedUserInvalidatesSessions(t *testing.T) {
	gh := defaultGitHub()
	srv, _ := newAuthServer(t, gh)
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")

	srv.auth.AllowedUserID = 99
	if rec := b.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d", rec.Code)
	}
}

func TestInsecureNoAuth(t *testing.T) {
	h := newTestServer(t, nil).Handler() // newTestServer sets InsecureNoAuth
	session := request(t, h, http.MethodGet, "/api/session", "")
	if !strings.Contains(session.Body.String(), `"authenticated":true`) || !strings.Contains(session.Body.String(), `"login":"local"`) {
		t.Errorf("session = %s", session.Body)
	}
}

func TestSecurityHeadersAndCookieOverHTTP(t *testing.T) {
	gh := defaultGitHub()
	srv := newTestServer(t, func(o *Options) {
		o.Auth = AuthOptions{GitHub: gh, AllowedUserID: 42, ConsoleURL: "http://localhost:8080"}
	})
	b := newBrowser(t, srv.Handler())
	rec := b.do(http.MethodGet, "/", "", nil)
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "same-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q", header, got)
		}
	}
	// A console served over plain HTTP (local development) cannot use Secure
	// cookies, and therefore not the __Host- prefix either.
	b.login("code-owner")
	if cookie := b.cookies[sessionCookie]; cookie == nil || cookie.Secure {
		t.Errorf("cookie = %+v (all cookies: %v)", cookie, b.cookies)
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be sent for a plain HTTP console")
	}
	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("Content-Security-Policy lacks %q: %q", want, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe") {
		t.Errorf("scripts must not allow inline or eval: %q", csp)
	}
}

func TestRepoEndpoints(t *testing.T) {
	gh := defaultGitHub()
	gh.repos = []github.Repo{{ID: 1, FullName: "octo/blog", Owner: "octo", Name: "blog", Private: true, DefaultBranch: "main", InstallationID: 9}}
	gh.branches = []github.Branch{{Name: "main", SHA: testSHA}, {Name: "feature/login", SHA: testSHA}}
	gh.commits = []github.Commit{{SHA: testSHA, Message: "Fix login\n\nLong body.", Author: "octo", Date: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}}
	gh.files = map[string]string{
		"README.md":          "hello",
		"docker-compose.yml": "services:\n  app:\n    image: nginx\n",
		"compose.yaml":       "services:\n  web:\n    build: .\n    ports:\n      - \"3000:3000\"\n  db:\n    image: postgres:17\n",
		"deploy/prod.yaml":   "services:\n  api:\n    image: example/api\n    expose: [4000]\n",
	}
	srv, _ := newAuthServer(t, gh)
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")

	repos := b.do(http.MethodGet, "/api/repos", "", nil)
	if strings.TrimSpace(repos.Body.String()) != `{"repos":[{"default_branch":"main","full_name":"octo/blog","id":1,"private":true}]}` {
		t.Errorf("repos = %s", repos.Body)
	}
	branches := b.do(http.MethodGet, "/api/repos/octo/blog/branches", "", nil)
	if !strings.Contains(branches.Body.String(), `{"name":"feature/login","sha":"`+testSHA+`"}`) {
		t.Errorf("branches = %s", branches.Body)
	}
	commits := b.do(http.MethodGet, "/api/repos/octo/blog/commits?ref=feature/login", "", nil)
	if gh.lastRef != "feature/login" || !strings.Contains(commits.Body.String(), `"message":"Fix login"`) ||
		strings.Contains(commits.Body.String(), "Long body") {
		t.Errorf("commits (ref %q) = %s", gh.lastRef, commits.Body)
	}

	// compose.yaml wins over docker-compose.yml; services and ports are prefilled.
	detected := b.do(http.MethodGet, "/api/repos/octo/blog/compose?ref=main", "", nil)
	want := `{"files":["compose.yaml","docker-compose.yml"],"path":"compose.yaml","services":[{"name":"web","ports":[3000]},{"name":"db","ports":[]}]}`
	if strings.TrimSpace(detected.Body.String()) != want {
		t.Errorf("compose = %s", detected.Body)
	}
	custom := b.do(http.MethodGet, "/api/repos/octo/blog/compose?path=deploy/prod.yaml", "", nil)
	if !strings.Contains(custom.Body.String(), `"path":"deploy/prod.yaml","services":[{"name":"api","ports":[4000]}]`) {
		t.Errorf("custom path = %s", custom.Body)
	}
	for _, bad := range []string{"../secrets.yaml", "/etc/passwd"} {
		rec := b.do(http.MethodGet, "/api/repos/octo/blog/compose?path="+url.QueryEscape(bad), "", nil)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("path %q: status = %d", bad, rec.Code)
		}
	}
	if rec := b.do(http.MethodGet, "/api/repos/octo/blog/compose?path=missing.yaml", "", nil); rec.Code != http.StatusNotFound {
		t.Errorf("missing file status = %d", rec.Code)
	}

	gh.files = map[string]string{"README.md": "hello"}
	none := b.do(http.MethodGet, "/api/repos/octo/blog/compose", "", nil)
	if strings.TrimSpace(none.Body.String()) != `{"files":[],"path":"","services":[]}` {
		t.Errorf("no compose file = %s", none.Body)
	}
}

func TestRepoEndpointErrors(t *testing.T) {
	gh := defaultGitHub()
	srv, _ := newAuthServer(t, gh)
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")

	cases := map[string]struct {
		err    error
		status int
		code   string
	}{
		"not found":    {github.ErrNotFound, http.StatusNotFound, "repo_not_found"},
		"rate limited": {github.ErrRateLimited, http.StatusServiceUnavailable, "github_rate_limited"},
		"invalid":      {github.ErrInvalidArgument, http.StatusUnprocessableEntity, "validation_failed"},
		"other":        {fmt.Errorf("dial tcp: secret-internal-detail"), http.StatusBadGateway, "github_error"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gh.err = tc.err
			for _, target := range []string{"/api/repos", "/api/repos/octo/blog/branches", "/api/repos/octo/blog/commits", "/api/repos/octo/blog/compose"} {
				rec := b.do(http.MethodGet, target, "", nil)
				if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.code) {
					t.Errorf("%s: %d %s", target, rec.Code, rec.Body)
				}
				if strings.Contains(rec.Body.String(), "secret-internal-detail") {
					t.Errorf("%s leaks upstream details: %s", target, rec.Body)
				}
			}
		})
	}
}

func TestRepoEndpointsWithoutGitHub(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	for _, target := range []string{"/api/repos", "/api/repos/octo/blog/branches", "/api/repos/octo/blog/commits", "/api/repos/octo/blog/compose"} {
		rec := request(t, h, http.MethodGet, target, "")
		if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not_configured") {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body)
		}
	}
}

// TestSessionCookieCannotBePlantedByASiblingApp: hosted apps live on sibling
// subdomains and can set cookies for the parent domain. Only the __Host-
// cookie counts as a session, so a planted "paas_session" is ignored even
// when its value is a valid token.
func TestSessionCookieCannotBePlantedByASiblingApp(t *testing.T) {
	srv, _ := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")
	valid := b.cookies[secureSessionCookie]
	if valid == nil || !strings.HasPrefix(valid.Name, "__Host-") || valid.Domain != "" || valid.Path != "/" || !valid.Secure {
		t.Fatalf("session cookie = %+v", valid)
	}

	attacker := newBrowser(t, srv.Handler())
	attacker.cookies[sessionCookie] = &http.Cookie{Name: sessionCookie, Value: valid.Value}
	if rec := attacker.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unprefixed cookie was accepted as a session: %d", rec.Code)
	}

	// The same holds for the OAuth state: a planted unprefixed state cookie
	// does not let a forged callback through.
	forged := newBrowser(t, srv.Handler())
	forged.cookies[stateCookie] = &http.Cookie{Name: stateCookie, Value: "attacker-state"}
	rec := forged.do(http.MethodGet, "/auth/github/callback?code=code-owner&state=attacker-state", "", nil)
	if rec.Header().Get("Location") != "/?login_error=state_mismatch" {
		t.Errorf("forged state: Location = %q", rec.Header().Get("Location"))
	}

	// HTTPS consoles announce HSTS.
	if got := b.do(http.MethodGet, "/", "", nil).Header().Get("Strict-Transport-Security"); !strings.HasPrefix(got, "max-age=") {
		t.Errorf("Strict-Transport-Security = %q", got)
	}
}
