package idle

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

var t0 = time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)

type fakeSleeper struct {
	mu    sync.Mutex
	slugs []string
	err   error
	st    *store.Store
	clock *clock.Fake
}

func (f *fakeSleeper) Sleep(ctx context.Context, app store.App) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slugs = append(f.slugs, app.Slug)
	if f.err != nil {
		return f.err
	}
	_, err := f.st.MarkAppSleeping(ctx, app.ID, f.clock.Now())
	return err
}

func (f *fakeSleeper) slept() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.slugs)
}

type fixture struct {
	t       *testing.T
	st      *store.Store
	clock   *clock.Fake
	sleeper *fakeSleeper
	tracker *Tracker
	logDir  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	clk := clock.NewFake(t0)
	f := &fixture{t: t, st: st, clock: clk, logDir: t.TempDir()}
	f.sleeper = &fakeSleeper{st: st, clock: clk}
	f.tracker = &Tracker{
		Store: st, Sleeper: f.sleeper, Clock: clk,
		AccessLogDir: f.logDir, ConsoleURL: "https://console.dev.example.com",
	}
	return f
}

// app creates a deployed app; minutes 0 leaves idle sleep off.
func (f *fixture) app(slug string, minutes int, protected bool) store.App {
	f.t.Helper()
	ctx := context.Background()
	app, err := f.st.CreateApp(ctx, store.App{
		Slug: slug, RepoFullName: "octo/" + slug, DefaultBranch: "main",
		WebService: "web", WebPort: 3000, HealthcheckPath: "/",
	}, 20000, 20999, t0)
	if err != nil {
		f.t.Fatal(err)
	}
	app.IdleSleep, app.AccessProtection = minutes > 0, protected
	if minutes > 0 {
		app.IdleSleepMinutes = minutes
	}
	if err := f.st.UpdateApp(ctx, app, t0); err != nil {
		f.t.Fatal(err)
	}
	f.deploy(app)
	return app
}

func (f *fixture) deploy(app store.App) {
	f.t.Helper()
	ctx := context.Background()
	d, _, err := f.st.EnqueueDeployment(ctx, app.ID, "main", strings.Repeat("a", 40), "", t0)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.StartDeployment(ctx, d.ID, t0); err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.FinishDeployment(ctx, d.ID, store.StatusSucceeded, "", t0); err != nil {
		f.t.Fatal(err)
	}
}

// pass advances the clock and runs one check.
func (f *fixture) pass(d time.Duration) {
	f.clock.Advance(d)
	f.tracker.Check(context.Background())
}

func entry(status int, location string) []byte {
	if location == "" {
		return []byte(`{"msg":"handled request","status":` + strconv.Itoa(status) + `}`)
	}
	return []byte(`{"msg":"handled request","status":` + strconv.Itoa(status) + `,"resp_headers":{"Location":["` + location + `"]}}`)
}

func TestIdleAppGoesToSleep(t *testing.T) {
	f := newFixture(t)
	f.app("blog", 30, false)
	f.app("shop", 0, false) // not opted in

	f.pass(time.Minute) // first look: every app counts as just used
	f.pass(20 * time.Minute)
	f.tracker.Observe("blog", entry(200, ""))
	f.pass(29 * time.Minute)
	if got := f.sleeper.slept(); len(got) != 0 {
		t.Fatalf("slept too early: %v", got)
	}
	f.pass(time.Minute)
	if got := f.sleeper.slept(); !slices.Equal(got, []string{"blog"}) {
		t.Fatalf("slept = %v, want [blog]", got)
	}
	// Asleep: not put to sleep again.
	f.pass(time.Hour)
	if got := f.sleeper.slept(); len(got) != 1 {
		t.Errorf("slept again: %v", got)
	}
}

func TestStartupCountsAsUse(t *testing.T) {
	f := newFixture(t)
	f.app("blog", 30, false)
	// The app has been idle for days, but koroku just started.
	f.clock.Advance(72 * time.Hour)
	f.pass(time.Minute)
	f.pass(29 * time.Minute)
	if got := f.sleeper.slept(); len(got) != 0 {
		t.Errorf("slept right after the start: %v", got)
	}
}

