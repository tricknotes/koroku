//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/caddy"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/config"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/routing"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
	"github.com/tricknotes/koroku/internal/system"
)

// patchCaddyPorts moves a generated configuration off ports 80, 443 and
// 2019, as startCaddy does.
func patchCaddyPorts(t *testing.T, config []byte, httpPort, httpsPort, adminPort int) []byte {
	t.Helper()
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
	return patched
}

// portLoader loads configurations into a test Caddy through its admin API,
// with the ports moved like the configuration it started with.
type portLoader struct {
	t                             *testing.T
	httpPort, httpsPort, adminPort int
}

func (l *portLoader) Load(ctx context.Context, config []byte) error {
	client := &caddy.Client{AdminURL: fmt.Sprintf("http://127.0.0.1:%d", l.adminPort)}
	return client.Load(ctx, patchCaddyPorts(l.t, config, l.httpPort, l.httpsPort, l.adminPort))
}

// bgStack is a koroku stack (store, engine with a router, real Caddy) for
// one blue/green app. Its worker can be stopped and started again, like
// koroku restarting.
type bgStack struct {
	t      *testing.T
	db     *store.Store
	opts   deploy.Options
	engine *deploy.Engine
	stop   func()
	slug   string
	app    store.App
}

func newBGStack(t *testing.T) *bgStack {
	t.Helper()
	if out, err := exec.Command("docker", "compose", "version").CombinedOutput(); err != nil {
		t.Skipf("docker compose is not available: %v: %s", err, out)
	}
	const image = "caddy:2"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		if out, err := exec.Command("docker", "pull", "--quiet", image).CombinedOutput(); err != nil {
			t.Skipf("the %s image is not available: %v: %s", image, err, out)
		}
	}
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dir, "koroku.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	key := make([]byte, secrets.KeySize)
	_, _ = rand.Read(key)
	box, err := secrets.NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.BaseDomain = "dev.example.com"
	cfg.DataDir = dir

	loader := &portLoader{t: t, httpPort: freePort(t), httpsPort: freePort(t), adminPort: freePort(t)}
	router := &routing.Router{Store: st, Loader: loader, Settings: routing.Settings{
		BaseDomain:      cfg.BaseDomain,
		ConsoleHost:     "console.dev.example.com",
		ConsoleUpstream: fmt.Sprintf("127.0.0.1:%d", freePort(t)),
		ListenIP:        "127.0.0.1",
		InternalCA:      true,
	}}
	initial, err := router.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	container := fmt.Sprintf("koroku-bluegreen-test-%d", loader.httpsPort)
	cmd := exec.Command("docker", "run", "--rm", "-i", "--name", container, "--network", "host", image,
		"sh", "-c", "cat > /tmp/caddy.json && exec caddy run --config /tmp/caddy.json")
	cmd.Stdin = bytes.NewReader(patchCaddyPorts(t, initial, loader.httpPort, loader.httpsPort, loader.adminPort))
	var caddyLog bytes.Buffer
	cmd.Stdout, cmd.Stderr = &caddyLog, &caddyLog
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", container).Run()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("--- caddy ---\n%s", caddyLog.String())
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for router.ForceSync(ctx) != nil {
		if time.Now().After(deadline) {
			t.Fatalf("caddy did not come up\n%s", caddyLog.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	run := runner.Exec{}
	fetcher := gitrepo.Fetcher{Runner: run, AllowFileURLs: true}
	s := &bgStack{t: t, db: st, opts: deploy.Options{
		Store: st, Runner: run, Clock: clock.System{}, Box: box,
		System: system.Host{DataDir: dir, Runner: run}, Fetcher: fetcher, Router: router,
		AppsDir: cfg.AppsDir(), BaseDomain: cfg.BaseDomain,
		DiskLimitPercent: 100, WaitTimeout: 90 * time.Second, HealthTimeout: 30 * time.Second,
		Logger: slog.New(slog.NewTextHandler(t.Output(), nil)),
	}}

	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	s.slug = "it-bg-" + hex.EncodeToString(suffix)
	t.Cleanup(func() {
		out, err := exec.Command("docker", "compose", "-p", "koroku-"+s.slug,
			"down", "--volumes", "--remove-orphans", "--rmi", "local", "--timeout", "1").CombinedOutput()
		if err != nil {
			t.Logf("cleanup failed: %v\n%s", err, out)
		}
	})
	s.startWorker()
	t.Cleanup(func() { s.stop() })
	return s
}

func (s *bgStack) startWorker() {
	ctx, cancel := context.WithCancel(context.Background())
	s.engine = deploy.New(s.opts)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = s.engine.Run(ctx)
	}()
	s.stop = func() {
		cancel()
		<-done
	}
}

func (s *bgStack) reload() store.App {
	s.t.Helper()
	app, err := s.db.GetApp(context.Background(), s.slug)
	if err != nil {
		s.t.Fatal(err)
	}
	s.app = app
	return app
}

func (s *bgStack) enqueue(sha string) store.Deployment {
	s.t.Helper()
	d, err := s.engine.Enqueue(context.Background(), s.reload(), "main", sha, "")
	if err != nil {
		s.t.Fatal(err)
	}
	return d
}

func (s *bgStack) wait(id int64) store.Deployment {
	s.t.Helper()
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		d, err := s.db.GetDeployment(context.Background(), id)
		if err != nil {
			s.t.Fatal(err)
		}
		if store.IsTerminal(d.Status) {
			// The worker cleans up after recording the result.
			time.Sleep(time.Second)
			return d
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.t.Fatalf("deployment %d did not finish:\n%s", id, s.log(id))
	return store.Deployment{}
}

func (s *bgStack) log(id int64) string {
	data, _ := readFileString(s.engine.LogPath(s.slug, id))
	return data
}

func readFileString(path string) (string, error) {
	out, err := exec.Command("cat", path).Output()
	return string(out), err
}

// containerCreated returns when the container of service was created, ""
// without one.
func (s *bgStack) containerCreated(service string) string {
	s.t.Helper()
	out, err := exec.Command("docker", "ps", "--all", "--no-trunc",
		"--filter", "label=com.docker.compose.project=koroku-"+s.slug,
		"--filter", "label=com.docker.compose.service="+service,
		"--format", "{{.ID}} {{.CreatedAt}}").Output()
	if err != nil {
		s.t.Fatalf("docker ps: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// prober sends a GET through Caddy every 0.2 seconds and keeps every
// answer.
type prober struct {
	mu      sync.Mutex
	results []probeResult
	stop    chan struct{}
	done    chan struct{}
}

type probeResult struct {
	at     time.Time
	status int
	body   string
	err    error
}

func startProber(t *testing.T, httpsPort int, host string) *prober {
	client := &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", httpsPort))
			},
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Caddy's local CA
			DisableKeepAlives: true,
		},
	}
	p := &prober{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(p.done)
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
			}
			r := probeResult{at: time.Now()}
			res, err := client.Get("https://" + host + "/")
			if err != nil {
				r.err = err
			} else {
				b, _ := io.ReadAll(res.Body)
				_ = res.Body.Close()
				r.status, r.body = res.StatusCode, string(b)
			}
			p.mu.Lock()
			p.results = append(p.results, r)
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *prober) snapshot() []probeResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.results)
}

