package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/store"
)

// copyHarness has a deployed production app "blog" with a dump hook and a
// deployed staging app "blog-staging" with a load hook, whose services
// web, worker and db run.
type copyHarness struct {
	*harness
	source, target store.App
	backups        *fakeCopyBackups

	mu sync.Mutex
	// dump is what the source's dump hook prints; dumpErr ends it.
	dump    string
	dumpErr error
	// loaded is what the load hook read; loadSawEOF tells whether it saw
	// the end of its input (instead of being killed).
	loaded     string
	loadSawEOF bool
	loadErr    error
	// running lists the target services `ps` reports.
	running []string
	// onRun fails `docker compose run` (the post-load command).
	runErr error
}

type fakeCopyBackups struct {
	mu       sync.Mutex
	backedUp []string
	err      error
	latest   *DumpSnapshot
}

func (f *fakeCopyBackups) BackupDump(_ context.Context, app store.App) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.backedUp = append(f.backedUp, app.Slug)
	if f.err != nil {
		return "", f.err
	}
	return "4bba301e5d2c7a90", nil
}

func (f *fakeCopyBackups) LatestDump(context.Context, store.App) (DumpSnapshot, error) {
	if f.latest == nil {
		return DumpSnapshot{}, ErrNoDumpSnapshot
	}
	return *f.latest, nil
}

