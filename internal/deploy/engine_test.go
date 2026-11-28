package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/compose"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
	"github.com/tricknotes/koroku/internal/system"
)

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shaC = "cccccccccccccccccccccccccccccccccccccccc"
)

type fakeProber struct {
	mu      sync.Mutex
	percent float64
	err     error
	free    *uint64
}

func (p *fakeProber) Info(context.Context) system.Info { return system.Info{} }

// AvailableMemory reports plenty unless a test set free.
func (p *fakeProber) AvailableMemory() (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.free == nil {
		return 1 << 40, nil
	}
	return *p.free, nil
}

func (p *fakeProber) DiskUsage() (system.Disk, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return system.Disk{TotalBytes: 100, UsedBytes: uint64(p.percent), UsedPercent: p.percent}, p.err
}

type fakeHealth struct {
	mu   sync.Mutex
	err  error
	urls []string
}

func (h *fakeHealth) Wait(_ context.Context, url string, _ time.Duration) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.urls = append(h.urls, url)
	return h.err
}

type fakeRouter struct {
	mu    sync.Mutex
	calls int
	err   error
	// onSync, when set, runs on every call (to look at the database the
	// real router would build the configuration from).
	onSync func()
}

func (r *fakeRouter) Sync(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.onSync != nil {
		r.onSync()
	}
	return r.err
}

type fakeTokens struct{ token string }

func (f fakeTokens) RepoToken(context.Context, store.App) (string, error) { return f.token, nil }

// harness wires an Engine to fakes. The fake runner plays git and docker.
type harness struct {
	t      *testing.T
	store  *store.Store
	engine *Engine
	runner *runner.Fake
	health *fakeHealth
	prober *fakeProber
	router *fakeRouter
	box    *secrets.Box
	clock  *clock.Fake
	dir    string

	mu sync.Mutex
	// repoServices is what the repository's Compose file declares.
	repoServices string
	// servicesFor, when set, replaces repoServices per config call (to
	// interpolate the env file the call was given).
	servicesFor func(cmd runner.Cmd) string
	// onCompose lets a test fail or delay a compose subcommand.
	onCompose func(ctx context.Context, sub string, cmd runner.Cmd) error
	// buildOutput is printed by `docker compose build`.
	buildOutput string
	// onFetch lets a test add files to the checkout, as a repository would.
	onFetch func(dir string) error
	// branches, when set, is what the repository's branches reach: the
	// commits of each, newest first. Without it every commit is the tip of
	// the ref it is deployed from.
	branches map[string][]string
	// historyOf is the ref whose history git fetched last, per directory.
	historyOf map[string]string
	// psOutput, when set, prints `docker compose ps` (the app's containers).
	psOutput func() string
}

const defaultRepoServices = `{
	"web": {"build": {"context": "."}, "ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}]},
	"db": {"image": "postgres:17", "ports": [{"target": 5432, "published": "5432", "protocol": "tcp"}]}
}`

func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	box, err := secrets.NewBox([]byte(strings.Repeat("k", secrets.KeySize)))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:            t,
		store:        st,
		health:       &fakeHealth{},
		prober:       &fakeProber{percent: 40},
		router:       &fakeRouter{},
		box:          box,
		clock:        clock.NewFake(time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)),
		dir:          t.TempDir(),
		repoServices: defaultRepoServices,
	}
	h.runner = &runner.Fake{Handler: h.handle}
	opts := Options{
		Store:   st,
		Runner:  h.runner,
		Clock:   h.clock,
		Box:     box,
		System:  h.prober,
		Health:  h.health,
		Router:  h.router,
		AppsDir: filepath.Join(h.dir, "apps"),
	}
	if mutate != nil {
		mutate(&opts)
	}
	h.engine = New(opts)
	return h
}

// handle simulates git and docker compose.
func (h *harness) handle(ctx context.Context, cmd runner.Cmd) (string, error) {
	switch cmd.Name {
	case "git":
		switch cmd.Args[0] {
		case "ls-remote":
			// The check that the ref reaches the commit (ARCHITECTURE 7).
			name := cmd.Args[3]
			h.mu.Lock()
			branches := h.branches
			h.mu.Unlock()
			if branches == nil {
				return h.deployingSHA(cmd.Dir) + "\t" + name + "\n", nil
			}
			if commits := branches[strings.TrimPrefix(name, "refs/heads/")]; len(commits) > 0 {
				return commits[0] + "\t" + name + "\n", nil
			}
			return "", nil
		case "rev-list":
			h.mu.Lock()
			commits := h.branches[h.historyOf[cmd.Dir]]
			h.mu.Unlock()
			return strings.Join(commits, "\n") + "\n", nil
		case "fetch":
			sha := cmd.Args[len(cmd.Args)-1]
			if slices.Contains(cmd.Args, "--filter=tree:0") {
				// The history of a ref, not a checkout.
				h.mu.Lock()
				defer h.mu.Unlock()
				if h.historyOf == nil {
					h.historyOf = map[string]string{}
				}
				h.historyOf[cmd.Dir] = strings.TrimPrefix(sha, "refs/heads/")
				return "", nil
			}
			if err := os.WriteFile(filepath.Join(cmd.Dir, "compose.yaml"), []byte("# "+sha+"\n"), 0o644); err != nil {
				return "", err
			}
			h.mu.Lock()
			onFetch := h.onFetch
			h.mu.Unlock()
			if onFetch != nil {
				if err := onFetch(cmd.Dir); err != nil {
					return "", err
				}
			}
			return "", os.WriteFile(filepath.Join(cmd.Dir, ".sha"), []byte(sha), 0o644)
		case "log":
			sha, err := os.ReadFile(filepath.Join(cmd.Dir, ".sha"))
			if err != nil {
				return "", err
			}
			return string(sha) + "\nCommit " + string(sha[:4]) + "\n", nil
		}
		return "", nil
	case "docker":
		sub, merged := composeSubcommand(cmd.Args)
		h.mu.Lock()
		hook, services, buildOutput, servicesFor, psOutput := h.onCompose, h.repoServices, h.buildOutput, h.servicesFor, h.psOutput
		h.mu.Unlock()
		if servicesFor != nil && sub == "config" {
			services = servicesFor(cmd)
		}
		if hook != nil {
			if err := hook(ctx, sub, cmd); err != nil {
				return "", err
			}
		}
		switch sub {
		case "config":
			if merged {
				// Simulate what the override does to published ports.
				override, err := os.ReadFile(filepath.Join(cmd.Dir, compose.OverrideFileName))
				if err != nil {
					return "", fmt.Errorf("override not written before the merged config: %w", err)
				}
				services = strings.ReplaceAll(services,
					`"ports": [{"target": 5432, "published": "5432", "protocol": "tcp"}]`, `"ports": null`)
				if m := loopbackPort.FindStringSubmatch(string(override)); m != nil {
					services = strings.ReplaceAll(services,
						`"ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}]`,
						`"ports": [{"host_ip": "127.0.0.1", "target": 3000, "published": "`+m[1]+`", "protocol": "tcp"}]`)
				}
			}
			return `{"name": "koroku-blog", "services": ` + services + `}`, nil
		case "ps":
			if psOutput != nil {
				return psOutput(), nil
			}
		case "build":
			return buildOutput, nil
		}
		return "", nil
	}
	return "", fmt.Errorf("unexpected command %s", cmd.Name)
}

