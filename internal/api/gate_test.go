package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/store"
)

const (
	stagingHost = "shop-staging.dev.example.com"
	otherHost   = "blog-staging.dev.example.com"
	customHost  = "staging.example.org"
	publicHost  = "shop.dev.example.com"
)

// gateFixture is a server with a logged-in console browser and three apps:
// two protected staging apps (one with an active custom domain) and an
// unprotected production app.
type gateFixture struct {
	t       *testing.T
	srv     *Server
	h       http.Handler
	clk     *clock.Fake
	console *browser
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	srv, clk := newAuthServer(t, defaultGitHub())
	f := &gateFixture{t: t, srv: srv, h: srv.Handler(), clk: clk}
	ctx := context.Background()
	mk := func(slug string, protected bool, env string) store.App {
		app, err := srv.store.CreateApp(ctx, store.App{
			Slug: slug, RepoFullName: "octo/" + slug, DefaultBranch: "main", WebService: "web",
			WebPort: 3000, HealthcheckPath: "/", Environment: env, AccessProtection: protected,
		}, 20000, 20999, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		return app
	}
	staging := mk("shop-staging", true, store.EnvironmentStaging)
	mk("blog-staging", true, store.EnvironmentStaging)
	mk("shop", false, store.EnvironmentProduction)
	d, err := srv.store.AddDomain(ctx, staging.ID, customHost, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.RecordDomainCheck(ctx, d.ID, true, "", clk.Now()); err != nil {
		t.Fatal(err)
	}
	f.console = newBrowser(t, f.h)
	if rec := f.console.login("code-owner"); rec.Code != http.StatusSeeOther {
		t.Fatalf("login: %d", rec.Code)
	}
	return f
}

// onHost sends a request as Caddy forwards it from an app host.
func (f *gateFixture) onHost(host, method, target string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	req.RemoteAddr = "127.0.0.1:50000"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return send(f.h, req)
}

// check asks the gate the way Caddy's forward_auth does.
func (f *gateFixture) check(host, method, uri string, cookie *http.Cookie) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.onHost(host, http.MethodGet, caddy.GateCheckPath, cookie,
		map[string]string{"X-Forwarded-Method": method, "X-Forwarded-Uri": uri})
}

// enter walks the redirect chain for host and returns the gate cookie.
func (f *gateFixture) enter(host, back string) *http.Cookie {
	f.t.Helper()
	callback := f.startGate(host, back)
	rec := f.onHost(host, http.MethodGet, callback.RequestURI(), nil, nil)
	if rec.Code != http.StatusSeeOther {
		f.t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	if loc := rec.Header().Get("Location"); loc != back {
		f.t.Errorf("callback redirects to %q, want %q", loc, back)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == caddy.GateCookie {
			return c
		}
	}
	f.t.Fatalf("no gate cookie in %v", rec.Header())
	return nil
}

// startGate asks the console for a code and returns the callback URL.
func (f *gateFixture) startGate(host, back string) *url.URL {
	f.t.Helper()
	rec := f.console.do(http.MethodGet, "/auth/gate?"+url.Values{"host": {host}, "return": {back}}.Encode(), "", nil)
	if rec.Code != http.StatusSeeOther {
		f.t.Fatalf("/auth/gate: %d %s", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		f.t.Fatal(err)
	}
	if loc.Scheme != "https" || loc.Host != host || loc.Path != "/.koroku/auth/callback" || loc.Query().Get("code") == "" {
		f.t.Fatalf("/auth/gate redirects to %s", loc)
	}
	return loc
}

func TestGateFlow(t *testing.T) {
	f := newGateFixture(t)

	// Without a cookie, a page load goes to the console; other methods get 401.
	rec := f.check(stagingHost, http.MethodGet, "/orders?page=2", nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("check without cookie: %d", rec.Code)
	}
	want := consoleURL + "/auth/gate?" + url.Values{"host": {stagingHost}, "return": {"/orders?page=2"}}.Encode()
	if got := rec.Header().Get("Location"); got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if rec := f.check(stagingHost, http.MethodPost, "/orders", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST without cookie: %d", rec.Code)
	}

	cookie := f.enter(stagingHost, "/orders?page=2")
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" || cookie.Domain != "" {
		t.Errorf("gate cookie attributes: %+v", cookie)
	}
	if !strings.HasPrefix(cookie.Name, "__Host-") {
		t.Errorf("gate cookie name %q lacks __Host-", cookie.Name)
	}
	if cookie.MaxAge <= 0 || time.Duration(cookie.MaxAge)*time.Second > SessionLifetime {
		t.Errorf("gate cookie lives %ds, beyond the session", cookie.MaxAge)
	}

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if rec := f.check(stagingHost, method, "/orders", cookie); rec.Code != http.StatusNoContent {
			t.Errorf("%s with the cookie: %d %s", method, rec.Code, rec.Body)
		}
	}
	// Only for the hostname it was issued on: not the other protected app,
	// and not even another hostname of the same app.
	for _, host := range []string{otherHost, customHost} {
		if rec := f.check(host, http.MethodGet, "/", cookie); rec.Code != http.StatusFound {
			t.Errorf("cookie of %s accepted on %s: %d", stagingHost, host, rec.Code)
		}
	}
	// A custom domain of a protected app goes through the same flow.
	custom := f.enter(customHost, "/")
	if rec := f.check(customHost, http.MethodGet, "/", custom); rec.Code != http.StatusNoContent {
		t.Errorf("custom domain with its cookie: %d", rec.Code)
	}

	// Logging out of the host forgets that grant only.
	if rec := f.onHost(stagingHost, http.MethodGet, "/.koroku/auth/logout", cookie, nil); rec.Code != http.StatusOK {
		t.Errorf("logout: %d", rec.Code)
	}
	if rec := f.check(stagingHost, http.MethodGet, "/", cookie); rec.Code != http.StatusFound {
		t.Errorf("cookie still valid after the host logout: %d", rec.Code)
	}
	if rec := f.check(customHost, http.MethodGet, "/", custom); rec.Code != http.StatusNoContent {
		t.Errorf("host logout ended the grant of another host: %d", rec.Code)
	}
}

func TestGateEndsWithTheConsoleSession(t *testing.T) {
	cases := map[string]func(f *gateFixture){
		"console logout": func(f *gateFixture) {
			rec := f.console.do(http.MethodPost, "/auth/logout", "", map[string]string{"Origin": consoleURL})
			if rec.Code != http.StatusNoContent {
				f.t.Fatalf("logout: %d", rec.Code)
			}
		},
		"session revoked": func(f *gateFixture) {
			sessions, err := f.srv.store.ListSessions(context.Background(), 42, f.clk.Now())
			if err != nil || len(sessions) != 1 {
				f.t.Fatalf("sessions: %v %v", sessions, err)
			}
			if err := f.srv.store.DeleteSessionByID(context.Background(), 42, sessions[0].ID); err != nil {
				f.t.Fatal(err)
			}
		},
		"session expired": func(f *gateFixture) { f.clk.Advance(SessionLifetime + time.Second) },
		"allowed user changed": func(f *gateFixture) {
			f.srv.auth.AllowedUserID = 43
		},
	}
	for name, end := range cases {
		t.Run(name, func(t *testing.T) {
			f := newGateFixture(t)
			cookie := f.enter(stagingHost, "/")
			if rec := f.check(stagingHost, http.MethodGet, "/", cookie); rec.Code != http.StatusNoContent {
				t.Fatalf("before: %d", rec.Code)
			}
			end(f)
			if rec := f.check(stagingHost, http.MethodGet, "/", cookie); rec.Code != http.StatusFound {
				t.Errorf("after %s: %d", name, rec.Code)
			}
		})
	}
}

func TestGateCodeIsOneTime(t *testing.T) {
	f := newGateFixture(t)
	callback := f.startGate(stagingHost, "/")
	if rec := f.onHost(stagingHost, http.MethodGet, callback.RequestURI(), nil, nil); rec.Code != http.StatusSeeOther {
		t.Fatalf("first use: %d", rec.Code)
	}
	rec := f.onHost(stagingHost, http.MethodGet, callback.RequestURI(), nil, nil)
	if rec.Code != http.StatusBadRequest || len(rec.Result().Cookies()) != 0 {
		t.Errorf("second use: %d %v", rec.Code, rec.Result().Cookies())
	}
	if !strings.Contains(rec.Body.String(), "/auth/gate?") {
		t.Errorf("the error page offers no way to retry: %s", rec.Body)
	}

	// A code is bound to its host.
	callback = f.startGate(stagingHost, "/")
	if rec := f.onHost(otherHost, http.MethodGet, callback.RequestURI(), nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("code redeemed on another host: %d", rec.Code)
	}

	// A code expires after a minute.
	callback = f.startGate(stagingHost, "/")
	f.clk.Advance(gateCodeLifetime + time.Second)
	if rec := f.onHost(stagingHost, http.MethodGet, callback.RequestURI(), nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("expired code: %d", rec.Code)
	}

	// A code dies with the session it came from.
	callback = f.startGate(stagingHost, "/")
	f.console.do(http.MethodPost, "/auth/logout", "", map[string]string{"Origin": consoleURL})
	if rec := f.onHost(stagingHost, http.MethodGet, callback.RequestURI(), nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("code of a logged-out session: %d", rec.Code)
	}

	for _, target := range []string{"/.koroku/auth/callback", "/.koroku/auth/callback?code=made-up"} {
		if rec := f.onHost(stagingHost, http.MethodGet, target, nil, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", target, rec.Code)
		}
	}
}

func TestGateRefusesOpenRedirects(t *testing.T) {
	f := newGateFixture(t)
	for _, host := range []string{"evil.example", publicHost, "console.dev.example.com", "", "nope.dev.example.com", "pending.example.org"} {
		rec := f.console.do(http.MethodGet, "/auth/gate?"+url.Values{"host": {host}, "return": {"/"}}.Encode(), "", nil)
		if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
			t.Errorf("host %q: %d Location=%q", host, rec.Code, rec.Header().Get("Location"))
		}
	}
	// Return paths are kept on the app host.
	for back, want := range map[string]string{
		"/ok?x=1":                       "/ok?x=1",
		"//evil.example/":               "/",
		"https://evil.example/":         "/",
		"/\\evil.example":               "/",
		"javascript:alert(1)":           "/",
		"/" + strings.Repeat("a", 3000): "/",
	} {
		callback := f.startGate(stagingHost, back)
		if got := callback.Query().Get("return"); got != want {
			t.Errorf("return %q became %q, want %q", back, got, want)
		}
		rec := f.onHost(stagingHost, http.MethodGet, "/.koroku/auth/callback?"+url.Values{
			"code": {callback.Query().Get("code")}, "return": {back},
		}.Encode(), nil, nil)
		if got := rec.Header().Get("Location"); got != want {
			t.Errorf("callback with return %q redirects to %q, want %q", back, got, want)
		}
	}
	// The forwarded URI of the check is sanitized too.
	rec := f.check(stagingHost, http.MethodGet, "//evil.example/", nil)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc == nil || loc.Query().Get("return") != "/" {
		t.Errorf("check redirect = %q", rec.Header().Get("Location"))
	}
}

