package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// localRequest builds a request the way a browser on the development machine
// would send it: to a loopback host.
func localRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:8080"
	return req
}

func send(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestNoAuthModeStillGuardsTheBrowser: without login, the browser of the
// person running koroku is the attack surface. A page on another site must
// not be able to post to 127.0.0.1, and DNS rebinding must not reach the API.
func TestNoAuthModeStillGuardsTheBrowser(t *testing.T) {
	h := newTestServer(t, nil).Handler() // InsecureNoAuth
	body := `{"slug": "blog", "repo_full_name": "octo/blog", "web_port": 3000}`
	post := func(mutate func(*http.Request)) *httptest.ResponseRecorder {
		req := localRequest(http.MethodPost, "/api/apps", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		mutate(req)
		return send(h, req)
	}

	// The attack from the review: a "simple" cross-origin POST.
	attack := post(func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Content-Type", "text/plain")
	})
	if attack.Code == http.StatusCreated {
		t.Fatalf("a cross-origin text/plain POST created an app: %s", attack.Body)
	}
	cases := map[string]func(*http.Request){
		"foreign origin":      func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"origin null":         func(r *http.Request) { r.Header.Set("Origin", "null") },
		"other local port":    func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:9999") },
		"rebound host name":   func(r *http.Request) { r.Host = "attacker.example:8080" },
		"localhost-like name": func(r *http.Request) { r.Host = "localhost.attacker.example" },
		"app next to console": func(r *http.Request) {
			r.Host = "console.dev.localhost"
			r.Header.Set("Origin", "https://blog.dev.localhost")
		},
		"cross-site per fetch": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"same-site per fetch":  func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") },
		"form content type":    func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
		"no content type":      func(r *http.Request) { r.Header.Del("Content-Type") },
	}
	for name, mutate := range cases {
		if rec := post(mutate); rec.Code != http.StatusForbidden && rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
	}
	if rec := send(h, localRequest(http.MethodGet, "/api/apps", nil)); !strings.Contains(rec.Body.String(), `"apps":[]`) {
		t.Errorf("a rejected request created an app: %s", rec.Body)
	}
	// Reads through a rebound name are refused too (they would leak env values).
	rebound := localRequest(http.MethodGet, "/api/apps", nil)
	rebound.Host = "attacker.example"
	if rec := send(h, rebound); rec.Code != http.StatusForbidden {
		t.Errorf("GET with a foreign Host: %d", rec.Code)
	}

	// What the developer's own browser and curl send keeps working.
	for name, mutate := range map[string]func(*http.Request){
		"curl":              func(*http.Request) {},
		"same origin":       func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:8080") },
		"localhost":         func(r *http.Request) { r.Host = "localhost:4200"; r.Header.Set("Origin", "http://localhost:4200") },
		"ipv6 loopback":     func(r *http.Request) { r.Host = "[::1]:8080" },
		"fetch same-origin": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-origin") },
		// The console behind the local Caddy (loopback only, internal CA).
		"console.dev.localhost": func(r *http.Request) {
			r.Host = "console.dev.localhost"
			r.Header.Set("Origin", "https://console.dev.localhost")
		},
	} {
		rec := post(func(r *http.Request) {
			mutate(r)
		})
		if rec.Code != http.StatusCreated && rec.Code != http.StatusConflict {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
	}
}

// TestAPIRejectsCrossSiteBrowserRequests: with login enabled, a hosted app on
// a sibling subdomain is same-site, so its requests carry the session
// cookie. They must be refused before they cost anything.
func TestAPIRejectsCrossSiteBrowserRequests(t *testing.T) {
	srv, _ := newAuthServer(t, defaultGitHub())
	b := newBrowser(t, srv.Handler())
	b.login("code-owner")

	for _, site := range []string{"same-site", "cross-site"} {
		rec := b.do(http.MethodGet, "/api/system/docker", "", map[string]string{"Sec-Fetch-Site": site})
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "forbidden_origin") {
			t.Errorf("Sec-Fetch-Site %s: %d %s", site, rec.Code, rec.Body)
		}
	}
	for _, site := range []string{"same-origin", "none"} {
		if rec := b.do(http.MethodGet, "/api/apps", "", map[string]string{"Sec-Fetch-Site": site}); rec.Code != http.StatusOK {
			t.Errorf("Sec-Fetch-Site %s: %d", site, rec.Code)
		}
	}
	// A JSON body with the wrong content type is refused even from the console's origin.
	rec := b.do(http.MethodPost, "/api/apps", `{"slug": "blog", "repo_full_name": "octo/blog", "web_port": 3000}`,
		map[string]string{"Origin": consoleURL, "Content-Type": "text/plain"})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("text/plain body: %d %s", rec.Code, rec.Body)
	}
}