// deployingSHA is the commit of the deployment whose release directory is
// dir.
func (h *harness) deployingSHA(dir string) string {
	id, err := strconv.ParseInt(filepath.Base(dir), 10, 64)
	if err != nil {
		return ""
	}
	d, err := h.store.GetDeployment(context.Background(), id)
	if err != nil {
		return ""
	}
	return d.CommitSHA
}

// loopbackPort finds the host port the override publishes the web service on.
var loopbackPort = regexp.MustCompile(`"127\.0\.0\.1:(\d+):3000"`)

// composeSubcommand extracts the subcommand of a `docker compose` call and
// whether the override file is part of it.
func composeSubcommand(args []string) (sub string, merged bool) {
	if len(args) == 0 || args[0] != "compose" {
		return strings.Join(args, " "), false
	}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "-p", "--env-file", "--profile", "--project-directory":
			i++
		case "-f":
			i++
			if strings.HasSuffix(args[i], compose.OverrideFileName) {
				merged = true
			}
		default:
			return args[i], merged
		}
	}
	return "", merged
}

func (h *harness) createApp(slug string) store.App {
	h.t.Helper()
	app, err := h.store.CreateApp(context.Background(), store.App{
		Slug:            slug,
		RepoFullName:    "octo/" + slug,
		DefaultBranch:   "main",
		WebService:      "web",
		WebPort:         3000,
		HealthcheckPath: "/up",
	}, 20000, 20999, h.clock.Now())
	if err != nil {
		h.t.Fatalf("CreateApp: %v", err)
	}
	return app
}

func (h *harness) setEnv(app store.App, vars map[string]string) {
	h.t.Helper()
	var rows []store.EnvVar
	for k, v := range vars {
		sealed, err := h.box.Seal([]byte(v), secrets.EnvVarContext(app.ID, k))
		if err != nil {
			h.t.Fatal(err)
		}
		rows = append(rows, store.EnvVar{Key: k, ValueEncrypted: sealed})
	}
	if err := h.store.ReplaceEnvVars(context.Background(), app.ID, rows); err != nil {
		h.t.Fatalf("ReplaceEnvVars: %v", err)
	}
}

// start runs the worker in the background until the test ends.
func (h *harness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.engine.Run(ctx)
	}()
	h.t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (h *harness) enqueue(slug, sha string) store.Deployment {
	h.t.Helper()
	app, err := h.store.GetApp(context.Background(), slug)
	if err != nil {
		h.t.Fatalf("GetApp: %v", err)
	}
	d, err := h.engine.Enqueue(context.Background(), app, "main", sha, "")
	if err != nil {
		h.t.Fatalf("Enqueue: %v", err)
	}
	return d
}

func (h *harness) wait(id int64) store.Deployment {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		d, err := h.store.GetDeployment(context.Background(), id)
		if err != nil {
			h.t.Fatalf("GetDeployment: %v", err)
		}
		if store.IsTerminal(d.Status) {
			// The worker still cleans up after recording the result.
			h.waitIdle()
			return d
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("deployment %d did not finish", id)
	return store.Deployment{}
}

func (h *harness) waitIdle() {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.engine.mu.Lock()
		busy := len(h.engine.cancels)
		h.engine.mu.Unlock()
		if busy == 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// deploy enqueues a commit and waits for the result.
func (h *harness) deploy(slug, sha string) store.Deployment {
	h.t.Helper()
	return h.wait(h.enqueue(slug, sha).ID)
}

func (h *harness) log(slug string, id int64) string {
	h.t.Helper()
	data, err := os.ReadFile(h.engine.LogPath(slug, id))
	if err != nil {
		h.t.Fatalf("read deploy log: %v", err)
	}
	return string(data)
}

// psGraces is the listing of the app's containers at the start step, which
// reads their stop grace periods for the deploy log (ARCHITECTURE 6).
const psGraces = "ps --all --no-trunc --quiet --filter label=com.docker.compose.project=koroku-blog"

// dockerCalls returns the compose subcommands seen so far, in order.
func (h *harness) dockerCalls() []string {
	var subs []string
	for _, c := range h.runner.Calls() {
		if c.Name != "docker" {
			continue
		}
		sub, _ := composeSubcommand(c.Args)
		subs = append(subs, sub)
	}
	return subs
}

func (h *harness) lastCall(sub string) runner.Cmd {
	h.t.Helper()
	calls := h.runner.Calls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].Name != "docker" {
			continue
		}
		if got, _ := composeSubcommand(calls[i].Args); got == sub {
			return calls[i]
		}
	}
	h.t.Fatalf("no %q call recorded", sub)
	return runner.Cmd{}
}

