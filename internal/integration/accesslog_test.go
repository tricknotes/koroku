//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/api"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/idle"
	"github.com/tricknotes/koroku/internal/routing"
	"github.com/tricknotes/koroku/internal/store"
)

// TestAccessLogThroughCaddy runs a real Caddy with the generated
// configuration and reads the per-app access log it writes: an entry holds
// no headers, stays far below the line length the collector keeps whatever
// the client sends, and is what the idle tracker reads to tell the gate's
// answers from the requests that reached a protected app (ARCHITECTURE 8,
// 8.4).
func TestAccessLogThroughCaddy(t *testing.T) {
	t.Parallel()
	const image = "caddy:2"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		if out, err := exec.Command("docker", "pull", "--quiet", image).CombinedOutput(); err != nil {
			t.Skipf("the %s image is not available: %v: %s", image, err, out)
		}
	}
	ctx := context.Background()
	now := time.Now()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	const (
		base        = "dev.example.com"
		consoleHost = "console." + base
		appHost     = "shop-staging." + base
		stoppedHost = "paused." + base
		secret      = "do-not-log-this-value"
	)
	// The app sets a cookie on every answer and redirects one path itself.
	appPort := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "app_session="+secret+"; Path=/")
		if strings.HasSuffix(r.URL.Path, "/moved") {
			http.Redirect(w, r, "https://"+appHost+"/login", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "app")
	}))
	create := func(slug string, port int, protected bool, exempt []string) store.App {
		app, err := st.CreateApp(ctx, store.App{
			Slug: slug, RepoFullName: "octo/" + slug, DefaultBranch: "main", WebService: "web",
			WebPort: 3000, HealthcheckPath: "/", Environment: store.EnvironmentStaging,
			AccessProtection: protected, ProtectionExemptPaths: exempt,
		}, port, port, now)
		if err != nil {
			t.Fatal(err)
		}
		d, _, err := st.EnqueueDeployment(ctx, app.ID, "main", strings.Repeat("a", 40), "", now)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.StartDeployment(ctx, d.ID, now); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", now); err != nil {
			t.Fatal(err)
		}
		return app
	}
	create("shop-staging", appPort, true, []string{"/webhooks/"})
	paused := create("paused", freePort(t), false, nil)
	if err := st.SetAppDesiredState(ctx, paused.ID, store.DesiredStopped, now); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.BaseDomain = base
	cfg.ConsoleHost = consoleHost
	cfg.ConsoleURL = "https://" + consoleHost
	cfg.DataDir = t.TempDir()
	server := api.New(api.Options{
		Config: cfg, Store: st, Clock: clock.System{},
		Auth: api.AuthOptions{AllowedUserID: 42, ConsoleURL: cfg.ConsoleURL},
	})
	korokuPort := serveOn(t, server.Handler())

	// The log directory is inside the Caddy container.
	const logDir = "/tmp/koroku-access"
	loader := &captureLoader{}
	router := &routing.Router{Store: st, Loader: loader, Settings: routing.Settings{
		BaseDomain:      base,
		ConsoleHost:     consoleHost,
		ConsoleUpstream: fmt.Sprintf("127.0.0.1:%d", korokuPort),
		ListenIP:        "127.0.0.1",
		InternalCA:      true,
		AccessLogDir:    logDir,
	}}
	if err := router.ForceSync(ctx); err != nil {
		t.Fatalf("ForceSync: %v", err)
	}
	httpsPort, caddyLog := startCaddy(t, image, "accesslog", loader.config)
	container := fmt.Sprintf("koroku-accesslog-test-%d", httpsPort)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpsPort))
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Caddy's local CA
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	send := func(method, host, path string, header http.Header) int {
		t.Helper()
		req, err := http.NewRequest(method, "https://"+host+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.URL.Opaque = path // sent as is
		for name, values := range header {
			req.Header[name] = values
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s%s: %v\n--- caddy ---\n%s", method, host, path[:min(len(path), 40)], err, caddyLog.String())
		}
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
		return res.StatusCode
	}

	// The console is not logged, so waiting for it adds no entries.
	deadline := time.Now().Add(30 * time.Second)
	for {
		res, err := client.Get("https://" + consoleHost + "/api/system")
		if err == nil {
			_ = res.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("caddy did not come up: %v\n%s", err, caddyLog.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Everything a client chooses, close to the 20 KB of request line and
	// headers that Caddy accepts (it answers 431 beyond).
	hostile := http.Header{
		"User-Agent":    {strings.Repeat("A", 4000)},
		"Cookie":        {"session=" + secret},
		"Authorization": {"Bearer " + secret},
	}
	for i := 0; i < 3; i++ {
		hostile[fmt.Sprintf("X-Padding-%d", i)] = []string{strings.Repeat(secret, 100)}
	}
	// Bytes that would take six characters each in the entry.
	escaped := http.Header{"User-Agent": {"agent \"quoted\" " + strings.Repeat("\xff", 4000)}}
	requests := []struct {
		name         string
		method, path string
		header       http.Header
		status       int
		// counts says whether the request keeps the protected app awake.
		counts bool
	}{
		{"gate redirect", http.MethodGet, "/secret?x=1", http.Header{"User-Agent": {"koroku-test/1.0"}, "Cookie": {"session=" + secret}}, http.StatusFound, false},
		{"gate refusal", http.MethodPost, "/secret", nil, http.StatusUnauthorized, false},
		{"reaches the app", http.MethodPost, "/webhooks/push", http.Header{"Authorization": {"Bearer " + secret}}, http.StatusOK, true},
		{"the app redirects", http.MethodGet, "/webhooks/moved", nil, http.StatusFound, true},
		{"hostile", http.MethodGet, "/webhooks/" + strings.Repeat("a", 8000), hostile, http.StatusOK, true},
		{"escaped", http.MethodGet, "/webhooks/" + strings.Repeat("\xff", 8000), escaped, http.StatusOK, true},
	}
	for _, r := range requests {
		if got := send(r.method, appHost, r.path, r.header); got != r.status {
			t.Fatalf("%s: status %d, want %d\n--- caddy ---\n%s", r.name, got, r.status, caddyLog.String())
		}
	}
	if got := send(http.MethodGet, stoppedHost, "/", nil); got != http.StatusServiceUnavailable {
		t.Fatalf("stopped app: status %d", got)
	}

	readLog := func(slug string, want int) [][]byte {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			out, err := exec.Command("docker", "exec", container, "cat", logDir+"/"+slug+".log").Output()
			lines := bytes.Split(bytes.TrimRight(out, "\n"), []byte("\n"))
			if err == nil && len(lines) >= want && len(out) > 0 {
				return lines
			}
			if time.Now().After(deadline) {
				t.Fatalf("access log of %s: %d lines, want %d (%v)\n--- caddy ---\n%s", slug, len(lines), want, err, caddyLog.String())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	type entry struct {
		TS      float64 `json:"ts"`
		Request struct {
			RemoteIP string `json:"remote_ip"`
			ClientIP string `json:"client_ip"`
			Proto    string `json:"proto"`
			Method   string `json:"method"`
			Host     string `json:"host"`
			URI      string `json:"uri"`
		} `json:"request"`
		Status    int      `json:"status"`
		Duration  *float64 `json:"duration"`
		Size      *int     `json:"size"`
		UserAgent string   `json:"user_agent"`
		Location  string   `json:"location"`
	}
	parse := func(line []byte) (entry, map[string]json.RawMessage) {
		t.Helper()
		var e entry
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatalf("the entry is not JSON: %v", err)
		}
		_ = json.Unmarshal(line, &raw)
		return e, raw
	}

	lines := readLog("shop-staging", len(requests))
	if len(lines) != len(requests) {
		t.Fatalf("%d entries for %d requests", len(lines), len(requests))
	}
	for i, line := range lines {
		r := requests[i]
		// The collector keeps lines of 16 KiB (ARCHITECTURE 10.1); an
		// entry is far below that.
		if len(line) > 2560 {
			t.Errorf("%s: the entry is %d bytes", r.name, len(line))
		}
		if bytes.Contains(line, []byte(secret)) {
			t.Errorf("%s: the entry holds a header value", r.name)
		}
		e, raw := parse(line)
		var request map[string]json.RawMessage
		_ = json.Unmarshal(raw["request"], &request)
		for _, gone := range []string{"headers", "tls"} {
			if _, ok := request[gone]; ok {
				t.Errorf("%s: the entry has request.%s", r.name, gone)
			}
		}
		if _, ok := raw["resp_headers"]; ok {
			t.Errorf("%s: the entry has resp_headers", r.name)
		}
		if e.TS < float64(now.Unix()) || e.Request.Method != r.method || e.Request.Host != appHost ||
			e.Status != r.status || e.Request.ClientIP != "127.0.0.1" || e.Request.RemoteIP != "127.0.0.1" ||
			e.Request.Proto == "" || e.Duration == nil || e.Size == nil {
			t.Errorf("%s: entry = %+v", r.name, e)
		}
		if len(r.path) < 100 && e.Request.URI != r.path {
			t.Errorf("%s: uri = %q", r.name, e.Request.URI)
		}
	}
	gate, _ := parse(lines[0])
	if gate.UserAgent != "koroku-test/1.0" || !strings.HasPrefix(gate.Location, "https://"+consoleHost+"/auth/gate?") {
		t.Errorf("gate redirect: user_agent %q, location %q", gate.UserAgent, gate.Location)
	}
	if moved, _ := parse(lines[3]); moved.Location != "https://"+appHost+"/login" {
		t.Errorf("the app's redirect: location %q", moved.Location)
	}
	// Long values are cut to a fixed length, and anything that would be
	// escaped ends the value.
	hostileEntry, _ := parse(lines[4])
	if got := hostileEntry.Request.URI; got != "/webhooks/"+strings.Repeat("a", 990)+"..." {
		t.Errorf("hostile: the uri has %d bytes", len(got))
	}
	if got := hostileEntry.UserAgent; got != strings.Repeat("A", 200)+"..." {
		t.Errorf("hostile: the user agent has %d bytes", len(got))
	}
	escapedEntry, _ := parse(lines[5])
	if escapedEntry.Request.URI != "/webhooks/..." || escapedEntry.UserAgent != "agent ..." {
		t.Errorf("escaped: uri %q, user_agent %q", escapedEntry.Request.URI, escapedEntry.UserAgent)
	}
	t.Logf("entry sizes: plain %d bytes, hostile %d bytes, escaped %d bytes", len(lines[0]), len(lines[4]), len(lines[5]))

	if e, _ := parse(readLog("paused", 1)[0]); e.Status != http.StatusServiceUnavailable || e.Request.Host != stoppedHost {
		t.Errorf("stopped app: entry = %+v", e)
	}

	// The idle tracker reads these entries as they are.
	clk := clock.NewFake(now)
	tracker := &idle.Tracker{Store: st, Clock: clk, ConsoleURL: cfg.ConsoleURL}
	tracker.Check(ctx) // learns that the app is protected
	for i, line := range lines {
		clk.Advance(time.Minute)
		tracker.Observe("shop-staging", line)
		last, _ := tracker.LastActive("shop-staging")
		if counted := last.Equal(clk.Now()); counted != requests[i].counts {
			t.Errorf("%s: counted as a request = %v, want %v", requests[i].name, counted, requests[i].counts)
		}
	}
}