func newCopyHarness(t *testing.T, mutate func(*Options)) *copyHarness {
	t.Helper()
	backups := &fakeCopyBackups{}
	h := newHarness(t, func(o *Options) {
		o.CopyBackups = backups
		if mutate != nil {
			mutate(o)
		}
	})
	h.createApp("blog")
	h.createApp("blog-staging")
	h.start()
	for _, slug := range []string{"blog", "blog-staging"} {
		if d := h.deploy(slug, shaA); d.Status != store.StatusSucceeded {
			t.Fatalf("deploy %s = %+v", slug, d)
		}
	}
	ctx := context.Background()
	source := h.reload("blog")
	source.Environment = store.EnvironmentProduction
	source.DumpHookService, source.DumpHookCommand = "db", `pg_dump -Fc -U "$POSTGRES_USER" "$POSTGRES_DB"`
	if err := h.store.UpdateApp(ctx, source, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	target := h.reload("blog-staging")
	target.LoadHookService, target.LoadHookCommand = "db", `pg_restore --clean -d "$POSTGRES_DB"`
	if err := h.store.UpdateApp(ctx, target, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	c := &copyHarness{
		harness: h, backups: backups,
		source: h.reload("blog"), target: h.reload("blog-staging"),
		dump:    "PGDMP binary dump\n",
		running: []string{"db", "web", "worker"},
	}
	h.runner.Reset()
	h.mu.Lock()
	h.onCompose = c.compose
	h.mu.Unlock()
	return c
}

func projectOf(args []string) string {
	if i := slices.Index(args, "-p"); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// readInput reads the load hook's standard input until it ends or ctx is
// canceled.
func readInput(ctx context.Context, r io.Reader) (string, bool) {
	ch := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		ch <- string(data)
	}()
	select {
	case data := <-ch:
		return data, true
	case <-ctx.Done():
		return "", false
	}
}

func (c *copyHarness) compose(ctx context.Context, sub string, cmd runner.Cmd) error {
	project := projectOf(cmd.Args)
	switch sub {
	case "ps":
		c.mu.Lock()
		defer c.mu.Unlock()
		if project != "koroku-blog-staging" {
			return nil
		}
		var lines []string
		for _, svc := range c.running {
			lines = append(lines, fmt.Sprintf(`{"Name":"%s-%s-1","Service":"%s","State":"running"}`, project, svc, svc))
		}
		_, err := io.WriteString(cmd.Stdout, strings.Join(lines, "\n")+"\n")
		return err
	case "exec":
		switch project {
		case "koroku-blog":
			c.mu.Lock()
			dump, dumpErr := c.dump, c.dumpErr
			c.mu.Unlock()
			if _, err := io.WriteString(cmd.Stdout, dump); err != nil {
				return err
			}
			io.WriteString(cmd.Stderr, "pg_dump: connecting with password s3cret-production-pw\n")
			return dumpErr
		case "koroku-blog-staging":
			if cmd.Stdin == nil {
				return nil // the post-load command in the db service
			}
			data, eof := readInput(ctx, cmd.Stdin)
			c.mu.Lock()
			defer c.mu.Unlock()
			c.loaded, c.loadSawEOF = data, eof
			if !eof {
				return ctx.Err()
			}
			return c.loadErr
		}
	case "run":
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.runErr
	}
	return nil
}

func (c *copyHarness) startCopy(kind string) (store.DBCopy, error) {
	return c.engine.StartCopy(context.Background(), CopyRequest{Source: c.reload("blog"), Target: c.reload("blog-staging"), Kind: kind})
}

// waitCopy waits until the copy is recorded and its locks are released.
func (c *copyHarness) waitCopy(id int64) store.DBCopy {
	c.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := c.store.GetDBCopy(context.Background(), id)
		if err != nil {
			c.t.Fatal(err)
		}
		lock := c.engine.locks.get(c.target.ID)
		if got.Status != store.CopyRunning && lock.TryLock() {
			lock.Unlock()
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("copy %d did not finish", id)
	return store.DBCopy{}
}

func (c *copyHarness) copyLog(id int64) string {
	c.t.Helper()
	data, err := os.ReadFile(c.engine.CopyLogPath("blog-staging", id))
	if err != nil {
		c.t.Fatal(err)
	}
	return string(data)
}

// composeCalls renders the docker calls after the setup, without the
// project's files: "koroku-blog-staging stop web worker".
func (c *copyHarness) composeCalls() []string {
	var out []string
	for _, call := range c.runner.Calls() {
		sub, _ := composeSubcommand(call.Args)
		i := slices.Index(call.Args, sub)
		out = append(out, projectOf(call.Args)+" "+strings.Join(call.Args[i:], " "))
	}
	return out
}

func TestCopyStreamsTheDumpIntoTheLoadHook(t *testing.T) {
	c := newCopyHarness(t, nil)
	c.setEnv(c.source, map[string]string{"POSTGRES_PASSWORD": "s3cret-production-pw"})
	c.dump = strings.Repeat("COPY posts FROM stdin;\n", 20000) // more than a pipe buffer

	started, err := c.startCopy(store.CopySourceLive)
	if err != nil {
		t.Fatalf("StartCopy: %v", err)
	}
	if started.Status != store.CopyRunning || started.SourceSlug != "blog" {
		t.Errorf("started = %+v", started)
	}
	got := c.waitCopy(started.ID)
	if got.Status != store.CopySucceeded || got.Bytes != int64(len(c.dump)) || got.Error != "" {
		t.Fatalf("copy = %+v", got)
	}
	if c.loaded != c.dump || !c.loadSawEOF {
		t.Errorf("load read %d bytes (eof %v), the dump printed %d", len(c.loaded), c.loadSawEOF, len(c.dump))
	}
	// The target has no dump hook: nothing to back up first.
	if len(c.backups.backedUp) != 0 || got.BackupSnapshotID != "" {
		t.Errorf("backed up %v", c.backups.backedUp)
	}
	want := []string{
		"koroku-blog-staging ps --format json",
		"koroku-blog-staging stop web worker",
		"koroku-blog exec -T db sh -c " + c.source.DumpHookCommand,
		"koroku-blog-staging exec -T db sh -c " + c.target.LoadHookCommand,
		"koroku-blog-staging start web worker",
	}
	calls := c.composeCalls()
	// The two hooks run at the same time; compare them as a set.
	slices.Sort(calls[2:4])
	slices.Sort(want[2:4])
	if !slices.Equal(calls, want) {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	log := c.copyLog(got.ID)
	for _, step := range []string{" check ", " backup ", " stop ", " copy ", " start ", " done "} {
		if !strings.Contains(log, step) {
			t.Errorf("log lacks the %q step:\n%s", step, log)
		}
	}
	if strings.Contains(log, "s3cret-production-pw") || !strings.Contains(log, "dump | pg_dump: connecting with password [MASKED]") {
		t.Errorf("the source's variables must be masked:\n%s", log)
	}
	if strings.Contains(log, "COPY posts") {
		t.Errorf("the dump itself must not be logged")
	}
}

func TestCopyBacksUpTheTargetFirst(t *testing.T) {
	c := newCopyHarness(t, nil)
	target := c.reload("blog-staging")
	target.DumpHookService, target.DumpHookCommand = "db", "pg_dump app"
	if err := c.store.UpdateApp(context.Background(), target, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	started, err := c.startCopy(store.CopySourceLive)
	if err != nil {
		t.Fatal(err)
	}
	got := c.waitCopy(started.ID)
	if got.Status != store.CopySucceeded || got.BackupSnapshotID != "4bba301e5d2c7a90" {
		t.Errorf("copy = %+v", got)
	}
	if !slices.Equal(c.backups.backedUp, []string{"blog-staging"}) {
		t.Errorf("backed up %v", c.backups.backedUp)
	}

	// A failed backup stops the copy before anything is touched.
	c.backups.err = errors.New("another backup is running")
	c.runner.Reset()
	started, _ = c.startCopy(store.CopySourceLive)
	got = c.waitCopy(started.ID)
	if got.Status != store.CopyFailed || !strings.Contains(got.Error, "backup before the copy") {
		t.Errorf("copy = %+v", got)
	}
	if calls := c.composeCalls(); !slices.Equal(calls, []string{"koroku-blog-staging ps --format json"}) {
		t.Errorf("calls after a failed backup = %v", calls)
	}
}

func TestCopyIntoANewReviewAppSkipsTheBackup(t *testing.T) {
	c := newCopyHarness(t, nil)
	target := c.reload("blog-staging")
	target.DumpHookService, target.DumpHookCommand = "db", "pg_dump app"
	if err := c.store.UpdateApp(context.Background(), target, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	started, err := c.engine.StartCopy(context.Background(), CopyRequest{
		Source: c.reload("blog"), Target: c.reload("blog-staging"), Kind: store.CopySourceLive, SkipBackup: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := c.waitCopy(started.ID)
	if got.Status != store.CopySucceeded || got.BackupSnapshotID != "" || len(c.backups.backedUp) != 0 {
		t.Errorf("copy = %+v, backed up %v", got, c.backups.backedUp)
	}
	if log := c.copyLog(got.ID); !strings.Contains(log, "was just created; there is no data to back up first") {
		t.Errorf("log:\n%s", log)
	}
}

func TestFailedDumpNeverReachesTheEndOfTheLoadInput(t *testing.T) {
	c := newCopyHarness(t, nil)
	c.dumpErr = &runner.ExitError{Cmd: "docker", Code: 1}
	started, _ := c.startCopy(store.CopySourceLive)
	got := c.waitCopy(started.ID)
	if got.Status != store.CopyFailed || !strings.Contains(got.Error, "the dump command failed") {
		t.Errorf("copy = %+v", got)
	}
	if c.loadSawEOF {
		t.Error("the load hook saw the end of a failed dump")
	}
	// The stopped services come back.
	if calls := c.composeCalls(); calls[len(calls)-1] != "koroku-blog-staging start web worker" {
		t.Errorf("last call = %q", calls[len(calls)-1])
	}
	if log := c.copyLog(got.ID); !strings.Contains(log, " failed Copy failed: the dump command failed") {
		t.Errorf("log:\n%s", log)
	}
}

func TestEmptyDumpFails(t *testing.T) {
	c := newCopyHarness(t, nil)
	c.dump = ""
	started, _ := c.startCopy(store.CopySourceLive)
	got := c.waitCopy(started.ID)
	if got.Status != store.CopyFailed || got.Error != "the dump command printed nothing" || c.loadSawEOF {
		t.Errorf("copy = %+v, load saw EOF %v", got, c.loadSawEOF)
	}
}

func TestFailedLoadFailsTheCopy(t *testing.T) {
	c := newCopyHarness(t, nil)
	c.loadErr = &runner.ExitError{Cmd: "docker", Code: 1}
	started, _ := c.startCopy(store.CopySourceLive)
	got := c.waitCopy(started.ID)
	if got.Status != store.CopyFailed || !strings.Contains(got.Error, "the load command failed") {
		t.Errorf("copy = %+v", got)
	}
}

func TestCopyStopsAtTheSizeLimit(t *testing.T) {
	c := newCopyHarness(t, func(o *Options) { o.CopyMaxBytes = 1000 })
	c.dump = strings.Repeat("x", 5000)
	started, _ := c.startCopy(store.CopySourceLive)
	got := c.waitCopy(started.ID)
	if got.Status != store.CopyFailed || !strings.Contains(got.Error, "larger than the limit of 1.0 kB") || c.loadSawEOF {
		t.Errorf("copy = %+v, load saw EOF %v", got, c.loadSawEOF)
	}
}

func TestPostLoadCommandRunsBeforeTheAppIsServed(t *testing.T) {
	c := newCopyHarness(t, nil)
	target := c.reload("blog-staging")
	target.PostLoadService, target.PostLoadCommand = "web", "bin/rails db:anonymize"
	if err := c.store.UpdateApp(context.Background(), target, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	started, _ := c.startCopy(store.CopySourceLive)
	got := c.waitCopy(started.ID)
	if got.Status != store.CopySucceeded {
		t.Fatalf("copy = %+v", got)
	}
	run := c.lastCall("run")
	release := c.engine.ReleaseDir("blog-staging", *target.CurrentDeploymentID)
	if run.Dir != release || !slices.Equal(run.Args[len(run.Args)-8:],
		[]string{"run", "--rm", "--no-deps", "-T", "web", "sh", "-c", "bin/rails db:anonymize"}) ||
		!slices.Contains(run.Args, filepath.Join(release, "compose.yaml")) {
		t.Errorf("run = %+v", run)
	}
	calls := c.composeCalls()
	if !strings.HasPrefix(calls[len(calls)-2], "koroku-blog-staging run") || calls[len(calls)-1] != "koroku-blog-staging start web worker" {
		t.Errorf("the command must run before the services start: %v", calls)
	}

	// When it fails, the services stay stopped.
	c.runErr = &runner.ExitError{Cmd: "docker", Code: 1}
	c.runner.Reset()
	started, _ = c.startCopy(store.CopySourceLive)
	got = c.waitCopy(started.ID)
	if got.Status != store.CopyFailed || !strings.Contains(got.Error, "the command after the copy failed") {
		t.Errorf("copy = %+v", got)
	}
	for _, call := range c.composeCalls() {
		if strings.Contains(call, " start ") {
			t.Errorf("services were started after a failed post-load command: %v", call)
		}
	}
	if log := c.copyLog(got.ID); !strings.Contains(log, "stay stopped") {
		t.Errorf("log:\n%s", log)
	}
}

func TestPostLoadCommandInTheLoadServiceUsesExec(t *testing.T) {
	c := newCopyHarness(t, nil)
	target := c.reload("blog-staging")
	target.PostLoadService, target.PostLoadCommand = "db", "psql -c 'UPDATE users SET email = id'"
	if err := c.store.UpdateApp(context.Background(), target, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	started, _ := c.startCopy(store.CopySourceLive)
	if got := c.waitCopy(started.ID); got.Status != store.CopySucceeded {
		t.Fatalf("copy = %+v", got)
	}
	want := "koroku-blog-staging exec -T db sh -c psql -c 'UPDATE users SET email = id'"
	if !slices.Contains(c.composeCalls(), want) {
		t.Errorf("calls = %v", c.composeCalls())
	}
}

func TestCopyFromSnapshot(t *testing.T) {
	c := newCopyHarness(t, nil)
	c.backups.latest = &DumpSnapshot{ID: "9f2c41d07a1b3e55", Time: c.clock.Now(), Write: func(_ context.Context, w io.Writer) error {
		_, err := io.WriteString(w, "snapshot dump\n")
		return err
	}}
	// The source need not run: its containers are not used.
	source := c.reload("blog")
	if err := c.store.SetAppDesiredState(context.Background(), source.ID, store.DesiredStopped, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	started, err := c.startCopy(store.CopySourceSnapshot)
	if err != nil {
		t.Fatalf("StartCopy: %v", err)
	}
	got := c.waitCopy(started.ID)
	if got.Status != store.CopySucceeded || c.loaded != "snapshot dump\n" || got.SourceKind != store.CopySourceSnapshot {
		t.Errorf("copy = %+v, loaded %q", got, c.loaded)
	}
	for _, call := range c.composeCalls() {
		if strings.HasPrefix(call, "koroku-blog ") {
			t.Errorf("the source's containers were used: %s", call)
		}
	}

	c.backups.latest = nil
	if _, err := c.startCopy(store.CopySourceSnapshot); !errors.Is(err, ErrNoDumpSnapshot) {
		t.Errorf("without a snapshot: %v", err)
	}
}

func TestCopyRefusals(t *testing.T) {
	c := newCopyHarness(t, nil)
	ctx := context.Background()
	update := func(slug string, change func(*store.App)) {
		app := c.reload(slug)
		change(&app)
		if err := c.store.UpdateApp(ctx, app, c.clock.Now()); err != nil {
			t.Fatal(err)
		}
	}
	try := func(kind string) error {
		_, err := c.startCopy(kind)
		return err
	}

	if _, err := c.engine.StartCopy(ctx, CopyRequest{Source: c.target, Target: c.target, Kind: store.CopySourceLive}); !errors.Is(err, ErrSameApp) {
		t.Errorf("same app: %v", err)
	}
	if _, err := c.engine.StartCopy(ctx, CopyRequest{Source: c.target, Target: c.source, Kind: store.CopySourceLive}); !errors.Is(err, ErrTargetNotStaging) {
		t.Errorf("production target: %v", err)
	}

	// Busy: a deployment of either app, or another operation.
	lock := c.engine.locks.get(c.source.ID)
	lock.Lock()
	if err := try(store.CopySourceLive); !errors.Is(err, ErrBusy) {
		t.Errorf("source busy: %v", err)
	}
	// A snapshot copy does not need the source.
	c.backups.latest = &DumpSnapshot{Write: func(_ context.Context, w io.Writer) error { _, err := io.WriteString(w, "x"); return err }}
	if started, err := c.startCopy(store.CopySourceSnapshot); err != nil {
		t.Errorf("snapshot copy while the source is busy: %v", err)
	} else {
		c.waitCopy(started.ID)
	}
	lock.Unlock()
	targetLock := c.engine.locks.get(c.target.ID)
	targetLock.Lock()
	for _, kind := range []string{store.CopySourceLive, store.CopySourceSnapshot} {
		if err := try(kind); !errors.Is(err, ErrBusy) {
			t.Errorf("target busy (%s): %v", kind, err)
		}
	}
	targetLock.Unlock()
	// The source's lock is released again when the target's is not free.
	if lock.TryLock() {
		lock.Unlock()
	} else {
		t.Error("a refused copy kept the source's lock")
	}

	update("blog-staging", func(a *store.App) { a.LoadHookCommand = "" })
	if err := try(store.CopySourceLive); !errors.Is(err, ErrNoLoadHook) {
		t.Errorf("no load hook: %v", err)
	}
	update("blog-staging", func(a *store.App) { a.LoadHookCommand = "pg_restore" })
	update("blog", func(a *store.App) { a.DumpHookService, a.DumpHookCommand = "", "" })
	if err := try(store.CopySourceLive); !errors.Is(err, ErrNoDumpHook) {
		t.Errorf("no dump hook: %v", err)
	}
	update("blog", func(a *store.App) { a.DumpHookService, a.DumpHookCommand = "db", "pg_dump" })
	if err := c.store.SetAppDesiredState(ctx, c.source.ID, store.DesiredStopped, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := try(store.CopySourceLive); !errors.Is(err, ErrSourceNotRunning) {
		t.Errorf("stopped source: %v", err)
	}
	if err := c.store.SetAppDesiredState(ctx, c.target.ID, store.DesiredStopped, c.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if err := try(store.CopySourceSnapshot); !errors.Is(err, ErrTargetNotRunning) {
		t.Errorf("stopped target: %v", err)
	}
}

func TestCopyWithoutBackupsRefusesSnapshots(t *testing.T) {
	c := newCopyHarness(t, func(o *Options) { o.CopyBackups = nil })
	if _, err := c.startCopy(store.CopySourceSnapshot); !errors.Is(err, ErrBackupsNotConfigured) {
		t.Errorf("snapshot without backups: %v", err)
	}
	started, err := c.startCopy(store.CopySourceLive)
	if err != nil {
		t.Fatal(err)
	}
	got := c.waitCopy(started.ID)
	if got.Status != store.CopySucceeded || !strings.Contains(c.copyLog(got.ID), "Backups are not configured") {
		t.Errorf("copy = %+v", got)
	}
}

func TestLoadServiceMustRun(t *testing.T) {
	c := newCopyHarness(t, nil)
	c.running = []string{"web"}
	started, _ := c.startCopy(store.CopySourceLive)
	got := c.waitCopy(started.ID)
	if got.Status != store.CopyFailed || !strings.Contains(got.Error, "load hook service db of blog-staging is not running") {
		t.Errorf("copy = %+v", got)
	}
	for _, call := range c.composeCalls() {
		if strings.Contains(call, " stop ") || strings.Contains(call, " exec ") {
			t.Errorf("nothing may be touched: %s", call)
		}
	}
}

func TestInterruptedCopiesFailAtStartup(t *testing.T) {
	h := newHarness(t, nil)
	source, target := h.createApp("blog"), h.createApp("blog-staging")
	c, err := h.store.StartDBCopy(context.Background(), target, source, store.CopySourceLive, h.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	h.start()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := h.store.GetDBCopy(context.Background(), c.ID)
		if got.Status == store.CopyFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("copy = %+v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	data, _ := os.ReadFile(h.engine.CopyLogPath("blog-staging", c.ID))
	if !strings.Contains(string(data), "start the app from the console") {
		t.Errorf("log = %q", data)
	}
}