func TestDeploySucceeds(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.PruneImages = true })
	app := h.createApp("blog")
	h.setEnv(app, map[string]string{"DATABASE_URL": "postgres://app:s3cr3t-password@db/app", "RAILS_ENV": "production"})
	h.buildOutput = "Step 1/3: ARG DATABASE_URL=postgres://app:s3cr3t-password@db/app\nbuilt\n"
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded || d.Error != "" {
		t.Fatalf("deployment = %+v\n%s", d, h.log("blog", d.ID))
	}
	if d.CommitMessage != "Commit aaaa" || d.StartedAt == nil || d.FinishedAt == nil {
		t.Errorf("deployment metadata = %+v", d)
	}

	// The steps ran in the documented order.
	want := []string{"config", "config", "config", "build", "pull", psGraces, "up", "image prune -f"}
	if got := h.dockerCalls(); !slices.Equal(got, want) {
		t.Errorf("docker calls = %v, want %v", got, want)
	}
	release := h.engine.ReleaseDir("blog", d.ID)
	up := h.lastCall("up")
	wantUp := []string{"compose", "-p", "koroku-blog",
		"-f", filepath.Join(release, "compose.yaml"),
		"-f", filepath.Join(release, compose.OverrideFileName),
		"--env-file", h.engine.EnvFile("blog"), "--env-file", h.engine.ReleaseEnvFile("blog"),
		"up", "-d", "--pull", "missing", "--remove-orphans", "--wait", "--wait-timeout", "300"}
	if !slices.Equal(up.Args, wantUp) {
		t.Errorf("up args = %v\nwant      %v", up.Args, wantUp)
	}
	if pull := h.lastCall("pull"); !slices.Contains(pull.Args, "--ignore-buildable") {
		t.Errorf("pull args = %v", pull.Args)
	}

	// The env file is private and holds the decrypted values.
	envPath := h.engine.EnvFile("blog")
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("env file mode = %04o, want 0600", info.Mode().Perm())
	}
	env, _ := os.ReadFile(envPath)
	if string(env) != "DATABASE_URL=postgres://app:s3cr3t-password@db/app\nRAILS_ENV=production\n" {
		t.Errorf("env file = %q", env)
	}

	// The override publishes only the web service, on loopback.
	override, err := os.ReadFile(filepath.Join(release, compose.OverrideFileName))
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	for _, want := range []string{`- "127.0.0.1:20000:3000"`, "ports: !reset []", envPath, `mem_limit: "512m"`,
		fmt.Sprintf(`image: "koroku-blog-web:r%d"`, d.ID)} {
		if !strings.Contains(string(override), want) {
			t.Errorf("override lacks %q:\n%s", want, override)
		}
	}

	// Health check, routing and bookkeeping.
	if len(h.health.urls) != 1 || h.health.urls[0] != "http://127.0.0.1:20000/up" {
		t.Errorf("health URLs = %v", h.health.urls)
	}
	// Once before the health check, once after the deployment became current.
	if h.router.calls != 2 {
		t.Errorf("router calls = %d", h.router.calls)
	}
	got, _ := h.store.GetApp(context.Background(), "blog")
	if got.CurrentDeploymentID == nil || *got.CurrentDeploymentID != d.ID {
		t.Errorf("current deployment = %v", got.CurrentDeploymentID)
	}

	// The release records what it runs with, as fingerprints only.
	fingerprints, ok, err := h.store.DeploymentEnvFingerprints(context.Background(), d.ID)
	if err != nil || !ok || len(fingerprints) != 2 ||
		fingerprints["RAILS_ENV"] != h.box.Fingerprint(secrets.EnvVarContext(app.ID, "RAILS_ENV"), []byte("production")) {
		t.Errorf("env fingerprints = %v, %v, %v", fingerprints, ok, err)
	}

	// Secrets never reach the log.
	log := h.log("blog", d.ID)
	if strings.Contains(log, "s3cr3t-password") {
		t.Errorf("the deploy log leaks a secret:\n%s", log)
	}
	for _, want := range []string{"==> 2026-11-18T03:00:00Z fetch Fetching " + shaA, "ARG DATABASE_URL=[MASKED]", "==> 2026-11-18T03:00:00Z build Building images", "==> 2026-11-18T03:00:00Z done Deployment succeeded"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

// TestDeployProtectsProductionMemory: the environment decides the OOM
// order and the reservation, from the next deploy on (ARCHITECTURE 6.2),
// and the release records what it reserves.
func TestDeployProtectsProductionMemory(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	app.Environment = store.EnvironmentProduction
	app.ServiceMemoryReservations = map[string]string{"db": "384m"}
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.start()

	read := func(d store.Deployment) (string, map[string]int64) {
		t.Helper()
		override, err := os.ReadFile(filepath.Join(h.engine.ReleaseDir("blog", d.ID), compose.OverrideFileName))
		if err != nil {
			t.Fatal(err)
		}
		recorded, err := h.store.DeploymentMemoryReservations(context.Background(), d.ID)
		if err != nil {
			t.Fatal(err)
		}
		return string(override), recorded[d.ID]
	}

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, h.log("blog", d.ID))
	}
	override, recorded := read(d)
	for _, want := range []string{"oom_score_adj: -400", `mem_reservation: "256m"`, `mem_reservation: "384m"`} {
		if !strings.Contains(override, want) {
			t.Errorf("production override lacks %q:\n%s", want, override)
		}
	}
	if want := map[string]int64{"web": 256 << 20, "db": 384 << 20}; !maps.Equal(recorded, want) {
		t.Errorf("recorded reservations = %v, want %v", recorded, want)
	}
	if log := h.log("blog", d.ID); !strings.Contains(log, "reserving db 384 MiB, web 256 MiB") {
		t.Errorf("log does not say what is reserved:\n%s", log)
	}
	// The shapes let the next reservations be told from the settings.
	shapes, err := h.store.DeploymentMemoryShapes(context.Background(), d.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := shapes[d.ID]["web"]; !ok || len(shapes[d.ID]) != 2 {
		t.Errorf("recorded shapes = %v, want web and db", shapes[d.ID])
	}

	app = h.reload("blog")
	app.Environment = store.EnvironmentStaging
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	d = h.deploy("blog", shaB)
	override, recorded = read(d)
	if !strings.Contains(override, "oom_score_adj: 500") || strings.Contains(override, "mem_reservation") {
		t.Errorf("staging override:\n%s", override)
	}
	if recorded == nil || len(recorded) != 0 {
		t.Errorf("a staging release records an empty map, got %v", recorded)
	}
}

func TestDeployImageOnlySkipsBuild(t *testing.T) {
	h := newHarness(t, nil)
	h.repoServices = `{"web": {"image": "nginx:1.29", "ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}]}}`
	h.createApp("blog")
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, h.log("blog", d.ID))
	}
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config", "config", "config", "pull", psGraces, "up"}) {
		t.Errorf("docker calls = %v", got)
	}
}

func TestPolicyViolationFailsBeforeTouchingContainers(t *testing.T) {
	h := newHarness(t, nil)
	h.repoServices = `{"web": {"image": "example/app", "privileged": true,
		"ports": [{"target": 3000, "published": "3000", "protocol": "tcp"}]}}`
	app := h.createApp("blog")
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "privileged") {
		t.Fatalf("deployment = %+v", d)
	}
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config", "config", "config"}) {
		t.Errorf("no build, up or rollback may run: %v", got)
	}
	if !strings.Contains(h.log("blog", d.ID), "Policy violation: [privileged]") {
		t.Errorf("log:\n%s", h.log("blog", d.ID))
	}
	if _, err := os.Stat(h.engine.ReleaseDir("blog", d.ID)); !os.IsNotExist(err) {
		t.Error("the failed release directory must be removed")
	}

	// With the app's relax flag the same commit deploys, and the waiver is logged.
	app.RelaxPolicy = true
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	relaxed := h.deploy("blog", shaA)
	if relaxed.Status != store.StatusSucceeded {
		t.Fatalf("relaxed deployment = %+v\n%s", relaxed, h.log("blog", relaxed.ID))
	}
	if !strings.Contains(h.log("blog", relaxed.ID), "Policy waived for this app: [privileged]") {
		t.Errorf("log:\n%s", h.log("blog", relaxed.ID))
	}
}

func TestMissingWebServiceIsAPolicyError(t *testing.T) {
	h := newHarness(t, nil)
	h.repoServices = `{"app": {"image": "example/app"}}`
	h.createApp("blog")
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, compose.RuleWebServiceMissing) {
		t.Fatalf("deployment = %+v", d)
	}
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config"}) {
		t.Errorf("docker calls = %v", got)
	}
}