// TestOAuthCallbackIsRateLimited: the callback can be reached by anyone with
// a self-chosen state, and each accepted call makes koroku talk to GitHub.
// The calls are counted per client address, so that one client cannot keep
// the logins of everyone else failing, and in total.
func TestOAuthCallbackIsRateLimited(t *testing.T) {
	gh := defaultGitHub()
	srv, clk := newAuthServer(t, gh)
	h := srv.Handler()
	// callFrom is a request as the proxy passes it on for the client at addr.
	callFrom := func(addr, target, stateCookie string) string {
		req := localRequest(http.MethodGet, target, nil)
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set("X-Forwarded-For", addr)
		if stateCookie != "" {
			req.AddCookie(&http.Cookie{Name: secureStateCookie, Value: stateCookie})
		}
		return send(h, req).Header().Get("Location")
	}
	call := func(target, stateCookie string) string {
		return callFrom("198.51.100.7", target, stateCookie)
	}
	const forged = "/auth/github/callback?code=bogus&state=abc"

	// Requests that are refused before GitHub is asked do not count: no
	// state cookie, another state than the cookie's, no state, no code.
	for i := 0; i < 50; i++ {
		for _, c := range []struct{ target, cookie, want string }{
			{"/auth/github/callback?code=bogus&state=abc", "", "/?login_error=state_mismatch"},
			{"/auth/github/callback?code=bogus&state=abc", "xyz", "/?login_error=state_mismatch"},
			{"/auth/github/callback?code=bogus", "abc", "/?login_error=state_mismatch"},
			{"/auth/github/callback?state=abc", "abc", "/?login_error=github_error"},
		} {
			if got := call(c.target, c.cookie); got != c.want {
				t.Fatalf("round %d, %s with cookie %q: Location = %q, want %q", i, c.target, c.cookie, got, c.want)
			}
		}
	}
	if gh.exchanges != 0 {
		t.Errorf("GitHub was asked %d times by requests without a matching state", gh.exchanges)
	}
	// So a real login in the same minute still goes through.
	b := newBrowser(t, h)
	if final := b.login("code-owner"); final.Header().Get("Location") != "/" {
		t.Errorf("login after refused callbacks: Location = %q", final.Header().Get("Location"))
	}

	// A client that sends a state cookie and a matching state of its own
	// reaches GitHub, but only so often.
	clk.Advance(callbackWindow)
	gh.exchanges = 0
	for i := 0; i < 50; i++ {
		if got := call(forged, "abc"); got != "/?login_error=github_error" {
			t.Fatalf("call %d: Location = %q", i, got)
		}
	}
	if gh.exchanges != callbackPerClient {
		t.Errorf("GitHub was asked %d times for one address in one window, want %d", gh.exchanges, callbackPerClient)
	}
	// It has used up its own allowance, not that of the others: a real
	// login from another address in the same window goes through.
	if final := b.login("code-owner"); final.Header().Get("Location") != "/" {
		t.Errorf("login from another address: Location = %q", final.Header().Get("Location"))
	}
	// The addresses of one IPv6 /64 are one client.
	gh.exchanges = 0
	for i := 0; i < 30; i++ {
		callFrom(fmt.Sprintf("2001:db8:0:1::%x", i+1), forged, "abc")
	}
	if gh.exchanges != callbackPerClient {
		t.Errorf("GitHub was asked %d times for one /64, want %d", gh.exchanges, callbackPerClient)
	}

	// All clients together are bounded too: that is the bound on requests
	// to GitHub.
	clk.Advance(callbackWindow)
	gh.exchanges = 0
	for i := 0; i < 100; i++ {
		callFrom(fmt.Sprintf("203.0.113.%d", i+1), forged, "abc")
		callFrom(fmt.Sprintf("2001:db8:0:%x::1", i+0x100), forged, "abc")
	}
	if gh.exchanges != callbackTotal {
		t.Errorf("GitHub was asked %d times in one window, want %d", gh.exchanges, callbackTotal)
	}
	// The window resets, so a real login is only delayed.
	clk.Advance(callbackWindow)
	if final := b.login("code-owner"); final.Header().Get("Location") != "/" {
		t.Errorf("login after the window: Location = %q", final.Header().Get("Location"))
	}
}