func TestWakingCountsAsUse(t *testing.T) {
	f := newFixture(t)
	blog := f.app("blog", 30, false)
	f.pass(time.Minute)
	f.pass(31 * time.Minute)
	if got := f.sleeper.slept(); len(got) != 1 {
		t.Fatalf("slept = %v", got)
	}
	// Woken (by a request through koroku, or the console) long after.
	f.clock.Advance(5 * time.Hour)
	if _, err := f.st.MarkAppAwake(context.Background(), blog.ID); err != nil {
		t.Fatal(err)
	}
	f.pass(time.Minute)
	f.pass(20 * time.Minute)
	if got := f.sleeper.slept(); len(got) != 1 {
		t.Errorf("slept right after waking: %v", got)
	}
	// A new release counts as use too.
	f.pass(5 * time.Minute)
	f.deploy(blog)
	f.pass(time.Minute)
	f.pass(20 * time.Minute)
	if got := f.sleeper.slept(); len(got) != 1 {
		t.Errorf("slept right after a deployment: %v", got)
	}
}

func TestGateAnswersDoNotKeepAProtectedAppAwake(t *testing.T) {
	f := newFixture(t)
	f.app("staging", 30, true)
	f.app("public", 30, false)
	f.pass(time.Minute)
	f.pass(20 * time.Minute)
	for _, slug := range []string{"staging", "public"} {
		f.tracker.Observe(slug, entry(302, "https://console.dev.example.com/auth/gate?host=x&return=%2F"))
		f.tracker.Observe(slug, entry(401, ""))
	}
	f.pass(11 * time.Minute)
	if got := f.sleeper.slept(); !slices.Equal(got, []string{"staging"}) {
		t.Fatalf("slept = %v: the gate's answers keep only an unprotected app awake", got)
	}
	// A request that passed the gate counts, and so does an app's own redirect.
	if !counts(entry(200, ""), true, f.tracker.ConsoleURL) ||
		!counts(entry(302, "https://staging.example.org/login"), true, f.tracker.ConsoleURL) ||
		!counts([]byte("not json"), true, f.tracker.ConsoleURL) {
		t.Error("requests that reached the app must count")
	}
}

func TestChangedLogWithoutEntriesCountsAsUse(t *testing.T) {
	f := newFixture(t)
	f.app("blog", 30, false)
	path := filepath.Join(f.logDir, "blog.log")
	if err := os.WriteFile(path, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.pass(time.Minute)
	f.pass(25 * time.Minute)
	// The file grows, but the collector delivers nothing.
	if err := os.WriteFile(path, []byte("{}\n{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.pass(time.Minute)
	f.pass(25 * time.Minute)
	if got := f.sleeper.slept(); len(got) != 0 {
		t.Fatalf("slept although the access log changed: %v", got)
	}
	// Gate answers that were delivered do not trigger the safeguard.
	f.app("guarded", 30, true)
	gpath := filepath.Join(f.logDir, "guarded.log")
	_ = os.WriteFile(gpath, []byte("{}\n"), 0o644)
	f.pass(time.Minute)
	f.pass(25 * time.Minute)
	_ = os.WriteFile(gpath, []byte("{}\n{}\n"), 0o644)
	f.tracker.Observe("guarded", entry(401, ""))
	f.pass(6 * time.Minute)
	if got := f.sleeper.slept(); !slices.Contains(got, "guarded") {
		t.Errorf("slept = %v, want guarded", got)
	}
}

func TestBusyHostAndBusyAppsWait(t *testing.T) {
	f := newFixture(t)
	f.app("blog", 30, false)
	busy := true
	f.tracker.Busy = func() bool { return busy }
	f.pass(time.Minute)
	f.pass(31 * time.Minute)
	if got := f.sleeper.slept(); len(got) != 0 {
		t.Fatalf("slept during a backup: %v", got)
	}
	busy = false
	f.sleeper.err = deploy.ErrBusy
	f.pass(time.Minute)
	f.sleeper.err = nil
	f.pass(time.Minute)
	if got := f.sleeper.slept(); !slices.Equal(got, []string{"blog", "blog"}) {
		t.Errorf("slept = %v: a busy app is tried again on the next check", got)
	}
}