func TestFailedStartRollsBackToPreviousRelease(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	h.setEnv(app, map[string]string{"GREETING": "version-one"})
	h.start()

	first := h.deploy("blog", shaA)
	if first.Status != store.StatusSucceeded {
		t.Fatalf("first deployment = %+v", first)
	}

	// The second commit also changes the environment and then fails to start.
	h.setEnv(app, map[string]string{"GREETING": "version-two"})
	h.runner.Reset()
	var upCalls int
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub != "up" {
			return nil
		}
		upCalls++
		if upCalls == 1 {
			_, _ = io.WriteString(cmd.Stderr, "container blog-web-1 exited (1)\n")
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.mu.Unlock()

	second := h.deploy("blog", shaB)
	if second.Status != store.StatusFailed || !strings.Contains(second.Error, "start containers") {
		t.Fatalf("second deployment = %+v", second)
	}

	// The previous release was brought back with its own files and environment.
	// The output of the failed containers is read, then the previous
	// release is validated again and started, and the failed release's
	// image is removed.
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config", "config", "config", "build", "pull", psGraces, "up", "logs", "config", "up", "image rm koroku-blog-web:r2", "ps --all --no-trunc --format {{.Image}}"}) {
		t.Errorf("docker calls = %v", got)
	}
	rollback := h.lastCall("up")
	prevRelease := h.engine.ReleaseDir("blog", first.ID)
	if !slices.Contains(rollback.Args, filepath.Join(prevRelease, "compose.yaml")) ||
		!slices.Contains(rollback.Args, filepath.Join(prevRelease, compose.OverrideFileName)) {
		t.Errorf("rollback args = %v", rollback.Args)
	}
	env, _ := os.ReadFile(h.engine.EnvFile("blog"))
	if string(env) != "GREETING=version-one\n" {
		t.Errorf("env file after rollback = %q, want the previous release's environment", env)
	}
	got, _ := h.store.GetApp(context.Background(), "blog")
	if got.CurrentDeploymentID == nil || *got.CurrentDeploymentID != first.ID {
		t.Errorf("current deployment must stay #%d, got %v", first.ID, got.CurrentDeploymentID)
	}
	log := h.log("blog", second.ID)
	for _, want := range []string{"container blog-web-1 exited (1)", fmt.Sprintf(" rollback Rolling back to deployment #%d", first.ID), "is running again"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if _, err := os.Stat(prevRelease); err != nil {
		t.Errorf("the previous release directory must be kept: %v", err)
	}
}

func TestFailedHealthCheckRollsBack(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	first := h.deploy("blog", shaA)

	h.runner.Reset()
	h.health.mu.Lock()
	h.health.err = errors.New("health check did not pass within 1m0s (last result: HTTP 502)")
	h.health.mu.Unlock()

	second := h.deploy("blog", shaB)
	if second.Status != store.StatusFailed || !strings.Contains(second.Error, "HTTP 502") {
		t.Fatalf("second deployment = %+v", second)
	}
	// The output of the failed containers is read, then the previous
	// release is validated again and started, and the failed release's
	// image is removed.
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config", "config", "config", "build", "pull", psGraces, "up", "logs", "config", "up", "image rm koroku-blog-web:r2", "ps --all --no-trunc --format {{.Image}}"}) {
		t.Errorf("docker calls = %v", got)
	}
	if !strings.Contains(h.log("blog", second.ID), fmt.Sprintf("Rolling back to deployment #%d", first.ID)) {
		t.Errorf("log:\n%s", h.log("blog", second.ID))
	}
	// Both replaced the running containers, so the memory history marks
	// both (ARCHITECTURE 9.5).
	for _, d := range []store.Deployment{first, second} {
		if d.ContainersReplaced == nil || !*d.ContainersReplaced {
			t.Errorf("deployment #%d containers_replaced = %v, want true", d.ID, d.ContainersReplaced)
		}
	}
}

func TestFailedFirstDeployRemovesContainers(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "up" {
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", d)
	}
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config", "config", "config", "build", "pull", psGraces, "up", "logs", "down", "image rm koroku-blog-web:r1", "ps --all --no-trunc --format {{.Image}}", "image rm postgres:17"}) {
		t.Errorf("docker calls = %v", got)
	}
	down := h.lastCall("down")
	if !slices.Equal(down.Args, []string{"compose", "-p", "koroku-blog", "down", "--remove-orphans"}) {
		t.Errorf("down args = %v (volumes must be kept)", down.Args)
	}
}

func TestBuildFailureDoesNotRollBack(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	h.setEnv(app, map[string]string{"GREETING": "version-one"})
	h.start()
	first := h.deploy("blog", shaA)

	h.setEnv(app, map[string]string{"GREETING": "version-two"})
	h.runner.Reset()
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "build" {
			return &runner.ExitError{Cmd: "docker", Code: 17}
		}
		return nil
	}
	h.mu.Unlock()

	second := h.deploy("blog", shaB)
	if second.Status != store.StatusFailed || !strings.Contains(second.Error, "build") {
		t.Fatalf("deployment = %+v", second)
	}
	if got := h.dockerCalls(); !slices.Equal(got, []string{"config", "config", "config", "build", "image rm koroku-blog-web:r2", "ps --all --no-trunc --format {{.Image}}"}) {
		t.Errorf("a build failure must not touch running containers: %v", got)
	}
	if second.ContainersReplaced == nil || *second.ContainersReplaced {
		t.Errorf("containers_replaced = %v, want false", second.ContainersReplaced)
	}
	// The running release keeps its environment until a start is attempted.
	env, _ := os.ReadFile(h.engine.EnvFile("blog"))
	if string(env) != "GREETING=version-one\n" {
		t.Errorf("env file = %q", env)
	}
	got, _ := h.store.GetApp(context.Background(), "blog")
	if *got.CurrentDeploymentID != first.ID {
		t.Errorf("current deployment = %d", *got.CurrentDeploymentID)
	}
}

func TestDiskFullRejectsDeploy(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	h.prober.percent = 91

	_, err := h.engine.Enqueue(context.Background(), app, "main", shaA, "")
	var full *DiskFullError
	if !errors.As(err, &full) || full.LimitPercent != 85 {
		t.Fatalf("err = %v", err)
	}
	list, _ := h.store.ListDeployments(context.Background(), app.ID, 10)
	if len(list) != 0 {
		t.Errorf("a rejected deployment must not be recorded: %+v", list)
	}

	// Exactly at the limit is still accepted; an unreadable disk does not block.
	h.prober.percent = 85
	if _, err := h.engine.Enqueue(context.Background(), app, "main", shaA, ""); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	h.prober.percent, h.prober.err = 99, errors.New("statfs failed")
	if _, err := h.engine.Enqueue(context.Background(), app, "main", shaB, ""); err != nil {
		t.Errorf("unreadable disk: %v", err)
	}
}

func TestDiskFillingUpWhileQueuedFailsTheJob(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	d := h.enqueue("blog", shaA)
	h.prober.mu.Lock()
	h.prober.percent = 95
	h.prober.mu.Unlock()
	h.start()

	got := h.wait(d.ID)
	if got.Status != store.StatusFailed || !strings.Contains(got.Error, "disk usage is 95%") {
		t.Fatalf("deployment = %+v", got)
	}
	if calls := h.runner.Calls(); len(calls) != 0 {
		t.Errorf("nothing may run when the disk is full: %v", calls)
	}
}