func TestGateNeedsAConsoleLogin(t *testing.T) {
	f := newGateFixture(t)
	stranger := newBrowser(t, f.h)
	target := "/auth/gate?" + url.Values{"host": {stagingHost}, "return": {"/x"}}.Encode()
	rec := stranger.do(http.MethodGet, target, "", nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("without a session: %d", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Path != "/auth/github/login" || loc.Query().Get("return_to") != target {
		t.Errorf("Location = %q", rec.Header().Get("Location"))
	}
	// The login comes back to the gate.
	login := stranger.do(http.MethodGet, loc.RequestURI(), "", nil)
	state, _ := url.Parse(login.Header().Get("Location"))
	back := stranger.do(http.MethodGet, "/auth/github/callback?code=code-owner&state="+url.QueryEscape(state.Query().Get("state")), "", nil)
	if got := back.Header().Get("Location"); got != target {
		t.Errorf("after login: %q, want %q", got, target)
	}
}

func TestGateCheckForOtherHosts(t *testing.T) {
	f := newGateFixture(t)
	// An unprotected app passes; Caddy normally does not ask for it.
	if rec := f.check(publicHost, http.MethodGet, "/", nil); rec.Code != http.StatusNoContent {
		t.Errorf("unprotected app: %d", rec.Code)
	}
	for _, host := range []string{"unknown.example", "nope.dev.example.com"} {
		if rec := f.check(host, http.MethodGet, "/", nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s: %d", host, rec.Code)
		}
	}
	// The gate paths do not exist on the console host.
	for _, target := range []string{caddy.GateCheckPath, "/.koroku/auth/callback?code=x", "/.koroku/auth/logout"} {
		req := localRequest(http.MethodGet, target, nil)
		req.Host = "console.dev.example.com"
		if rec := send(f.h, req); rec.Code != http.StatusNotFound {
			t.Errorf("console %s: %d", target, rec.Code)
		}
	}
	// Nothing else of koroku answers on an app host.
	for _, target := range []string{"/api/apps", "/api/session", "/auth/gate?host=" + stagingHost, "/auth/github/login", "/"} {
		if rec := f.onHost(stagingHost, http.MethodGet, target, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("app host %s: %d", target, rec.Code)
		}
	}
	// The callback does nothing for an unprotected app.
	if rec := f.onHost(publicHost, http.MethodGet, "/.koroku/auth/callback?code=x", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("callback on an unprotected app: %d", rec.Code)
	}
}

// Caddy routes a request by its decoded, cleaned, lower-cased path and
// forwards it as the client sent it. Whatever Caddy takes for a gate path is
// one here too: another spelling is not found, on any hostname, and is never
// handed to the console or answered by a gate handler.
func TestGatePathsInAnotherSpellingAreNotFound(t *testing.T) {
	f := newGateFixture(t)
	cookie := f.enter(stagingHost, "/")
	for _, host := range []string{stagingHost, customHost, publicHost, "console.dev.example.com", "unknown.example"} {
		for _, target := range []string{
			"/graphql/../.koroku/auth/callback?code=x",
			"/graphql/..%2F.koroku/auth/logout",
			"/%2e%2e/.koroku/auth/logout",
			"/.KOROKU/auth/logout",
			"/.koroku/Auth/Logout",
			"//.koroku/auth/logout",
			"/.koroku/./auth/logout",
			"/.koroku",
			"/.koroku/",
			"/x/../.koroku",
			"/AUTH/GATE/CHECK",
			"/auth/gate/./check",
			"/auth//gate/check",
		} {
			rec := f.onHost(host, http.MethodGet, target, cookie, map[string]string{"X-Forwarded-Method": http.MethodGet})
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s%s: %d %s", host, target, rec.Code, rec.Body)
			}
			if len(rec.Result().Cookies()) != 0 {
				t.Errorf("%s%s was answered by a gate handler: %v", host, target, rec.Header())
			}
		}
	}
	// The grant is untouched: no spelling of the logout path ended it.
	if rec := f.check(stagingHost, http.MethodGet, "/", cookie); rec.Code != http.StatusNoContent {
		t.Errorf("check after the requests: %d", rec.Code)
	}
}

func TestGateInsecureNoAuthAlwaysPasses(t *testing.T) {
	srv := newTestServer(t, nil) // InsecureNoAuth
	h := srv.Handler()
	if _, err := srv.store.CreateApp(context.Background(), store.App{
		Slug: "shop-staging", RepoFullName: "octo/shop", DefaultBranch: "main", WebService: "web",
		WebPort: 3000, HealthcheckPath: "/", Environment: store.EnvironmentStaging, AccessProtection: true,
	}, 20000, 20999, time.Now()); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, caddy.GateCheckPath, nil)
	req.Host = stagingHost
	req.Header.Set("X-Forwarded-Method", http.MethodPost)
	if rec := send(h, req); rec.Code != http.StatusNoContent {
		t.Errorf("check: %d", rec.Code)
	}
	start := localRequest(http.MethodGet, "/auth/gate?host="+stagingHost+"&return=/x", nil)
	rec := send(h, start)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://"+stagingHost+"/x" {
		t.Errorf("gate start: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	evil := localRequest(http.MethodGet, "/auth/gate?host=evil.example&return=/x", nil)
	if rec := send(h, evil); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown host in no-auth mode: %d", rec.Code)
	}
}

