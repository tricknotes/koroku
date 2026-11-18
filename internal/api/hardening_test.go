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
		"foreign origin":       func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"origin null":          func(r *http.Request) { r.Header.Set("Origin", "null") },
		"other local port":     func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:9999") },
		"rebound host name":    func(r *http.Request) { r.Host = "attacker.example:8080" },
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
func TestOAuthCallbackIsRateLimited(t *testing.T) {
	gh := defaultGitHub()
	srv, clk := newAuthServer(t, gh)
	h := srv.Handler()
	exchanges := 0
	call := func() string {
		req := localRequest(http.MethodGet, "/auth/github/callback?code=bogus&state=abc", nil)
		req.AddCookie(&http.Cookie{Name: secureStateCookie, Value: "abc"})
		before := gh.exchanges
		rec := send(h, req)
		if gh.exchanges > before {
			exchanges++
		}
		return rec.Header().Get("Location")
	}
	for i := 0; i < 25; i++ {
		if got := call(); got != "/?login_error=github_error" {
			t.Fatalf("call %d: Location = %q", i, got)
		}
	}
	if exchanges != 10 {
		t.Errorf("GitHub was asked %d times in one minute, want 10", exchanges)
	}
	// The window resets, so a real login is only delayed.
	clk.Advance(time.Minute)
	b := newBrowser(t, h)
	if final := b.login("code-owner"); final.Header().Get("Location") != "/" {
		t.Errorf("login after the window: Location = %q", final.Header().Get("Location"))
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
	srv := newTestServer(t, func(o *Options) {
		o.Config.ConsoleHost = "admin.dev.example.com"
	})
	h := srv.Handler()
	rec := request(t, h, http.MethodPost, "/api/apps", `{"slug": "admin", "repo_full_name": "octo/admin", "web_port": 3000}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "reserved") {
		t.Errorf("slug equal to the console host: %d %s", rec.Code, rec.Body)
	}

	outside := newTestServer(t, func(o *Options) {
		o.Config.ConsoleHost = "console.example.org"
	}).Handler()
	createApp(t, outside, "blog")
	rec = request(t, outside, http.MethodPost, "/api/apps/blog/domains", fmt.Sprintf(`{"hostname": %q}`, "console.example.org"))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "console") {
		t.Errorf("custom domain equal to the console host: %d %s", rec.Code, rec.Body)
	}
}