func TestEnqueueRejectsPartialSHA(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	if _, err := h.engine.Enqueue(context.Background(), app, "main", "abc123", ""); err == nil {
		t.Error("a short SHA must be rejected")
	}
}

func TestNewerDeploymentSupersedesQueued(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")

	first := h.enqueue("blog", shaA)
	second := h.enqueue("blog", shaB)
	h.start()

	if got := h.wait(second.ID); got.Status != store.StatusSucceeded {
		t.Fatalf("second = %+v", got)
	}
	got, _ := h.store.GetDeployment(context.Background(), first.ID)
	if got.Status != store.StatusCanceled || got.Error != store.SupersededError {
		t.Errorf("first = %+v", got)
	}
	if !strings.Contains(h.log("blog", first.ID), "superseded") {
		t.Errorf("log:\n%s", h.log("blog", first.ID))
	}
}

// TestPushDeploymentSupersedesQueued: a deployment started by a push
// (ARCHITECTURE 5.2) is recorded as such and replaces an older queued one.
func TestPushDeploymentSupersedesQueued(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")

	first := h.enqueue("blog", shaA)
	pushed, err := h.engine.EnqueuePush(context.Background(), app, "main", shaB, "Fix", "octo")
	if err != nil {
		t.Fatal(err)
	}
	h.start()
	got := h.wait(pushed.ID)
	if got.Status != store.StatusSucceeded || got.Trigger != store.TriggerPush || got.Pusher != "octo" || got.Ref != "main" {
		t.Fatalf("pushed = %+v", got)
	}
	if old, _ := h.store.GetDeployment(context.Background(), first.ID); old.Status != store.StatusCanceled {
		t.Errorf("first = %+v", old)
	}
	if !strings.Contains(h.log("blog", pushed.ID), "pushed to main by octo") {
		t.Errorf("log:\n%s", h.log("blog", pushed.ID))
	}
}

// TestWorkflowRunDeploymentIsAPush: a push released by a workflow run
// (ARCHITECTURE 5.2) is a push deployment that names the workflow in its
// log.
func TestWorkflowRunDeploymentIsAPush(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	d, err := h.engine.EnqueueWorkflowRun(context.Background(), app, "main", shaB, "Fix", "octo", ".github/workflows/image.yml")
	if err != nil {
		t.Fatal(err)
	}
	if d.Trigger != store.TriggerPush || d.Pusher != "octo" {
		t.Errorf("deployment = %+v", d)
	}
	if !strings.Contains(h.log("blog", d.ID), "pushed to main by octo, after workflow .github/workflows/image.yml succeeded") {
		t.Errorf("log:\n%s", h.log("blog", d.ID))
	}
}

func TestCancelQueuedDeployment(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	d := h.enqueue("blog", shaA)

	if err := h.engine.Cancel(context.Background(), d.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got, _ := h.store.GetDeployment(context.Background(), d.ID)
	if got.Status != store.StatusCanceled {
		t.Errorf("status = %q", got.Status)
	}
	if err := h.engine.Cancel(context.Background(), d.ID); !errors.Is(err, ErrNotCancelable) {
		t.Errorf("second cancel err = %v", err)
	}
	if err := h.engine.Cancel(context.Background(), 999); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown deployment err = %v", err)
	}
	h.start()
	time.Sleep(20 * time.Millisecond)
	if calls := h.runner.Calls(); len(calls) != 0 {
		t.Errorf("a canceled deployment must not run: %v", calls)
	}
}

func TestCancelRunningDeploymentRollsBack(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	first := h.deploy("blog", shaA)

	h.runner.Reset()
	started := make(chan struct{})
	var once sync.Once
	var upCalls int
	h.mu.Lock()
	h.onCompose = func(ctx context.Context, sub string, _ runner.Cmd) error {
		if sub != "up" {
			return nil
		}
		upCalls++
		if upCalls > 1 {
			return nil // the rollback
		}
		once.Do(func() { close(started) })
		<-ctx.Done() // like a real command killed by cancellation
		return ctx.Err()
	}
	h.mu.Unlock()

	second := h.enqueue("blog", shaB)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the deployment did not reach the start step")
	}
	if err := h.engine.Cancel(context.Background(), second.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	got := h.wait(second.ID)
	if got.Status != store.StatusCanceled || got.Error != "canceled by user" {
		t.Fatalf("deployment = %+v", got)
	}
	if !strings.Contains(h.log("blog", second.ID), fmt.Sprintf("Rolling back to deployment #%d", first.ID)) {
		t.Errorf("log:\n%s", h.log("blog", second.ID))
	}
	app, _ := h.store.GetApp(context.Background(), "blog")
	if *app.CurrentDeploymentID != first.ID {
		t.Errorf("current deployment = %d", *app.CurrentDeploymentID)
	}
}

func TestJobTimeout(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.JobTimeout = 50 * time.Millisecond })
	h.createApp("blog")
	h.onCompose = func(ctx context.Context, sub string, _ runner.Cmd) error {
		if sub == "build" {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "timed out after 50ms") {
		t.Fatalf("deployment = %+v", d)
	}
}

func TestInterruptedDeploymentsFailAtStartup(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	ctx := context.Background()

	// Simulate a previous koroku process that died mid-deploy.
	stale, _, err := h.store.EnqueueDeployment(ctx, app.ID, "main", shaA, "", h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.StartDeployment(ctx, stale.ID, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	queued := h.enqueue("blog", shaB)
	h.start()

	if got := h.wait(queued.ID); got.Status != store.StatusSucceeded {
		t.Fatalf("the queued deployment must still run: %+v", got)
	}
	got, _ := h.store.GetDeployment(ctx, stale.ID)
	if got.Status != store.StatusFailed || got.Error != store.InterruptedError {
		t.Errorf("stale deployment = %+v", got)
	}
	if !strings.Contains(h.log("blog", stale.ID), "interrupted") {
		t.Errorf("log:\n%s", h.log("blog", stale.ID))
	}
}

func TestShutdownMarksRunningDeploymentInterrupted(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	started := make(chan struct{})
	var once sync.Once
	h.onCompose = func(ctx context.Context, sub string, _ runner.Cmd) error {
		if sub == "up" {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.engine.Run(ctx)
	}()
	d := h.enqueue("blog", shaA)
	<-started
	cancel()
	<-done

	got, _ := h.store.GetDeployment(context.Background(), d.ID)
	if got.Status != store.StatusFailed || got.Error != store.InterruptedError {
		t.Fatalf("deployment = %+v", got)
	}
	// No rollback or cleanup command may be started during shutdown.
	if subs := h.dockerCalls(); subs[len(subs)-1] != "up" {
		t.Errorf("docker calls = %v", subs)
	}
}

func TestOnlyOneDeploymentRunsAtATime(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("alpha")
	h.createApp("beta")

	var mu sync.Mutex
	active, maxActive := 0, 0
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub != "build" {
			return nil
		}
		mu.Lock()
		active++
		maxActive = max(maxActive, active)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}
	a := h.enqueue("alpha", shaA)
	b := h.enqueue("beta", shaB)
	h.start()
	h.wait(a.ID)
	h.wait(b.ID)
	if maxActive != 1 {
		t.Errorf("max concurrent builds = %d, want 1", maxActive)
	}
}

func TestOldReleasesArePruned(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.KeepReleases = 2 })
	h.createApp("blog")
	h.start()

	var ids []int64
	for _, sha := range []string{shaA, shaB, shaC} {
		d := h.deploy("blog", sha)
		if d.Status != store.StatusSucceeded {
			t.Fatalf("deployment = %+v", d)
		}
		ids = append(ids, d.ID)
	}
	if _, err := os.Stat(h.engine.ReleaseDir("blog", ids[0])); !os.IsNotExist(err) {
		t.Errorf("the oldest release must be removed, err = %v", err)
	}
	if _, err := os.Stat(h.engine.stateDir("blog", ids[0])); !os.IsNotExist(err) {
		t.Errorf("the oldest release state must be removed, err = %v", err)
	}
	for _, id := range ids[1:] {
		if _, err := os.Stat(h.engine.ReleaseDir("blog", id)); err != nil {
			t.Errorf("release %d must be kept: %v", id, err)
		}
	}
	// The pruned release's image goes with it; kept releases keep theirs.
	calls := h.dockerCalls()
	if !slices.Contains(calls, fmt.Sprintf("image rm koroku-blog-web:r%d", ids[0])) {
		t.Errorf("the image of the pruned release was not removed: %v", calls)
	}
	for _, id := range ids[1:] {
		if slices.Contains(calls, fmt.Sprintf("image rm koroku-blog-web:r%d", id)) {
			t.Errorf("the image of kept release %d was removed", id)
		}
	}
	// Deploy logs are kept for every deployment.
	if _, err := os.Stat(h.engine.LogPath("blog", ids[0])); err != nil {
		t.Errorf("the deploy log must be kept: %v", err)
	}
}