func TestAppAccessProtectionSettings(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	create := func(body string) appJSON {
		t.Helper()
		rec := request(t, h, http.MethodPost, "/api/apps", body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
		return decode[appJSON](t, rec)
	}
	if app := create(`{"slug": "a", "repo_full_name": "octo/a", "web_port": 3000}`); !app.AccessProtection || len(app.ProtectionExempt) != 0 {
		t.Errorf("staging default: %+v", app)
	}
	if app := create(`{"slug": "b", "repo_full_name": "octo/b", "web_port": 3000, "environment": "production"}`); app.AccessProtection {
		t.Error("production apps must start unprotected")
	}
	if app := create(`{"slug": "c", "repo_full_name": "octo/c", "web_port": 3000, "access_protection": false}`); app.AccessProtection {
		t.Error("an explicit false was ignored")
	}

	rec := request(t, h, http.MethodPatch, "/api/apps/a", `{"protection_exempt_paths": [" /webhooks/ ", "/webhooks/", "", "/api/hook"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	if got := decode[appJSON](t, rec).ProtectionExempt; strings.Join(got, ",") != "/webhooks/,/api/hook" {
		t.Errorf("exempt paths = %v", got)
	}
	// The environment no longer drives the flag once the app exists.
	if rec := request(t, h, http.MethodPatch, "/api/apps/a", `{"environment": "production"}`); !decode[appJSON](t, rec).AccessProtection {
		t.Error("changing the environment changed the protection")
	}

	for _, body := range []string{
		`{"protection_exempt_paths": ["/"]}`,
		`{"protection_exempt_paths": ["webhooks"]}`,
		`{"protection_exempt_paths": ["/hooks/*"]}`,
		`{"protection_exempt_paths": ["/.koroku/auth/"]}`,
		`{"protection_exempt_paths": ["/{http.request.uri}"]}`,
		`{"protection_exempt_paths": ["/a/../b"]}`,
		`{"protection_exempt_paths": [` + manyPaths(21) + `]}`,
	} {
		rec := request(t, h, http.MethodPatch, "/api/apps/a", body)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "protection_exempt_paths") {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
}

func manyPaths(n int) string {
	var parts []string
	for i := range n {
		parts = append(parts, `"/p`+string(rune('a'+i))+`/"`)
	}
	return strings.Join(parts, ",")
}

// Opening an app needs a recent login; closing it does not.
func TestAccessProtectionChangesNeedRecentLogin(t *testing.T) {
	f := newGateFixture(t)
	f.clk.Advance(ReauthWindow + time.Minute)
	patch := func(body string) int {
		rec := f.console.do(http.MethodPatch, "/api/apps/"+"shop-staging", body, map[string]string{"Origin": consoleURL})
		return rec.Code
	}
	for _, body := range []string{`{"access_protection": false}`, `{"protection_exempt_paths": ["/webhooks/"]}`} {
		if code := patch(body); code != http.StatusForbidden {
			t.Errorf("%s with an old login: %d", body, code)
		}
	}
	if code := patch(`{"access_protection": true}`); code != http.StatusOK {
		t.Errorf("keeping it on: %d", code)
	}
	rec := f.console.do(http.MethodPatch, "/api/apps/shop", `{"access_protection": true}`, map[string]string{"Origin": consoleURL})
	if rec.Code != http.StatusOK {
		t.Errorf("turning protection on: %d %s", rec.Code, rec.Body)
	}
}

// TestGateLetsMembersOfTheAppIn: a member opens the protected apps it has a
// role on, and no others (ARCHITECTURE 10.7).
func TestGateLetsMembersOfTheAppIn(t *testing.T) {
	f := newGateFixture(t)
	ctx := context.Background()
	staging, err := f.srv.store.GetApp(ctx, "shop-staging")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.AddMember(ctx, store.Member{GitHubUserID: 7, Login: "stranger", AddedAt: f.clk.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetAppRole(ctx, staging.ID, 7, store.RoleView); err != nil {
		t.Fatal(err)
	}
	f.console = newBrowser(t, f.h)
	if rec := f.console.login("code-stranger"); rec.Header().Get("Location") != "/" {
		t.Fatalf("member login: %s", rec.Header().Get("Location"))
	}
	cookie := f.enter(stagingHost, "/")
	if rec := f.check(stagingHost, http.MethodGet, "/", cookie); rec.Code != http.StatusNoContent {
		t.Errorf("member with a role: %d", rec.Code)
	}
	rec := f.console.do(http.MethodGet, "/auth/gate?"+url.Values{"host": {otherHost}, "return": {"/"}}.Encode(), "", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("gate of an app without a role: %d %s", rec.Code, rec.Header().Get("Location"))
	}
	// Losing the role closes the app at once, even with the cookie.
	if err := f.srv.store.SetAppRole(ctx, staging.ID, 7, ""); err != nil {
		t.Fatal(err)
	}
	if rec := f.check(stagingHost, http.MethodGet, "/", cookie); rec.Code == http.StatusNoContent {
		t.Error("the gate cookie still works after the role was taken away")
	}
}
