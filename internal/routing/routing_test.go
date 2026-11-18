package routing

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/domains"
	"github.com/tricknotes/koroku/internal/store"
)

var now = time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)

type fakeLoader struct {
	mu      sync.Mutex
	configs [][]byte
	err     error
}

func (f *fakeLoader) Load(_ context.Context, config []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.configs = append(f.configs, config)
	return nil
}

func (f *fakeLoader) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.configs)
}

func (f *fakeLoader) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.configs) == 0 {
		return ""
	}
	return string(f.configs[len(f.configs)-1])
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	addrs, ok := f[strings.TrimSuffix(host, ".")]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	out := make([]net.IPAddr, len(addrs))
	for i, a := range addrs {
		out[i] = net.IPAddr{IP: net.ParseIP(a)}
	}
	return out, nil
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func createApp(t *testing.T, st *store.Store, slug string) store.App {
	t.Helper()
	app, err := st.CreateApp(context.Background(), store.App{
		Slug: slug, RepoFullName: "octo/" + slug, DefaultBranch: "main",
		WebService: "web", WebPort: 3000, HealthcheckPath: "/",
	}, 20000, 20999, now)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// markDeployed gives the app a successful deployment.
func markDeployed(t *testing.T, st *store.Store, app store.App) {
	t.Helper()
	ctx := context.Background()
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

func newRouter(st *store.Store, loader *fakeLoader) *Router {
	return &Router{
		Store:  st,
		Loader: loader,
		Settings: Settings{
			BaseDomain:      "dev.example.com",
			ConsoleHost:     "console.dev.example.com",
			ConsoleUpstream: "127.0.0.1:8080",
			ACMECA:          "https://acme-staging-v02.api.letsencrypt.org/directory",
			AccessLogDir:    "/var/log/caddy/apps",
		},
	}
}

func TestBuildReflectsDatabase(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	blog := createApp(t, st, "blog")
	shop := createApp(t, st, "shop")
	fresh := createApp(t, st, "fresh")
	markDeployed(t, st, blog)
	markDeployed(t, st, shop)
	if err := st.SetAppDesiredState(ctx, shop.ID, store.DesiredStopped, now); err != nil {
		t.Fatal(err)
	}

	www, _ := st.AddDomain(ctx, blog.ID, "www.example.org", now)
	apex, _ := st.AddDomain(ctx, blog.ID, "example.org", now)
	if _, err := st.AddDomain(ctx, blog.ID, "pending.example.org", now); err != nil {
		t.Fatal(err)
	}
	_ = st.RecordDomainCheck(ctx, www.ID, true, "", now)
	_ = st.RecordDomainCheck(ctx, apex.ID, true, "", now)
	if err := st.SetDomainPrimary(ctx, blog.ID, www.ID, true); err != nil {
		t.Fatal(err)
	}

	config, err := newRouter(st, &fakeLoader{}).Build(ctx)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !json.Valid(config) {
		t.Fatal("the configuration is not valid JSON")
	}
	text := string(config)
	for _, want := range []string{
		"console.dev.example.com", "127.0.0.1:8080",
		"blog.dev.example.com", "www.example.org", "example.org",
		"127.0.0.1:20000", // blog is proxied
		"https://www.example.org{http.request.uri}", // other hostnames redirect to the primary
		"shop.dev.example.com", "fresh.dev.example.com",
		"acme-staging-v02", "/var/log/caddy/apps/blog.log",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("config lacks %q", want)
		}
	}
	// A pending domain must not reach Caddy: it would try to get a certificate.
	if strings.Contains(text, "pending.example.org") {
		t.Error("a pending domain is in the configuration")
	}
	// Stopped and never-deployed apps keep their route but are not proxied.
	for _, port := range []string{"127.0.0.1:20001", "127.0.0.1:20002"} {
		if strings.Contains(text, port) {
			t.Errorf("%s is proxied although its app is not running (%s / %s)", port, shop.Slug, fresh.Slug)
		}
	}
	if strings.Count(text, `"status_code": 503`) != 2 {
		t.Errorf("expected two 503 routes, got %d", strings.Count(text, `"status_code": 503`))
	}

	again, _ := newRouter(st, &fakeLoader{}).Build(ctx)
	if string(again) != text {
		t.Error("Build is not deterministic")
	}
}

func TestSyncLoadsOnlyWhenChanged(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	loader := &fakeLoader{}
	router := newRouter(st, loader)

	if err := router.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := router.Sync(ctx); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if loader.count() != 1 {
		t.Errorf("an unchanged configuration was loaded again: %d loads", loader.count())
	}

	// A redeploy changes nothing that matters to routing.
	app := createApp(t, st, "blog")
	markDeployed(t, st, app)
	_ = router.Sync(ctx)
	markDeployed(t, st, app)
	_ = router.Sync(ctx)
	if loader.count() != 2 {
		t.Errorf("loads = %d, want 2 (one for the new app, none for the redeploy)", loader.count())
	}
	if !strings.Contains(loader.last(), "blog.dev.example.com") {
		t.Error("the new app is missing from the loaded configuration")
	}

	// ForceSync always loads; paasd uses it at startup.
	if err := router.ForceSync(ctx); err != nil {
		t.Fatalf("ForceSync: %v", err)
	}
	if loader.count() != 3 {
		t.Errorf("loads = %d, want 3", loader.count())
	}
}

func TestRouterStatus(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	loader := &fakeLoader{err: errors.New("connection refused")}
	router := newRouter(st, loader)

	if status := router.Status(); status.LastLoadedAt != nil || status.LastError != "" {
		t.Errorf("initial status = %+v", status)
	}
	_ = router.Sync(ctx)
	if status := router.Status(); status.LastLoadedAt != nil || !strings.Contains(status.LastError, "connection refused") {
		t.Errorf("status after a failed load = %+v", status)
	}
	loader.mu.Lock()
	loader.err = nil
	loader.mu.Unlock()
	before := time.Now().Add(-time.Second)
	_ = router.Sync(ctx)
	status := router.Status()
	if status.LastLoadedAt == nil || status.LastLoadedAt.Before(before) || status.LastError != "" {
		t.Errorf("status after a successful load = %+v", status)
	}
}

func TestSyncRetriesAfterFailure(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	loader := &fakeLoader{err: errors.New("connection refused")}
	router := newRouter(st, loader)

	if err := router.Sync(ctx); err == nil || !strings.Contains(err.Error(), "load caddy config") {
		t.Fatalf("err = %v", err)
	}
	loader.mu.Lock()
	loader.err = nil
	loader.mu.Unlock()
	if err := router.Sync(ctx); err != nil {
		t.Fatalf("Sync after recovery: %v", err)
	}
	if loader.count() != 1 {
		t.Errorf("the configuration must be loaded once Caddy is back: %d loads", loader.count())
	}

	// A failure after a success also forces the next load.
	loader.mu.Lock()
	loader.err = errors.New("caddy restarted")
	loader.mu.Unlock()
	createApp(t, st, "blog")
	_ = router.Sync(ctx)
	loader.mu.Lock()
	loader.err = nil
	loader.mu.Unlock()
	_ = router.Sync(ctx)
	if loader.count() != 2 {
		t.Errorf("loads = %d, want 2", loader.count())
	}
}

func newChecker(st *store.Store, resolver fakeResolver, router Syncer) *DomainChecker {
	return &DomainChecker{
		Store:    st,
		Verifier: domains.Verifier{Resolver: resolver, PublicIP: net.ParseIP("203.0.113.10")},
		Router:   router,
		Clock:    clock.NewFake(now),
	}
}

func TestDomainCheckActivatesAndSyncs(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	app := createApp(t, st, "blog")
	markDeployed(t, st, app)
	loader := &fakeLoader{}
	router := newRouter(st, loader)
	resolver := fakeResolver{}
	checker := newChecker(st, resolver, router)

	d, _ := st.AddDomain(ctx, app.ID, "www.example.org", now)
	got, err := checker.Check(ctx, d)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.Status != store.DomainPending || got.LastError == "" || got.LastCheckedAt == nil {
		t.Errorf("unresolvable domain = %+v", got)
	}
	if loader.count() != 0 {
		t.Error("a domain that stays pending must not reload the proxy")
	}

	// DNS now points somewhere else.
	resolver["www.example.org"] = []string{"198.51.100.7"}
	got, _ = checker.Check(ctx, got)
	if got.Status != store.DomainPending || !strings.Contains(got.LastError, "203.0.113.10") {
		t.Errorf("mismatching domain = %+v", got)
	}

	// DNS is fixed: the domain becomes active and is routed.
	resolver["www.example.org"] = []string{"203.0.113.10"}
	got, _ = checker.Check(ctx, got)
	if got.Status != store.DomainActive || got.LastError != "" {
		t.Errorf("verified domain = %+v", got)
	}
	if loader.count() != 1 || !strings.Contains(loader.last(), "www.example.org") {
		t.Errorf("the proxy was not updated: %d loads", loader.count())
	}
}

func TestCheckPendingSkipsActiveDomains(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	app := createApp(t, st, "blog")
	resolver := fakeResolver{"a.example.org": {"203.0.113.10"}, "b.example.org": {"203.0.113.10"}}
	checker := newChecker(st, resolver, nil)

	a, _ := st.AddDomain(ctx, app.ID, "a.example.org", now)
	b, _ := st.AddDomain(ctx, app.ID, "b.example.org", now)
	c, _ := st.AddDomain(ctx, app.ID, "c.example.org", now)
	_ = st.RecordDomainCheck(ctx, a.ID, true, "", now.Add(-time.Hour))

	// An active domain whose DNS later breaks is left alone by the periodic
	// retry; only pending ones are re-verified.
	delete(resolver, "a.example.org")
	checker.CheckPending(ctx)

	gotA, _ := st.GetDomain(ctx, app.ID, a.ID)
	gotB, _ := st.GetDomain(ctx, app.ID, b.ID)
	gotC, _ := st.GetDomain(ctx, app.ID, c.ID)
	if gotA.Status != store.DomainActive || !gotA.LastCheckedAt.Equal(now.Add(-time.Hour)) {
		t.Errorf("active domain was re-checked: %+v", gotA)
	}
	if gotB.Status != store.DomainActive {
		t.Errorf("b = %+v", gotB)
	}
	if gotC.Status != store.DomainPending || gotC.LastError == "" {
		t.Errorf("c = %+v", gotC)
	}
}

func TestCheckDoesNotRecordCanceledLookups(t *testing.T) {
	st := newStore(t)
	app := createApp(t, st, "blog")
	d, _ := st.AddDomain(context.Background(), app.ID, "www.example.org", now)
	checker := newChecker(st, fakeResolver{}, nil)
	checker.Verifier.Resolver = cancelingResolver{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := checker.Check(ctx, d); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	got, _ := st.GetDomain(context.Background(), app.ID, d.ID)
	if got.LastCheckedAt != nil || got.LastError != "" {
		t.Errorf("a canceled check must not be stored: %+v", got)
	}
}

type cancelingResolver struct{}

func (cancelingResolver) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunReloadsRoutesEvenWhenUnchanged(t *testing.T) {
	st := newStore(t)
	loader := &fakeLoader{}
	checker := newChecker(st, fakeResolver{}, newRouter(st, loader))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		checker.Run(ctx, 5*time.Millisecond)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && loader.count() < 3 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	// Nothing changed in the database, yet the configuration is sent again:
	// that is what brings routes back after Caddy restarted on its own.
	if loader.count() < 3 {
		t.Errorf("loads = %d, want repeated loads", loader.count())
	}
}

func TestRunChecksPeriodically(t *testing.T) {
	st := newStore(t)
	app := createApp(t, st, "blog")
	d, _ := st.AddDomain(context.Background(), app.ID, "www.example.org", now)
	loader := &fakeLoader{}
	checker := newChecker(st, fakeResolver{"www.example.org": {"203.0.113.10"}}, newRouter(st, loader))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		checker.Run(ctx, 5*time.Millisecond)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := st.GetDomain(context.Background(), app.ID, d.ID)
		if got.Status == store.DomainActive && loader.count() > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	got, _ := st.GetDomain(context.Background(), app.ID, d.ID)
	if got.Status != store.DomainActive || loader.count() == 0 {
		t.Errorf("domain = %+v, loads = %d", got, loader.count())
	}
}