func TestRepositoryTokenIsMasked(t *testing.T) {
	const token = "ghs_exampleInstallationToken123"
	h := newHarness(t, func(o *Options) { o.Tokens = fakeTokens{token: token} })
	h.createApp("blog")
	h.onCompose = func(_ context.Context, sub string, cmd runner.Cmd) error {
		if sub == "build" {
			_, _ = io.WriteString(cmd.Stdout, "cloning with "+token+"\n")
			return errors.New("fatal: could not read from https://x-access-token:" + token + "@github.com/octo/blog")
		}
		return nil
	}
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", d)
	}
	if strings.Contains(d.Error, token) {
		t.Errorf("the stored error leaks the token: %q", d.Error)
	}
	if log := h.log("blog", d.ID); strings.Contains(log, token) {
		t.Errorf("the deploy log leaks the token:\n%s", log)
	}
	// git received the token through its environment.
	var sawAuth bool
	for _, c := range h.runner.Calls() {
		if c.Name == "git" && strings.Contains(strings.Join(c.Env, "\n"), "http.extraheader") {
			sawAuth = true
		}
	}
	if !sawAuth {
		t.Error("git was not given the token")
	}
	_ = gitrepo.Auth{}
}

func TestRouterFailureRollsBack(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.router.err = errors.New("caddy admin API unreachable")
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "update routes") {
		t.Fatalf("deployment = %+v", d)
	}
	if subs := h.dockerCalls(); !slices.Contains(subs, "down") {
		t.Errorf("docker calls = %v", subs)
	}
}

