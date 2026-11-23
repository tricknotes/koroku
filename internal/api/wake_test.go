package api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/store"
)

// fakeWaker wakes apps by clearing their sleeping state, or blocks or fails
// as a test asks.
type fakeWaker struct {
	st    *store.Store
	mu    sync.Mutex
	calls int
	err   error
	block bool
}

func (f *fakeWaker) Wake(ctx context.Context, app store.App) error {
	f.mu.Lock()
	f.calls++
	err, block := f.err, f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	_, err = f.st.MarkAppAwake(ctx, app.ID)
	return err
}

func (f *fakeWaker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// seenRequest is what the app behind the proxy received.
type seenRequest struct {
	host, forwardedFor, wakeHeader, body, path string
}

type wakeFixture struct {
	t     *testing.T
	srv   *Server
	h     http.Handler
	waker *fakeWaker
	app   store.App
	seen  chan seenRequest
}

// newWakeFixture serves a deployed, sleeping app "blog" whose port is a
// real HTTP server.
func newWakeFixture(t *testing.T, mutate func(*Options)) *wakeFixture {
	t.Helper()
	f := &wakeFixture{t: t, seen: make(chan seenRequest, 10)}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.seen <- seenRequest{
			host: r.Host, forwardedFor: r.Header.Get("X-Forwarded-For"),
			wakeHeader: r.Header.Get(caddy.WakeHeader), body: string(body), path: r.URL.RequestURI(),
		}
		w.Header().Set("Content-Security-Policy", "default-src 'self' https://cdn.example.com")
		_, _ = io.WriteString(w, "hello from the app")
	}))
	t.Cleanup(backend.Close)
	_, portText, _ := net.SplitHostPort(backend.Listener.Addr().String())
	port, _ := strconv.Atoi(portText)

	f.srv = newTestServer(t, func(o *Options) {
		f.waker = &fakeWaker{st: o.Store}
		o.Waker = f.waker
		o.Config.Caddy.Enabled = true
		o.Config.Caddy.AccessLogDir = "/var/log/caddy/apps"
		if mutate != nil {
			mutate(o)
		}
	})
	ctx := context.Background()
	st := f.srv.store
	app, err := st.CreateApp(ctx, store.App{
		Slug: "blog", RepoFullName: "octo/blog", DefaultBranch: "main", WebService: "web",
		WebPort: 3000, HealthcheckPath: "/", IdleSleep: true,
	}, port, port, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := st.EnqueueDeployment(ctx, app.ID, "main", testSHA, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_ = st.StartDeployment(ctx, d.ID, time.Now())
	if err := st.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MarkAppSleeping(ctx, app.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.app = app
	f.h = f.srv.Handler()
	return f
}

// forwarded is a request as Caddy forwards it for a sleeping app.
func (f *wakeFixture) forwarded(method, target, body, slug string, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Host = "blog.dev.example.com"
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	req.Header.Set("X-Forwarded-Proto", "https")
	if slug != "" {
		req.Header.Set(caddy.WakeHeader, slug)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return send(f.h, req)
}

func TestWakeThenProxyTheRequest(t *testing.T) {
	f := newWakeFixture(t, nil)
	rec := f.forwarded(http.MethodPost, "/posts?draft=1", "title=hello", "blog", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello from the app" {
		t.Fatalf("status = %d, body = %q", rec.Code, rec.Body)
	}
	if f.waker.count() != 1 {
		t.Errorf("wakes = %d", f.waker.count())
	}
	seen := <-f.seen
	if seen.host != "blog.dev.example.com" || seen.forwardedFor != "203.0.113.7" || seen.wakeHeader != "" ||
		seen.body != "title=hello" || seen.path != "/posts?draft=1" {
		t.Errorf("the app saw %+v", seen)
	}
	// The app's own headers, not the console's.
	if csp := rec.Header().Values("Content-Security-Policy"); len(csp) != 1 || !strings.Contains(csp[0], "cdn.example.com") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	if rec.Header().Get("X-Frame-Options") != "" {
		t.Error("the console's security headers must not be added to the app's answer")
	}

	// Awake now (Caddy has not caught up yet): proxied without a wake.
	if rec := f.forwarded(http.MethodGet, "/", "", "blog", nil); rec.Code != http.StatusOK {
		t.Fatalf("awake: status = %d", rec.Code)
	}
	<-f.seen
	if f.waker.count() != 1 {
		t.Errorf("an awake app was woken again: %d", f.waker.count())
	}
}

func TestWakeTakingTooLongAsksToRetry(t *testing.T) {
	f := newWakeFixture(t, func(o *Options) { o.WakeHold = 20 * time.Millisecond })
	f.waker.block = true

	page := f.forwarded(http.MethodGet, "/", "", "blog", map[string]string{"Accept": "text/html,application/xhtml+xml"})
	if page.Code != http.StatusServiceUnavailable || page.Header().Get("Retry-After") != "5" {
		t.Fatalf("status = %d, Retry-After %q", page.Code, page.Header().Get("Retry-After"))
	}
	body := page.Body.String()
	if !strings.Contains(body, "起動しています") || !strings.Contains(body, `http-equiv="refresh" content="5"`) ||
		strings.Contains(body, "<script") {
		t.Errorf("page:\n%s", body)
	}
	if page.Header().Get("Cache-Control") != "no-store" {
		t.Error("the page must not be cached")
	}

	api := f.forwarded(http.MethodPost, "/api/items", `{"a":1}`, "blog", map[string]string{"Accept": "application/json"})
	if api.Code != http.StatusServiceUnavailable || !strings.HasPrefix(api.Header().Get("Content-Type"), "text/plain") {
		t.Errorf("non-browser: %d %q", api.Code, api.Header().Get("Content-Type"))
	}
}

func TestFailedWakeIsA502(t *testing.T) {
	f := newWakeFixture(t, nil)
	f.waker.err = errors.New("wake blog: health check did not pass")
	rec := f.forwarded(http.MethodGet, "/", "", "blog", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "起動できませんでした") {
		t.Errorf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "health check") {
		t.Error("the error must not reach the visitor")
	}
}

func TestWakeHeaderIsHonouredOnlyWhereCaddySendsIt(t *testing.T) {
	f := newWakeFixture(t, nil)
	other, err := f.srv.store.CreateApp(context.Background(), store.App{
		Slug: "shop", RepoFullName: "octo/shop", DefaultBranch: "main", WebService: "web", WebPort: 3000, HealthcheckPath: "/",
	}, 21000, 21999, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	// The header names another app than the hostname's.
	if rec := f.forwarded(http.MethodGet, "/", "", other.Slug, nil); rec.Code != http.StatusNotFound {
		t.Errorf("mismatched slug: %d", rec.Code)
	}
	// The access protection paths never wake anything.
	for _, path := range []string{caddy.GateCheckPath, "/.koroku/auth/callback?code=x"} {
		if rec := f.forwarded(http.MethodGet, path, "", "blog", nil); rec.Code == http.StatusOK {
			t.Errorf("%s reached the app", path)
		}
	}
	// On the console host the header means nothing.
	req := localRequest(http.MethodGet, "/api/apps", nil)
	req.Header.Set(caddy.WakeHeader, "blog")
	if rec := send(f.h, req); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"apps"`) {
		t.Errorf("console with the header: %d %s", rec.Code, rec.Body)
	}
	if f.waker.count() != 0 {
		t.Errorf("woken %d times", f.waker.count())
	}
}

// Caddy routes a request by its decoded, cleaned, lower-cased path and
// forwards it as the client sent it. A request Caddy sent to koroku as a
// gate path must never be taken for one of the wake route, whatever its
// spelling and its headers; and the wake route proxies no path that could
// be read in two ways.
func TestWakeNeverProxiesAnotherSpellingOfAPath(t *testing.T) {
	f := newWakeFixture(t, nil)
	if _, err := f.srv.store.MarkAppAwake(context.Background(), f.app.ID); err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]int{
		// Gate paths in another spelling.
		"/graphql/../.koroku/auth/x?q=1":   http.StatusNotFound,
		"/graphql/..%2F.koroku/auth/x?q=1": http.StatusNotFound,
		"/%2e%2e/.koroku/auth/x":           http.StatusNotFound,
		"/.KOROKU/auth/x":                  http.StatusNotFound,
		"/.koroku":                         http.StatusNotFound,
		"/x/../.koroku":                    http.StatusNotFound,
		"//.koroku/auth/x":                 http.StatusNotFound,
		"/AUTH/GATE/CHECK":                 http.StatusNotFound,
		"/auth/gate/../gate/check":         http.StatusNotFound,
		// Paths of the app that are not in their cleaned form.
		"/admin/../webhooks/x":   http.StatusBadRequest,
		"/admin/..%2Fwebhooks/x": http.StatusBadRequest,
		"/admin/%2E%2E/x":        http.StatusBadRequest,
		"/a/./b":                 http.StatusBadRequest,
		"/a//b":                  http.StatusBadRequest,
		"//":                     http.StatusBadRequest,
	} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := f.forwarded(method, target, "", "blog", nil)
			if rec.Code != want {
				t.Errorf("%s %s: %d %s, want %d", method, target, rec.Code, rec.Body, want)
			}
		}
	}
	select {
	case seen := <-f.seen:
		t.Errorf("the app received %+v", seen)
	default:
	}

	// Cleaned paths reach the app as they were sent.
	for _, target := range []string{"/Posts/1?q=../x", "/files/a%2Fb", "/a/..b/.c/"} {
		if rec := f.forwarded(http.MethodGet, target, "", "blog", nil); rec.Code != http.StatusOK {
			t.Errorf("%s: %d", target, rec.Code)
			continue
		}
		if seen := <-f.seen; seen.path != target {
			t.Errorf("%s reached the app as %s", target, seen.path)
		}
	}

	// A sleeping app is not woken by any of them either.
	if _, err := f.srv.store.MarkAppSleeping(context.Background(), f.app.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/graphql/../.koroku/auth/x", "/.KOROKU/auth/x", "/admin/../webhooks/x", "/a//b"} {
		if rec := f.forwarded(http.MethodGet, target, "", "blog", nil); rec.Code == http.StatusOK {
			t.Errorf("%s: %d", target, rec.Code)
		}
	}
	if f.waker.count() != 0 {
		t.Errorf("woken %d times", f.waker.count())
	}
}

func TestWakeAppFromTheConsole(t *testing.T) {
	f := newWakeFixture(t, nil)
	rec := request(t, f.h, http.MethodPost, "/api/apps/blog/wake", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if app := decode[appJSON](t, rec); app.SleepingSince != nil || !app.IdleSleep {
		t.Errorf("app = %+v", app)
	}
	activity, err := f.srv.store.ListActivity(context.Background(), f.app.ID, 0, 10)
	if err != nil || len(activity) != 1 || activity[0].Action != actAppWoken {
		t.Errorf("activity = %+v, %v", activity, err)
	}
	// An awake app: nothing to do, nothing recorded.
	if rec := request(t, f.h, http.MethodPost, "/api/apps/blog/wake", ""); rec.Code != http.StatusOK || f.waker.count() != 1 {
		t.Errorf("awake: %d, wakes %d", rec.Code, f.waker.count())
	}

	f = newWakeFixture(t, nil)
	f.waker.err = errors.New("boom")
	if rec := request(t, f.h, http.MethodPost, "/api/apps/blog/wake", ""); rec.Code != http.StatusBadGateway ||
		!strings.Contains(rec.Body.String(), "wake_failed") {
		t.Errorf("failed wake: %d %s", rec.Code, rec.Body)
	}
}

func TestIdleSleepSettings(t *testing.T) {
	f := newWakeFixture(t, nil)
	rec := request(t, f.h, http.MethodPatch, "/api/apps/blog", `{"idle_sleep": true, "idle_sleep_minutes": 60}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if app := decode[appJSON](t, rec); !app.IdleSleep || app.IdleSleepMinutes != 60 || app.SleepingSince == nil {
		t.Errorf("app = %+v", app)
	}
	for _, minutes := range []string{"5", "1441"} {
		if rec := request(t, f.h, http.MethodPatch, "/api/apps/blog", `{"idle_sleep_minutes": `+minutes+`}`); rec.Code != http.StatusUnprocessableEntity ||
			!strings.Contains(rec.Body.String(), "idle_sleep_minutes") {
			t.Errorf("%s minutes: %d %s", minutes, rec.Code, rec.Body)
		}
	}
	if rec := do(t, f.h, http.MethodGet, "/api/system"); !strings.Contains(rec.Body.String(), `"idle_sleep":{"available":true}`) {
		t.Errorf("system: %s", rec.Body)
	}

	// Without access logs, requests cannot be counted.
	h := newTestServer(t, func(o *Options) { o.Config.Caddy.AccessLogDir = "" }).Handler()
	createApp(t, h, "shop")
	rec = request(t, h, http.MethodPatch, "/api/apps/shop", `{"idle_sleep": true}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"idle_sleep"`) {
		t.Errorf("unavailable: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, http.MethodGet, "/api/system"); !strings.Contains(rec.Body.String(), `"idle_sleep":{"available":false}`) {
		t.Errorf("system: %s", rec.Body)
	}
}