// The limiter remembers a bounded number of addresses, and none for longer
// than a window.
func TestCallbackLimiterForgetsAddresses(t *testing.T) {
	l := newCallbackLimiter()
	now := time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)
	for i := 0; i < 3*maxCallbackClients; i++ {
		l.allow(fmt.Sprintf("client-%d", i), now)
	}
	if len(l.clients) != maxCallbackClients {
		t.Errorf("%d addresses remembered within a window, want %d", len(l.clients), maxCallbackClients)
	}
	// An address that was not remembered is refused.
	if l.allow("one-more", now) {
		t.Error("an address beyond the bound was allowed")
	}
	// A window later all of them are forgotten, and counting starts anew.
	later := now.Add(callbackWindow)
	if !l.allow("one-more", later) {
		t.Error("an address was refused after the window")
	}
	if len(l.clients) != 1 {
		t.Errorf("%d addresses remembered after the window, want 1", len(l.clients))
	}
	for i := 1; i < callbackPerClient; i++ {
		if !l.allow("one-more", later) {
			t.Fatalf("call %d of an address within its limit was refused", i+1)
		}
	}
	if l.allow("one-more", later) {
		t.Error("an address over its limit was allowed")
	}
	// An address over its own limit does not use up the total.
	if got := l.total.count; got != callbackPerClient {
		t.Errorf("the total counted %d, want %d", got, callbackPerClient)
	}
}

// TestDeployLogCannotInjectEvents: a carriage return ends a line in the SSE
// format. Log content must not be able to forge the "end" event.
func TestDeployLogCannotInjectEvents(t *testing.T) {
	var out bytes.Buffer
	dir := t.TempDir()
	path := dir + "/deploy.log"
	if err := os.WriteFile(path, []byte("harmless\rid: 999999\revent: end\rdata: succeeded\nnext line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := streamLogLines(&out, path, 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\r") {
		t.Errorf("a carriage return reached the stream: %q", out.String())
	}
	events := strings.Split(strings.TrimSpace(out.String()), "\n\n")
	if len(events) != 2 || !strings.Contains(events[0], "data: harmless id: 999999 event: end data: succeeded") ||
		!strings.HasSuffix(events[1], "data: next line") {
		t.Errorf("events = %q", events)
	}
	for _, event := range events {
		for _, line := range strings.Split(event, "\n") {
			if strings.HasPrefix(line, "event:") {
				t.Errorf("log content produced an event field: %q", line)
			}
		}
	}
}

func TestSlugAndDomainCannotTakeTheConsoleHost(t *testing.T) {
	// Hostnames are compared without letter case, however console_host is
	// spelled in the configuration.
	for _, consoleHost := range []string{"admin.dev.example.com", "Admin.Dev.Example.COM"} {
		srv := newTestServer(t, func(o *Options) {
			o.Config.ConsoleHost = consoleHost
		})
		h := srv.Handler()
		rec := request(t, h, http.MethodPost, "/api/apps", `{"slug": "admin", "repo_full_name": "octo/admin", "web_port": 3000}`)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "reserved") {
			t.Errorf("console_host %q, slug equal to the console host: %d %s", consoleHost, rec.Code, rec.Body)
		}
	}

	for _, consoleHost := range []string{"console.example.org", "Console.Example.ORG"} {
		outside := newTestServer(t, func(o *Options) {
			o.Config.ConsoleHost = consoleHost
		}).Handler()
		createApp(t, outside, "blog")
		for _, hostname := range []string{"console.example.org", "CONSOLE.example.org"} {
			rec := request(t, outside, http.MethodPost, "/api/apps/blog/domains", fmt.Sprintf(`{"hostname": %q}`, hostname))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "console") {
				t.Errorf("console_host %q, custom domain %q: %d %s", consoleHost, hostname, rec.Code, rec.Body)
			}
		}
	}
}