// TestDeployLogStartsEmpty: when the database is recreated but the data
// directory is kept, deployment IDs start over and a log file of an earlier
// deployment with the same ID may still exist. Its lines must not appear in
// the new deployment's log.
func TestDeployLogStartsEmpty(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()

	// The next deployment gets ID 1 in the fresh database.
	stale := h.engine.LogPath("blog", 1)
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("Policy violation: left over from an earlier database\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := h.deploy("blog", shaA)
	if d.ID != 1 || d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	got := h.log("blog", d.ID)
	if strings.Contains(got, "left over") {
		t.Errorf("the log contains a stale line:\n%s", got)
	}
	if !strings.HasPrefix(got, "Queued deployment of "+shaA) {
		t.Errorf("the log does not start with the queued line:\n%s", got)
	}
}

// TestWorkerWaitsForTheQueuedLine: the worker may pick a deployment up as
// soon as its row exists (it polls, and other deployments wake it), which is
// before Enqueue has removed a stale log file and written the queued line.
// It must wait for that; otherwise its first lines are deleted with the
// stale file or come before the queued line.
func TestWorkerWaitsForTheQueuedLine(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	// Wake the worker right after the insert and give it time to run.
	h.engine.afterInsert = func() {
		h.engine.notify()
		time.Sleep(100 * time.Millisecond)
	}
	stale := h.engine.LogPath("blog", 1)
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("left over from an earlier database\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	d := h.deploy("blog", shaA)
	got := h.log("blog", d.ID)
	if !strings.HasPrefix(got, "Queued deployment of "+shaA) || strings.Contains(got, "left over") {
		t.Errorf("log:\n%s", got)
	}
	if !strings.Contains(got, " fetch Fetching ") {
		t.Errorf("the worker's lines are missing:\n%s", got)
	}
}

// TestFirstDeployIsRouted: the router builds the proxy configuration from
// the database, and an app without a current deployment gets the 503 page.
// The last sync must therefore see the new deployment as current and the app
// as running; otherwise the first release stays unreachable until the
// periodic sync.
func TestFirstDeployIsRouted(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	if err := h.store.SetAppDesiredState(context.Background(), app.ID, store.DesiredStopped, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	var lastSeen store.App
	h.router.onSync = func() {
		got, err := h.store.GetApp(context.Background(), "blog")
		if err != nil {
			t.Errorf("GetApp: %v", err)
			return
		}
		lastSeen = got
	}
	h.start()
	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	h.router.mu.Lock()
	defer h.router.mu.Unlock()
	if lastSeen.CurrentDeploymentID == nil || *lastSeen.CurrentDeploymentID != d.ID {
		t.Errorf("last sync saw current deployment %v, want %d", lastSeen.CurrentDeploymentID, d.ID)
	}
	if lastSeen.DesiredState != store.DesiredRunning {
		t.Errorf("last sync saw desired state %q", lastSeen.DesiredState)
	}
}

func TestDeploySetsDesiredStateRunning(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	if err := h.store.SetAppDesiredState(context.Background(), app.ID, store.DesiredStopped, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.start()
	if d := h.deploy("blog", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	got, _ := h.store.GetApp(context.Background(), "blog")
	if got.DesiredState != store.DesiredRunning {
		t.Errorf("DesiredState = %q", got.DesiredState)
	}
}

type fakeBackup struct {
	mu    sync.Mutex
	apps  []string
	err   error
	calls int
	// hook, when set, runs as the backup (outside mu).
	hook func(ctx context.Context, app store.App) error
}

func (f *fakeBackup) BackupApp(ctx context.Context, app store.App) error {
	f.mu.Lock()
	f.calls++
	f.apps = append(f.apps, app.Slug)
	err, hook := f.err, f.hook
	f.mu.Unlock()
	if hook != nil {
		return hook(ctx, app)
	}
	return err
}

func TestBackupBeforeDeploy(t *testing.T) {
	backup := &fakeBackup{}
	h := newHarness(t, func(o *Options) { o.Backup = backup })
	app := h.createApp("blog")
	app.BackupBeforeDeploy = true
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.createApp("plain") // has not opted in
	h.start()

	// Nothing to back up before the first deploy.
	if d := h.deploy("blog", shaA); d.Status != store.StatusSucceeded {
		t.Fatalf("first deployment = %+v", d)
	}
	if backup.calls != 0 {
		t.Errorf("backup ran before the first deploy: %d", backup.calls)
	}

	// From the second deploy on the backup runs after build and before start.
	h.runner.Reset()
	var order []string
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		backup.mu.Lock()
		order = append(order, fmt.Sprintf("%s(backups=%d)", sub, backup.calls))
		backup.mu.Unlock()
		return nil
	}
	h.mu.Unlock()
	second := h.deploy("blog", shaB)
	if second.Status != store.StatusSucceeded {
		t.Fatalf("second deployment = %+v", second)
	}
	want := []string{"config(backups=0)", "config(backups=0)", "config(backups=0)", "build(backups=0)", "pull(backups=0)", psGraces + "(backups=1)", "up(backups=1)"}
	if !slices.Equal(order, want) {
		t.Errorf("order = %v", order)
	}
	if !strings.Contains(h.log("blog", second.ID), " backup Backing up before deploy") {
		t.Errorf("log:\n%s", h.log("blog", second.ID))
	}

	// Apps that did not opt in are not backed up.
	h.deploy("plain", shaA)
	h.deploy("plain", shaB)
	if backup.calls != 1 || backup.apps[0] != "blog" {
		t.Errorf("backup calls = %d (%v)", backup.calls, backup.apps)
	}

	// A failed backup stops the deploy before the running containers are touched.
	backup.mu.Lock()
	backup.err = errors.New("restic: repository unreachable")
	backup.mu.Unlock()
	h.runner.Reset()
	h.mu.Lock()
	h.onCompose = nil
	h.mu.Unlock()
	third := h.deploy("blog", shaC)
	if third.Status != store.StatusFailed || !strings.Contains(third.Error, "backup before deploy") {
		t.Fatalf("third deployment = %+v", third)
	}
	if slices.Contains(h.dockerCalls(), "up") {
		t.Errorf("containers were touched after a failed backup: %v", h.dockerCalls())
	}
	current, _ := h.store.GetApp(context.Background(), "blog")
	if *current.CurrentDeploymentID != second.ID {
		t.Errorf("current deployment = %d", *current.CurrentDeploymentID)
	}
}

func TestBackupBeforeDeployWithoutBackupConfigured(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	app.BackupBeforeDeploy = true
	if err := h.store.UpdateApp(context.Background(), app, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	h.start()
	h.deploy("blog", shaA)
	d := h.deploy("blog", shaB)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v", d)
	}
	if !strings.Contains(h.log("blog", d.ID), "backups are not configured; skipping") {
		t.Errorf("log:\n%s", h.log("blog", d.ID))
	}
}

// TestLongDeploymentIsStillRecorded guards against starting the "record the
// result" timeout when the job starts: a deployment that outlives that
// timeout must still be marked finished instead of staying "running".
func TestLongDeploymentIsStillRecorded(t *testing.T) {
	h := newHarness(t, nil)
	h.engine.finishTimeout = 40 * time.Millisecond
	h.createApp("blog")
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "build" {
			time.Sleep(150 * time.Millisecond) // several times the finish timeout
		}
		return nil
	}
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded || d.FinishedAt == nil {
		t.Fatalf("deployment = %+v", d)
	}
	app, _ := h.store.GetApp(context.Background(), "blog")
	if app.CurrentDeploymentID == nil || *app.CurrentDeploymentID != d.ID {
		t.Errorf("current deployment = %v", app.CurrentDeploymentID)
	}

	// The same holds for a failing long deployment.
	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "build" {
			time.Sleep(150 * time.Millisecond)
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.mu.Unlock()
	if failed := h.deploy("blog", shaB); failed.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", failed)
	}
}

// TestDeployLogStepHeadlines pins the headline format the console parses:
// "==> <RFC 3339 time> <key> <title>", one per step, in order.
func TestDeployLogStepHeadlines(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.PruneImages = true })
	h.createApp("blog")
	h.start()
	first := h.deploy("blog", shaA)

	stepKeys := func(log string) []string {
		var keys []string
		for _, line := range strings.Split(log, "\n") {
			if !strings.HasPrefix(line, "==> ") {
				continue
			}
			fields := strings.SplitN(line, " ", 4)
			if len(fields) < 4 {
				t.Errorf("malformed headline %q", line)
				continue
			}
			if _, err := time.Parse(time.RFC3339, fields[1]); err != nil {
				t.Errorf("headline %q: bad time: %v", line, err)
			}
			keys = append(keys, fields[2])
		}
		return keys
	}
	want := []string{StepFetch, StepPrepare, StepValidate, StepBuild, StepPull, StepStart, StepRoute, StepCheck, StepDone, StepCleanup}
	if got := stepKeys(h.log("blog", first.ID)); !slices.Equal(got, want) {
		t.Errorf("steps of a successful deployment = %v, want %v", got, want)
	}

	// A failed start: the failure and the rollback are steps of their own.
	h.health.mu.Lock()
	h.health.err = errors.New("health check did not pass")
	h.health.mu.Unlock()
	second := h.deploy("blog", shaB)
	want = []string{StepFetch, StepPrepare, StepValidate, StepBuild, StepPull, StepStart, StepRoute, StepCheck, StepFailed, StepRollback}
	if got := stepKeys(h.log("blog", second.ID)); !slices.Equal(got, want) {
		t.Errorf("steps of a failed deployment = %v, want %v", got, want)
	}
	if !strings.Contains(h.log("blog", second.ID), " failed Deployment failed: health check did not pass") {
		t.Errorf("log:\n%s", h.log("blog", second.ID))
	}
}

// TestRollbackImpossibleWhenPreviousReleaseIsGone: without the previous
// release's files there is nothing to bring back. The deployment still ends
// as failed, says so in its log, and does not run a second `up`.
func TestRollbackImpossibleWhenPreviousReleaseIsGone(t *testing.T) {
	cases := map[string]func(h *harness, prev int64){
		"release directory removed": func(h *harness, prev int64) {
			_ = os.RemoveAll(h.engine.ReleaseDir("blog", prev))
		},
		"release metadata removed": func(h *harness, prev int64) {
			_ = os.Remove(filepath.Join(h.engine.stateDir("blog", prev), "release.json"))
		},
		"environment snapshot removed": func(h *harness, prev int64) {
			_ = os.Remove(filepath.Join(h.engine.stateDir("blog", prev), "env"))
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.createApp("blog")
			h.start()
			first := h.deploy("blog", shaA)
			breakIt(h, first.ID)

			h.runner.Reset()
			h.health.mu.Lock()
			h.health.err = errors.New("health check did not pass")
			h.health.mu.Unlock()
			second := h.deploy("blog", shaB)
			if second.Status != store.StatusFailed {
				t.Fatalf("deployment = %+v", second)
			}
			log := h.log("blog", second.ID)
			if !strings.Contains(log, "Rollback is not possible") {
				t.Errorf("log:\n%s", log)
			}
			ups := 0
			for _, sub := range h.dockerCalls() {
				if sub == "up" {
					ups++
				}
			}
			if ups != 1 {
				t.Errorf("up ran %d times, want only the failed start", ups)
			}
			app, _ := h.store.GetApp(context.Background(), "blog")
			if *app.CurrentDeploymentID != first.ID {
				t.Errorf("current deployment = %d", *app.CurrentDeploymentID)
			}
		})
	}
}

