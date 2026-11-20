package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Rejected logins of a known GitHub user are in the login history, and
// GitHub's answer about two-factor authentication plays no part (a GitHub App
// is never told; GitHub enforces it at sign-in).
func TestLoginHistory(t *testing.T) {
	srv, _ := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	if loc := b.login("code-stranger").Header().Get("Location"); loc != "/?login_error=not_allowed" {
		t.Errorf("stranger: redirected to %q", loc)
	}
	if loc := b.login("code-owner").Header().Get("Location"); loc != "/" {
		t.Errorf("owner: redirected to %q", loc)
	}
	rec := b.do(http.MethodGet, "/api/login-events", "", nil)
	var body struct {
		Events []loginEventJSON `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("login events: %d %s", rec.Code, rec.Body)
	}
	var outcomes []string
	for _, ev := range body.Events {
		outcomes = append(outcomes, ev.Outcome)
		if ev.IP != "192.0.2.1" {
			t.Errorf("event IP = %q", ev.IP)
		}
	}
	if got := strings.Join(outcomes, ","); got != "succeeded,not_allowed" {
		t.Errorf("outcomes = %s", got)
	}
}

func TestDangerousOperationsNeedRecentLogin(t *testing.T) {
	srv, clk := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")
	origin := map[string]string{"Origin": consoleURL}

	create := b.do(http.MethodPost, "/api/apps", `{"slug":"blog","web_port":3000,"repo_full_name":"octo/blog"}`, origin)
	if create.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", create.Code, create.Body)
	}
	if rec := b.do(http.MethodPut, "/api/apps/blog/env", `{"vars":[{"key":"SECRET","value":"s3cr3t"}]}`, origin); rec.Code != http.StatusOK {
		t.Fatalf("env within the window: %d %s", rec.Code, rec.Body)
	}
	if rec := b.do(http.MethodGet, "/api/apps/blog/env", "", nil); !strings.Contains(rec.Body.String(), `"value":"s3cr3t"`) ||
		!strings.Contains(rec.Body.String(), `"values_hidden":false`) {
		t.Errorf("env within the window: %s", rec.Body)
	}

	clk.Advance(ReauthWindow)

	refused := []struct{ method, target, body string }{
		{http.MethodDelete, "/api/apps/blog", ""},
		{http.MethodPut, "/api/apps/blog/env", `{"vars":[]}`},
		{http.MethodPut, "/api/apps/blog/env/SECRET", `{"value":"x"}`},
		{http.MethodDelete, "/api/apps/blog/env/SECRET", ""},
		{http.MethodPatch, "/api/apps/blog", `{"environment":"production"}`},
		{http.MethodPatch, "/api/apps/blog", `{"relax_policy":true}`},
		{http.MethodPatch, "/api/apps/blog", `{"repo_full_name":"octo/other"}`},
		{http.MethodPost, "/api/apps/blog/backups/restore", `{"snapshot_id":"4bba301e"}`},
		{http.MethodGet, "/api/apps/blog/backups/4bba301e/download", ""},
		{http.MethodPost, "/api/apps/blog/db-copies", `{"source":"blog-production","confirm":"blog"}`},
	}
	for _, c := range refused {
		rec := b.do(c.method, c.target, c.body, origin)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "reauth_required") {
			t.Errorf("%s %s after the window: %d %s", c.method, c.target, rec.Code, rec.Body)
		}
	}
	// Values are hidden rather than the request refused.
	env := b.do(http.MethodGet, "/api/apps/blog/env", "", nil)
	if env.Code != http.StatusOK || strings.Contains(env.Body.String(), "s3cr3t") ||
		!strings.Contains(env.Body.String(), `{"key":"SECRET","value":null,`) ||
		!strings.Contains(env.Body.String(), `"values_hidden":true`) {
		t.Errorf("env after the window: %d %s", env.Code, env.Body)
	}
	// Ordinary settings, unchanged guarded values and staging deploys go through.
	for _, body := range []string{`{"healthcheck_path":"/up"}`, `{"environment":"staging","relax_policy":false}`} {
		if rec := b.do(http.MethodPatch, "/api/apps/blog", body, origin); rec.Code != http.StatusOK {
			t.Errorf("PATCH %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := b.do(http.MethodPost, "/api/apps/blog/deployments", `{"ref":"main"}`, origin); rec.Code == http.StatusForbidden {
		t.Errorf("staging deploy: %d %s", rec.Code, rec.Body)
	}

	// Logging in again, with a way back to where the user was, opens the window.
	oldCookie := b.cookies[secureSessionCookie].Value
	start := b.do(http.MethodGet, "/auth/github/login?return_to="+url.QueryEscape("/apps/blog/env?x=1"), "", nil)
	loc, _ := url.Parse(start.Header().Get("Location"))
	final := b.do(http.MethodGet, "/auth/github/callback?code=code-owner&state="+url.QueryEscape(loc.Query().Get("state")), "", nil)
	if got := final.Header().Get("Location"); got != "/apps/blog/env?x=1" {
		t.Errorf("after logging in again: redirected to %q", got)
	}
	if _, err := srv.store.GetSession(t.Context(), hashToken(oldCookie), clk.Now()); err == nil {
		t.Error("logging in again must replace the browser's previous session")
	}
	if rec := b.do(http.MethodPatch, "/api/apps/blog", `{"environment":"production"}`, origin); rec.Code != http.StatusOK {
		t.Fatalf("PATCH after logging in again: %d %s", rec.Code, rec.Body)
	}

	// Production deploys need the window too.
	clk.Advance(ReauthWindow)
	if rec := b.do(http.MethodPost, "/api/apps/blog/deployments", `{"ref":"main"}`, origin); rec.Code != http.StatusForbidden {
		t.Errorf("production deploy after the window: %d %s", rec.Code, rec.Body)
	}
}

func TestReturnPathMustStayOnTheConsole(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "/",
		"/apps/blog/env":         "/apps/blog/env",
		"/apps/blog?tab=1#x":     "/apps/blog?tab=1#x",
		"//evil.example/":        "/",
		"/\\evil.example":        "/",
		"https://evil.example/":  "/",
		"apps":                   "/",
		"/ok\r\nSet-Cookie: x=1": "/",
	} {
		if got := safeReturnPath(in); got != want {
			t.Errorf("safeReturnPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientIP(t *testing.T) {
	for name, c := range map[string]struct {
		remote string
		xff    []string
		want   string
	}{
		"direct":                  {"203.0.113.9:5555", nil, "203.0.113.9"},
		"forged header, direct":   {"203.0.113.9:5555", []string{"198.51.100.1"}, "203.0.113.9"},
		"via caddy":               {"127.0.0.1:40000", []string{"203.0.113.5"}, "203.0.113.5"},
		"via caddy, last entry":   {"127.0.0.1:40000", []string{"198.51.100.1, 203.0.113.5"}, "203.0.113.5"},
		"via caddy, ipv6":         {"[::1]:40000", []string{"2001:db8::1"}, "2001:db8::1"},
		"via caddy, garbage":      {"127.0.0.1:40000", []string{"nonsense"}, "127.0.0.1"},
		"loopback without header": {"127.0.0.1:40000", nil, "127.0.0.1"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = c.remote
		for _, v := range c.xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		if got := clientIP(req); got != c.want {
			t.Errorf("%s: clientIP = %q, want %q", name, got, c.want)
		}
	}
}

func TestSessionListAndRevoke(t *testing.T) {
	srv, clk := newAuthServer(t, defaultGitHub())
	laptop := newBrowser(t, srv.Handler())
	laptop.login("code-owner")
	clk.Advance(time.Minute)
	phone := newBrowser(t, srv.Handler())
	phone.login("code-owner")
	origin := map[string]string{"Origin": consoleURL}

	var body struct {
		Sessions []sessionJSON `json:"sessions"`
	}
	rec := laptop.do(http.MethodGet, "/api/sessions", "", nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Sessions) != 2 {
		t.Fatalf("sessions: %d %s", rec.Code, rec.Body)
	}
	phoneSession, laptopSession := body.Sessions[0], body.Sessions[1]
	if phoneSession.Current || !laptopSession.Current || len(laptopSession.ID) != 16 || laptopSession.IP != "192.0.2.1" {
		t.Errorf("sessions = %+v", body.Sessions)
	}
	if strings.Contains(rec.Body.String(), hashToken(laptop.cookies[secureSessionCookie].Value)) {
		t.Error("the token hash must not be exposed")
	}

	// Last-seen is updated at most once a minute.
	clk.Advance(2 * time.Minute)
	laptop.do(http.MethodGet, "/api/apps", "", nil)
	sess, _ := srv.store.GetSession(t.Context(), hashToken(laptop.cookies[secureSessionCookie].Value), clk.Now())
	if !sess.LastSeenAt.Equal(clk.Now()) {
		t.Errorf("last seen = %v, want %v", sess.LastSeenAt, clk.Now())
	}

	if rec := laptop.do(http.MethodDelete, "/api/sessions/0000000000000000", "", origin); rec.Code != http.StatusNotFound {
		t.Errorf("unknown session: %d %s", rec.Code, rec.Body)
	}
	revoke := laptop.do(http.MethodPost, "/api/sessions/revoke-others", "", origin)
	if revoke.Code != http.StatusOK || strings.TrimSpace(revoke.Body.String()) != `{"revoked":1}` {
		t.Errorf("revoke others: %d %s", revoke.Code, revoke.Body)
	}
	if rec := phone.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked phone: %d", rec.Code)
	}
	if rec := laptop.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusOK {
		t.Errorf("the caller's own session must survive: %d", rec.Code)
	}

	// Ending one's own session logs out.
	if rec := laptop.do(http.MethodDelete, "/api/sessions/"+laptopSession.ID, "", origin); rec.Code != http.StatusNoContent {
		t.Fatalf("delete own session: %d %s", rec.Code, rec.Body)
	}
	if _, ok := laptop.cookies[secureSessionCookie]; ok {
		t.Error("ending one's own session must clear the cookie")
	}
	if rec := laptop.do(http.MethodGet, "/api/apps", "", nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("after ending own session: %d", rec.Code)
	}
}

func TestSessionsInNoAuthMode(t *testing.T) {
	h := newTestServer(t, nil).Handler()
	for target, want := range map[string]string{
		"/api/sessions":     `{"sessions":[]}`,
		"/api/login-events": `{"events":[]}`,
	} {
		rec := request(t, h, http.MethodGet, target, "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != want {
			t.Errorf("%s: %d %s", target, rec.Code, rec.Body)
		}
	}
	session := request(t, h, http.MethodGet, "/api/session", "")
	if !strings.Contains(session.Body.String(), `"reauth_until":null`) {
		t.Errorf("session: %s", session.Body)
	}
}
