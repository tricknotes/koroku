//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/api"
	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/routing"
	"github.com/tricknotes/koroku/internal/store"
)

// serveOn starts h on a fresh loopback port and returns the port.
func serveOn(t *testing.T, h http.Handler) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return l.Addr().(*net.TCPAddr).Port
}

// startCaddy runs a real Caddy in a container with a generated
// configuration, moved off ports 80, 443 and 2019 (which a development
// stack on this machine may hold). It returns the HTTPS port and Caddy's
// output.
func startCaddy(t *testing.T, image, name string, config []byte) (int, *bytes.Buffer) {
	t.Helper()
	httpPort, httpsPort, adminPort := freePort(t), freePort(t), freePort(t)
	var doc map[string]any
	if err := json.Unmarshal(config, &doc); err != nil {
		t.Fatal(err)
	}
	doc["admin"] = map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", adminPort)}
	httpApp := doc["apps"].(map[string]any)["http"].(map[string]any)
	httpApp["http_port"], httpApp["https_port"] = httpPort, httpsPort
	servers := httpApp["servers"].(map[string]any)
	servers["http"].(map[string]any)["listen"] = []string{fmt.Sprintf("127.0.0.1:%d", httpPort)}
	servers["https"].(map[string]any)["listen"] = []string{fmt.Sprintf("127.0.0.1:%d", httpsPort)}
	patched, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	container := fmt.Sprintf("koroku-%s-test-%d", name, httpsPort)
	cmd := exec.Command("docker", "run", "--rm", "-i", "--name", container, "--network", "host", image,
		"sh", "-c", "cat > /tmp/caddy.json && exec caddy run --config /tmp/caddy.json")
	cmd.Stdin = bytes.NewReader(patched)
	var caddyLog bytes.Buffer
	cmd.Stdout, cmd.Stderr = &caddyLog, &caddyLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", container).Run()
		_ = cmd.Wait()
	})
	return httpsPort, &caddyLog
}