func TestFailedRollbackIsReported(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	h.deploy("blog", shaA)

	h.mu.Lock()
	h.onCompose = func(_ context.Context, sub string, _ runner.Cmd) error {
		if sub == "up" {
			return &runner.ExitError{Cmd: "docker", Code: 1}
		}
		return nil
	}
	h.mu.Unlock()
	second := h.deploy("blog", shaB)
	if second.Status != store.StatusFailed {
		t.Fatalf("deployment = %+v", second)
	}
	if log := h.log("blog", second.ID); !strings.Contains(log, "Rollback failed") {
		t.Errorf("log:\n%s", log)
	}
}

// TestUndecryptableEnvFailsBeforeAnythingRuns: with the wrong master key the
// stored values cannot be read. The deployment must fail early instead of
// starting the app without its environment.
func TestUndecryptableEnvFailsBeforeAnythingRuns(t *testing.T) {
	h := newHarness(t, nil)
	app := h.createApp("blog")
	other, err := secrets.NewBox([]byte(strings.Repeat("x", secrets.KeySize)))
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := other.Seal([]byte("s3cr3t-password"), secrets.EnvVarContext(app.ID, "DATABASE_URL"))
	if err := h.store.ReplaceEnvVars(context.Background(), app.ID, []store.EnvVar{{Key: "DATABASE_URL", ValueEncrypted: sealed}}); err != nil {
		t.Fatal(err)
	}
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusFailed || !strings.Contains(d.Error, "DATABASE_URL") || !strings.Contains(d.Error, "master key") {
		t.Fatalf("deployment = %+v", d)
	}
	if calls := h.dockerCalls(); len(calls) != 0 {
		t.Errorf("docker ran although the environment is unreadable: %v", calls)
	}
	if _, err := os.Stat(h.engine.EnvFile("blog")); !os.IsNotExist(err) {
		t.Error("an env file was written")
	}
}

func TestDeployWithoutRepositoryFails(t *testing.T) {
	h := newHarness(t, nil)
	app, err := h.store.CreateApp(context.Background(), store.App{
		Slug: "norepo", DefaultBranch: "main", WebService: "web", WebPort: 3000, HealthcheckPath: "/",
	}, 20000, 20999, h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.start()
	d, err := h.engine.Enqueue(context.Background(), app, "main", shaA, "")
	if err != nil {
		t.Fatal(err)
	}
	got := h.wait(d.ID)
	if got.Status != store.StatusFailed || !strings.Contains(got.Error, "no repository configured") {
		t.Fatalf("deployment = %+v", got)
	}
	if calls := h.runner.Calls(); len(calls) != 0 {
		t.Errorf("nothing may run: %v", calls)
	}
}

// TestRedeployOfTheSameCommitGetsItsOwnRelease: rolling back means deploying
// an older commit again, so the same SHA must be deployable repeatedly.
func TestRedeployOfTheSameCommitGetsItsOwnRelease(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	first := h.deploy("blog", shaA)
	h.deploy("blog", shaB)
	again := h.deploy("blog", shaA)
	if again.Status != store.StatusSucceeded || again.ID == first.ID {
		t.Fatalf("redeploy = %+v", again)
	}
	if h.engine.ReleaseDir("blog", again.ID) == h.engine.ReleaseDir("blog", first.ID) {
		t.Error("the redeploy reused the old release directory")
	}
	override, err := os.ReadFile(filepath.Join(h.engine.ReleaseDir("blog", again.ID), compose.OverrideFileName))
	if err != nil || !strings.Contains(string(override), fmt.Sprintf(`image: "koroku-blog-web:r%d"`, again.ID)) {
		t.Errorf("override of the redeploy: %v\n%s", err, override)
	}
	app, _ := h.store.GetApp(context.Background(), "blog")
	if *app.CurrentDeploymentID != again.ID {
		t.Errorf("current deployment = %d", *app.CurrentDeploymentID)
	}
}

// TestOverrideIsNotWrittenThroughRepositorySymlink: the checkout is
// repository content. A repository that ships koroku.override.yaml as a
// symlink must not make koroku (root) overwrite the link's target.
func TestOverrideIsNotWrittenThroughRepositorySymlink(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	victim := filepath.Join(h.dir, "victim.conf")
	if err := os.WriteFile(victim, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.onFetch = func(dir string) error {
		return os.Symlink(victim, filepath.Join(dir, compose.OverrideFileName))
	}
	h.start()

	d := h.deploy("blog", shaA)
	if d.Status != store.StatusSucceeded {
		t.Fatalf("deployment = %+v\n%s", d, h.log("blog", d.ID))
	}
	if got, _ := os.ReadFile(victim); string(got) != "do not touch" {
		t.Errorf("the symlink target was overwritten: %q", got)
	}
	override := filepath.Join(h.engine.ReleaseDir("blog", d.ID), compose.OverrideFileName)
	info, err := os.Lstat(override)
	if err != nil || !info.Mode().IsRegular() {
		t.Errorf("the override must be a regular file in the release: %v %v", info, err)
	}
	content, _ := os.ReadFile(override)
	if !strings.Contains(string(content), "Generated by koroku") {
		t.Errorf("override content = %q", content)
	}
}

func TestComposeConfigDoesNotResolveEnvFiles(t *testing.T) {
	h := newHarness(t, nil)
	h.createApp("blog")
	h.start()
	h.deploy("blog", shaA)
	for _, c := range h.runner.Calls() {
		if sub, _ := composeSubcommand(c.Args); sub == "config" && !slices.Contains(c.Args, "--no-env-resolution") {
			t.Errorf("config must not fold env files into its output: %v", c.Args)
		}
	}
}

// TestRunStoppedWhileStartingIsNotAnError: koroku can be stopped right after
// it starts, while the worker is still recording what the previous process
// left behind. The canceled context must end Run quietly, not as a failure.
func TestRunStoppedWhileStartingIsNotAnError(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.engine.Run(ctx); err != nil {
		t.Fatalf("Run with a canceled context = %v, want nil", err)
	}
}