// failures are the answers since index from that were not a 200 of the app.
func (p *prober) failuresSince(from int) []string {
	var out []string
	for _, r := range p.snapshot()[from:] {
		if r.err != nil || r.status != http.StatusOK || !strings.Contains(r.body, "version=") {
			out = append(out, fmt.Sprintf("%s: %d %v %q", r.at.Format("15:04:05.000"), r.status, r.err, r.body))
		}
	}
	return out
}

func (p *prober) last() string {
	results := p.snapshot()
	if len(results) == 0 {
		return ""
	}
	return results[len(results)-1].body
}

// waitFor waits until the prober saw a body containing want.
func (p *prober) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(p.last(), want) {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the app never answered %q; last answer %q", want, p.last())
}

// TestBlueGreenDeploysWithoutDowntime walks the completion criteria of M6
// with a stand-in app (slow start, database, one-shot migration) behind a
// real Caddy, sending a request every 0.2 seconds (ARCHITECTURE 7.2).
func TestBlueGreenDeploysWithoutDowntime(t *testing.T) {
	t.Parallel()
	s := newBGStack(t)
	ctx := context.Background()
	repo := newRepo(t, "bluegreen-app")
	first := repo.commit("version 1")

	portStart, portEnd := portRange(t)
	app, err := s.db.CreateApp(ctx, store.App{
		Slug: s.slug, RepoURL: repo.url(), DefaultBranch: "main",
		WebService: "web", WebPort: 8080, HealthcheckPath: "/",
		BlueGreen: true, BlueGreenDrainSeconds: 1,
	}, portStart, portEnd, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	app.BlueGreen = true
	app.BlueGreenDrainSeconds = 1
	if err := s.db.UpdateApp(ctx, app, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.AllocateAltHostPort(ctx, app.ID, portStart, portEnd); err != nil {
		t.Fatal(err)
	}

	d1 := s.wait(s.enqueue(first).ID)
	if d1.Status != store.StatusSucceeded || d1.WebColor != store.ColorBlue {
		t.Fatalf("first deployment: %+v\n%s", d1, s.log(d1.ID))
	}
	loader := s.opts.Router.(*routing.Router).Loader.(*portLoader)
	p := startProber(t, loader.httpsPort, s.slug+".dev.example.com")
	defer func() {
		close(p.stop)
		<-p.done
	}()
	p.waitFor(t, "version=1")

	// Other services reach the public service by its name.
	if out, err := exec.Command("docker", "compose", "-p", "koroku-"+s.slug, "exec", "-T", "db",
		"wget", "-qO-", "http://web:8080/").CombinedOutput(); err != nil || !strings.Contains(string(out), "version=1") {
		t.Errorf("web by name from db: %v %s", err, out)
	}

	check := func(name string, from int) {
		t.Helper()
		if failures := p.failuresSince(from); len(failures) > 0 {
			t.Errorf("%s: %d failed requests:\n%s", name, len(failures), strings.Join(failures, "\n"))
		}
	}

	// (1) A code-only change: the database container stays.
	dbBefore := s.containerCreated("db")
	repo.write("VERSION", "2\n")
	from := len(p.snapshot())
	d2 := s.wait(s.enqueue(repo.commit("version 2")).ID)
	if d2.Status != store.StatusSucceeded || d2.WebColor != store.ColorGreen || d2.SwitchedAt == nil {
		t.Fatalf("code-only deployment: %+v\n%s", d2, s.log(d2.ID))
	}
	p.waitFor(t, "version=2")
	check("code-only deployment", from)
	if after := s.containerCreated("db"); after != dbBefore || after == "" {
		t.Errorf("the database container was recreated: %q -> %q\n%s", dbBefore, after, s.log(d2.ID))
	}
	if blue := s.containerCreated("web-blue"); blue != "" {
		t.Errorf("the old colour is left: %s", blue)
	}

	// (2) An additive migration runs before the new colour starts.
	repo.write("VERSION", "3\n")
	repo.write("MIGRATIONS", "001_create_posts\n002_add_title\n")
	from = len(p.snapshot())
	d3 := s.wait(s.enqueue(repo.commit("version 3 with a migration")).ID)
	if d3.Status != store.StatusSucceeded || d3.WebColor != store.ColorBlue {
		t.Fatalf("migration deployment: %+v\n%s", d3, s.log(d3.ID))
	}
	p.waitFor(t, "migrations=001_create_posts,002_add_title,")
	check("migration deployment", from)
	if after := s.containerCreated("db"); after != dbBefore {
		t.Errorf("the database container was recreated: %q -> %q", dbBefore, after)
	}

	// (3) A commit that does not start: failed, the old colour answers, no
	// new colour is left.
	repo.write("VERSION", "broken\n")
	from = len(p.snapshot())
	d4 := s.wait(s.enqueue(repo.commit("broken")).ID)
	if d4.Status != store.StatusFailed || strings.Contains(d4.Error, "restored") {
		t.Fatalf("broken deployment: %+v\n%s", d4, s.log(d4.ID))
	}
	time.Sleep(2 * time.Second)
	check("broken deployment", from)
	if green := s.containerCreated("web-green"); green != "" {
		t.Errorf("the failed colour is left: %s\n%s", green, s.log(d4.ID))
	}
	if !strings.Contains(p.last(), "version=3") || s.reload().WebColor != store.ColorBlue {
		t.Errorf("after the failure: %q, colour %s", p.last(), s.app.WebColor)
	}

	// (4) Instant rollback to version 2.
	from = len(p.snapshot())
	rb, err := s.engine.EnqueueRollback(ctx, s.reload(), d2.ID)
	if err != nil {
		t.Fatal(err)
	}
	rb = s.wait(rb.ID)
	if rb.Status != store.StatusSucceeded || rb.WebColor != store.ColorGreen {
		t.Fatalf("rollback: %+v\n%s", rb, s.log(rb.ID))
	}
	p.waitFor(t, "version=2")
	check("rollback", from)

	// (5) koroku stops while a deployment runs the new colour; once it
	// starts again, the colour that receives no requests is gone and the
	// live one kept answering.
	repo.write("VERSION", "5\n")
	from = len(p.snapshot())
	d5 := s.enqueue(repo.commit("version 5"))
	deadline := time.Now().Add(3 * time.Minute)
	for s.containerCreated("web-blue") == "" {
		if time.Now().After(deadline) {
			t.Fatalf("the new colour never started:\n%s", s.log(d5.ID))
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.stop()
	s.startWorker()
	interrupted := s.wait(d5.ID)
	if interrupted.Status != store.StatusFailed || interrupted.Error != store.InterruptedError {
		t.Errorf("interrupted deployment: %+v", interrupted)
	}
	deadline = time.Now().Add(time.Minute)
	for s.containerCreated("web-blue") != "" {
		if time.Now().After(deadline) {
			t.Fatalf("the colour that receives no requests is left after the restart")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if s.reload().WebColor != store.ColorGreen || !strings.Contains(p.last(), "version=2") {
		t.Errorf("after the restart: colour %s, answer %q", s.app.WebColor, p.last())
	}
	check("restart during a deployment", from)
	t.Logf("%d requests, all answered", len(p.snapshot()))

	// Stop, start and restart run the live colour only: `start` would fail
	// on the colour without a container.
	if err := s.engine.Stop(ctx, s.reload()); err != nil {
		t.Fatal(err)
	}
	if err := s.engine.Start(ctx, s.reload()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.engine.Restart(ctx, s.reload()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	p.waitFor(t, "version=2")
	if blue := s.containerCreated("web-blue"); blue != "" || s.containerCreated("web") != "" {
		t.Errorf("start created another form of the public service: blue %q", blue)
	}
}