// TestAccessProtectionThroughCaddy runs a real Caddy with the generated
// configuration in front of koroku and a stand-in app, and walks the login
// of a protected staging app (ARCHITECTURE 8.3).
func TestAccessProtectionThroughCaddy(t *testing.T) {
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

	// The app echoes what reaches it.
	appPort := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "app path=%s cookie=%q", r.URL.RequestURI(), r.Header.Get("Cookie"))
	}))
	publicPort := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "public app")
	}))

	create := func(slug, env string, port int, protected bool, exempt []string) {
		app, err := st.CreateApp(ctx, store.App{
			Slug: slug, RepoFullName: "octo/" + slug, DefaultBranch: "main", WebService: "web",
			WebPort: 3000, HealthcheckPath: "/", Environment: env,
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
	}
	create("shop-staging", store.EnvironmentStaging, appPort, true, []string{"/webhooks/"})
	create("shop", store.EnvironmentProduction, publicPort, false, nil)

	const (
		base        = "dev.example.com"
		consoleHost = "console." + base
		appHost     = "shop-staging." + base
		publicHost  = "shop." + base
	)
	// A console session of the allowed user, as a login would leave it.
	token := make([]byte, 32)
	_, _ = rand.Read(token)
	sessionToken := hex.EncodeToString(token)
	sum := sha256.Sum256([]byte(sessionToken))
	if err := st.CreateSession(ctx, store.Session{
		TokenHash: hex.EncodeToString(sum[:]), ID: "0123456789abcdef", GitHubUserID: 42,
		CreatedAt: now, ExpiresAt: now.Add(api.SessionLifetime),
	}); err != nil {
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

	loader := &captureLoader{}
	router := &routing.Router{Store: st, Loader: loader, Settings: routing.Settings{
		BaseDomain:      base,
		ConsoleHost:     consoleHost,
		ConsoleUpstream: fmt.Sprintf("127.0.0.1:%d", korokuPort),
		ListenIP:        "127.0.0.1",
		InternalCA:      true,
	}}
	if err := router.ForceSync(ctx); err != nil {
		t.Fatalf("ForceSync: %v", err)
	}

	httpsPort, caddyLog := startCaddy(t, image, "gate", loader.config)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			// Every hostname is this Caddy; its certificates come from its
			// own CA.
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpsPort))
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Caddy's local CA
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	type result struct {
		status int
		header http.Header
		body   string
	}
	do := func(method, rawURL string, opaque string, cookies ...string) result {
		t.Helper()
		req, err := http.NewRequest(method, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if opaque != "" {
			req.URL.Opaque = opaque // sent as is, without cleaning
		}
		if len(cookies) > 0 {
			req.Header.Set("Cookie", strings.Join(cookies, "; "))
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v\n--- caddy ---\n%s", method, rawURL, err, caddyLog.String())
		}
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		return result{res.StatusCode, res.Header, string(body)}
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, "https://"+publicHost+"/", nil)
		res, err := client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("caddy did not come up: %v\n%s", err, caddyLog.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	// The public production app: proxied, no noindex.
	if r := do(http.MethodGet, "https://"+publicHost+"/", ""); r.status != 200 || r.header.Get("X-Robots-Tag") != "" {
		t.Errorf("public app: %d %v", r.status, r.header)
	}

	// Without the cookie: a page load goes to the console, a POST gets 401.
	r := do(http.MethodGet, "https://"+appHost+"/secret?x=1", "")
	wantLoc := "https://" + consoleHost + "/auth/gate?" + url.Values{"host": {appHost}, "return": {"/secret?x=1"}}.Encode()
	if r.status != http.StatusFound || r.header.Get("Location") != wantLoc {
		t.Fatalf("protected without cookie: %d %q", r.status, r.header.Get("Location"))
	}
	if r.header.Get("X-Robots-Tag") != "noindex" {
		t.Errorf("staging response lacks noindex: %v", r.header)
	}
	if r := do(http.MethodPost, "https://"+appHost+"/secret", ""); r.status != http.StatusUnauthorized {
		t.Errorf("POST without cookie: %d", r.status)
	}
	// Exempt paths pass; a crafted path does not escape through them.
	if r := do(http.MethodPost, "https://"+appHost+"/webhooks/github", ""); r.status != 200 || !strings.Contains(r.body, "path=/webhooks/github") {
		t.Errorf("exempt path: %d %s", r.status, r.body)
	}
	if r := do(http.MethodGet, "https://"+appHost+"/", "/webhooks/../secret"); r.status != http.StatusFound {
		t.Errorf("/webhooks/../secret: %d %s", r.status, r.body)
	}
	if r := do(http.MethodGet, "https://"+appHost+"/", "/webhooks/%2e%2e/secret"); r.status != http.StatusFound {
		t.Errorf("/webhooks/%%2e%%2e/secret: %d %s", r.status, r.body)
	}
	// koroku's prefix never reaches the app.
	for _, p := range []string{"/.koroku", "/.koroku/", "/.koroku/other"} {
		if r := do(http.MethodGet, "https://"+appHost+p, ""); r.status != http.StatusNotFound || strings.Contains(r.body, "app path") {
			t.Errorf("%s: %d %s", p, r.status, r.body)
		}
	}

	// Caddy routes on the decoded, cleaned, lower-cased path and forwards
	// the request as it was sent. No spelling of a path gets a request
	// without a login to the app, with or without the header of the wake
	// route (ARCHITECTURE 8.4), and only the plain spelling of an exempt
	// path skips the gate.
	crafted := func(target string, wake bool) result {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, "https://"+appHost+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.URL.Opaque = target
		if wake {
			req.Header.Set(caddy.WakeHeader, "shop-staging")
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v\n--- caddy ---\n%s", target, err, caddyLog.String())
		}
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		return result{res.StatusCode, res.Header, string(body)}
	}
	for _, c := range []struct {
		target string
		wake   bool
		want   int
	}{
		// Other spellings of koroku's own paths.
		{"/graphql/../.koroku/auth/x?q=1", true, http.StatusNotFound},
		{"/graphql/..%2F.koroku/auth/x?q=1", true, http.StatusNotFound},
		{"/.KOROKU/auth/x", true, http.StatusNotFound},
		{"/graphql/../.koroku/auth/x?q=1", false, http.StatusNotFound},
		{"/.KOROKU/auth/x", false, http.StatusNotFound},
		{"/.koroku/auth/x", true, http.StatusNotFound},
		{"/x/../.koroku", true, http.StatusNotFound},
		// The header alone opens nothing.
		{"/secret", true, http.StatusUnauthorized},
		{"/auth/gate/check", true, http.StatusUnauthorized},
		{"/AUTH/GATE/CHECK", true, http.StatusUnauthorized},
		// Paths that only resolve to an exempt prefix, or reach it in
		// another spelling.
		{"/admin/..%2Fwebhooks/x", false, http.StatusUnauthorized},
		{"/admin/../webhooks/x", false, http.StatusUnauthorized},
		{"/admin/..%2Fwebhooks/x", true, http.StatusUnauthorized},
		{"/admin/../webhooks/x", true, http.StatusUnauthorized},
		{"/WEBHOOKS/x", false, http.StatusUnauthorized},
		{"/Webhooks/x", true, http.StatusUnauthorized},
		{"/%77ebhooks/x", false, http.StatusUnauthorized},
		{"/webhooks%2Fx", false, http.StatusUnauthorized},
		// An exempt prefix followed by something an app could resolve to
		// another path.
		{"/webhooks/../webhooks/x", false, http.StatusUnauthorized},
		{"/webhooks/./x", false, http.StatusUnauthorized},
		{"/webhooks//x", false, http.StatusUnauthorized},
		{"/webhooks/x/..", false, http.StatusUnauthorized},
		{"/webhooks/x/..?a=1", false, http.StatusUnauthorized},
		{"/webhooks/%2E%2E/secret", false, http.StatusUnauthorized},
		{"/webhooks/x%2e", false, http.StatusUnauthorized},
		{"/webhooks/a%2Fb", false, http.StatusUnauthorized},
		{"/webhooks/a%2fb", false, http.StatusUnauthorized},
		{"/webhooks/a%5Cb", false, http.StatusUnauthorized},
		{"/webhooks/a%5cb", false, http.StatusUnauthorized},
		{`/webhooks/a\..\b`, false, http.StatusUnauthorized},
		{"/webhooks/..;/secret", false, http.StatusUnauthorized},
		{"/webhooks/.;x/secret", false, http.StatusUnauthorized},
		{"/webhooks/{http.request.uri.query}/..%2Fx?a", false, http.StatusUnauthorized},
		// The plain spelling: dots inside a segment, other escapes and
		// anything in the query are the app's business.
		{"/webhooks/x", true, http.StatusOK},
		{"/webhooks/v1.2/..a/b..c/x%20y", false, http.StatusOK},
		{"/webhooks/x?next=https%3A%2F%2Fexample.com%2F..%2F&dir=a//../b%5C", false, http.StatusOK},
	} {
		r := crafted(c.target, c.wake)
		reached := strings.Contains(r.body, "app path=")
		if r.status != c.want || reached != (c.want == http.StatusOK) {
			t.Errorf("POST %s (wake header %v): %d %q, want %d", c.target, c.wake, r.status, r.body, c.want)
		}
		if reached && !strings.Contains(r.body, "path="+c.target+" ") {
			t.Errorf("POST %s reached the app as another path: %s", c.target, r.body)
		}
	}

	// The console hands out a one-time code for the app host.
	sessionCookie := "__Host-koroku_session=" + sessionToken
	r = do(http.MethodGet, wantLoc, "", sessionCookie)
	if r.status != http.StatusSeeOther {
		t.Fatalf("console gate: %d %s", r.status, r.body)
	}
	callback := r.header.Get("Location")
	if !strings.HasPrefix(callback, "https://"+appHost+"/.koroku/auth/callback?") {
		t.Fatalf("console gate redirects to %q", callback)
	}
	r = do(http.MethodGet, callback, "")
	if r.status != http.StatusSeeOther || r.header.Get("Location") != "/secret?x=1" {
		t.Fatalf("callback: %d %q %s", r.status, r.header.Get("Location"), r.body)
	}
	var gate string
	for _, c := range (&http.Response{Header: r.header}).Cookies() {
		if c.Name == caddy.GateCookie {
			gate = c.Name + "=" + c.Value
		}
	}
	if gate == "" {
		t.Fatalf("no gate cookie: %v", r.header)
	}
	if r := do(http.MethodGet, callback, ""); r.status != http.StatusBadRequest {
		t.Errorf("code used twice: %d", r.status)
	}

	// With the cookie the app answers, and never sees the gate cookie.
	for _, cookies := range [][]string{{gate}, {gate, "a=1"}, {"a=1", gate, "b=2"}} {
		r := do(http.MethodGet, "https://"+appHost+"/secret?x=1", "", cookies...)
		if r.status != 200 || !strings.Contains(r.body, "path=/secret?x=1") {
			t.Errorf("with cookie %v: %d %s", cookies, r.status, r.body)
			continue
		}
		if strings.Contains(r.body, "koroku_gate") {
			t.Errorf("the gate cookie reached the app: %s", r.body)
		}
		if strings.Contains(strings.Join(cookies, ";"), "a=1") && !strings.Contains(r.body, "a=1") {
			t.Errorf("another cookie was lost: %s", r.body)
		}
		if r.header.Get("X-Robots-Tag") != "noindex" {
			t.Errorf("proxied staging response lacks noindex")
		}
	}
	if r := do(http.MethodPost, "https://"+appHost+"/secret", "", gate); r.status != 200 {
		t.Errorf("POST with the cookie: %d", r.status)
	}
	// The cookie does not open the console's API either (another host).
	if r := do(http.MethodGet, "https://"+consoleHost+"/api/apps", "", gate); r.status != http.StatusUnauthorized {
		t.Errorf("gate cookie on the console: %d", r.status)
	}

	// Logging out of the console ends the grant.
	req, _ := http.NewRequest(http.MethodPost, "https://"+consoleHost+"/auth/logout", nil)
	req.Header.Set("Origin", "https://"+consoleHost)
	req.Header.Set("Cookie", sessionCookie)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("console logout: %d", res.StatusCode)
	}
	if r := do(http.MethodGet, "https://"+appHost+"/secret", "", gate); r.status != http.StatusFound {
		t.Errorf("after console logout: %d", r.status)
	}
}

// TestWakeHeaderReachesKorokuOnlyFromTheWakeRoute runs a real Caddy with a
// stand-in for koroku that records the wake header of every request it
// gets: the header of a client is deleted on the console route, the login
// endpoints and the gate check, and replaced on the wake route
// (ARCHITECTURE 8.4).
func TestWakeHeaderReachesKorokuOnlyFromTheWakeRoute(t *testing.T) {
	t.Parallel()
	const image = "caddy:2"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		if out, err := exec.Command("docker", "pull", "--quiet", image).CombinedOutput(); err != nil {
			t.Skipf("the %s image is not available: %v: %s", image, err, out)
		}
	}

	type seen struct{ host, uri, wake string }
	var mu sync.Mutex
	var got []seen
	korokuPort := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, seen{r.Host, r.URL.RequestURI(), strings.Join(r.Header.Values(caddy.WakeHeader), ",")})
		mu.Unlock()
		if r.URL.Path == caddy.GateCheckPath {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = io.WriteString(w, "koroku")
	}))
	appPort := serveOn(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "app")
	}))

	const (
		consoleHost   = "console.dev.example.com"
		protectedHost = "shop-staging.dev.example.com"
		sleepingHost  = "blog.dev.example.com"
	)
	config, err := caddy.Generate(caddy.Input{
		ConsoleHost:     consoleHost,
		ConsoleUpstream: fmt.Sprintf("127.0.0.1:%d", korokuPort),
		ListenIP:        "127.0.0.1",
		InternalCA:      true,
		Apps: []caddy.App{
			{Slug: "shop-staging", HostPort: appPort, Hostnames: []string{protectedHost}, Protected: true},
			{Slug: "blog", HostPort: freePort(t), Hostnames: []string{sleepingHost}, Sleeping: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	httpsPort, caddyLog := startCaddy(t, image, "wake-header", config)

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpsPort))
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Caddy's local CA
		},
	}
	get := func(host, target string) (int, string, error) {
		req, err := http.NewRequest(http.MethodGet, "https://"+host+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set(caddy.WakeHeader, "from-the-client")
		res, err := client.Do(req)
		if err != nil {
			return 0, "", err
		}
		defer func() { _ = res.Body.Close() }()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body), nil
	}

	deadline := time.Now().Add(30 * time.Second)
	for {
		status, _, err := get(consoleHost, "/")
		if err == nil && status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("caddy did not come up: %v\n%s", err, caddyLog.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	mu.Lock()
	got = nil
	mu.Unlock()

	for _, c := range []struct{ host, target, body string }{
		{consoleHost, "/api/apps", "koroku"},
		{protectedHost, "/.koroku/auth/callback?code=x", "koroku"},
		{protectedHost, "/page", "app"},
		{sleepingHost, "/page", "koroku"},
	} {
		status, body, err := get(c.host, c.target)
		if err != nil || status != http.StatusOK || body != c.body {
			t.Fatalf("GET %s%s: %d %q %v\n--- caddy ---\n%s", c.host, c.target, status, body, err, caddyLog.String())
		}
	}
	want := []seen{
		{consoleHost, "/api/apps", ""},
		{protectedHost, "/.koroku/auth/callback?code=x", ""},
		{protectedHost, caddy.GateCheckPath, ""},
		{sleepingHost, "/page", "blog"},
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(got, want) {
		t.Errorf("koroku received %+v, want %+v", got, want)
	}
}
