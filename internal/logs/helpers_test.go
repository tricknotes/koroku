package logs

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/objstore"
	"github.com/tricknotes/koroku/internal/runner"
)

// jst stands in for the production location without needing tzdata.
var jst = time.FixedZone("JST", 9*60*60)

// t0 is 2026-11-18 12:00 in jst.
var t0 = time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)

// syncBuffer is a log sink that can be read while loops still write to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type fixture struct {
	t     *testing.T
	c     *Collector
	clock *clock.Fake
	dir   string
	logs  *syncBuffer
}

// newFixture builds a Collector on a temporary directory with a fake clock
// and intervals short enough for tests.
func newFixture(t *testing.T, mod func(*Options)) *fixture {
	t.Helper()
	return newFixtureIn(t, t.TempDir(), mod)
}

func newFixtureIn(t *testing.T, dir string, mod func(*Options)) *fixture {
	t.Helper()
	f := &fixture{t: t, clock: clock.NewFake(t0), dir: dir, logs: &syncBuffer{}}
	opts := Options{
		Dir:                dir,
		Clock:              f.clock,
		Location:           jst,
		Logger:             slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ReconcileInterval:  5 * time.Millisecond,
		RestartDelay:       5 * time.Millisecond,
		MaxRestartDelay:    20 * time.Millisecond,
		AccessPollInterval: 5 * time.Millisecond,
		MaintainInterval:   5 * time.Millisecond,
	}
	if mod != nil {
		mod(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.settleIdle = testSettleIdle
	f.c = c
	return f
}

// testSettleIdle replaces the pause after which a stream counts as live.
const testSettleIdle = 5 * time.Millisecond

// printsThenLive is a run that prints a backlog in one burst, pauses long
// enough for the collector to see that the backlog is over, prints more and
// keeps following.
func printsThenLive(backlog, live []string) func(context.Context, io.Writer) error {
	return func(ctx context.Context, out io.Writer) error {
		if _, err := io.WriteString(out, strings.Join(backlog, "\n")+"\n"); err != nil {
			return err
		}
		time.Sleep(10 * testSettleIdle)
		return printsAndStays(live...)(ctx, out)
	}
}

// held is a line the fake Docker daemon still has.
type held struct {
	container string
	ts        time.Time
	text      string
}

// daemon answers a follow command like Docker: with every line it holds
// that is not older than --since, then, after a pause, with live lines.
func daemon(backlog []held, live ...held) func(ctx context.Context, cmd runner.Cmd) (string, error) {
	format := func(lines []held, since time.Time) []string {
		var out []string
		for _, l := range lines {
			if !l.ts.Before(since) {
				out = append(out, composeLine(l.container, l.ts, l.text))
			}
		}
		return out
	}
	return func(ctx context.Context, cmd runner.Cmd) (string, error) {
		var since time.Time
		for i, arg := range cmd.Args {
			if arg == "--since" {
				since, _ = time.Parse(time.RFC3339Nano, cmd.Args[i+1])
			}
		}
		if len(live) == 0 {
			return "", printsAndStays(format(backlog, since)...)(ctx, cmd.Stdout)
		}
		return "", printsThenLive(format(backlog, since), format(live, since))(ctx, cmd.Stdout)
	}
}

// start runs fn in the background and returns a function that cancels it
// and fails the test when it does not return promptly. It also runs at
// cleanup, so no loop outlives its test.
func start(t *testing.T, fn func(ctx context.Context)) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("loop did not stop within 5s of cancellation")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func (f *fixture) startRun() (stop func()) {
	f.t.Helper()
	return start(f.t, func(ctx context.Context) {
		if err := f.c.Run(ctx); err != nil {
			f.t.Errorf("Run: %v", err)
		}
	})
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// script plays the role of `docker compose logs --follow`: each run of the
// command takes the next entry. When the entries are used up, the command
// stays silent until it is killed.
type script struct {
	mu    sync.Mutex
	runs  []func(ctx context.Context, out io.Writer) error
	calls []runner.Cmd
}

func (s *script) add(runs ...func(ctx context.Context, out io.Writer) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs = append(s.runs, runs...)
}

func (s *script) handle(ctx context.Context, cmd runner.Cmd) (string, error) {
	s.mu.Lock()
	s.calls = append(s.calls, cmd)
	var run func(ctx context.Context, out io.Writer) error
	if len(s.runs) > 0 {
		run, s.runs = s.runs[0], s.runs[1:]
	}
	s.mu.Unlock()
	if run == nil {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "", run(ctx, cmd.Stdout)
}

func (s *script) seen() []runner.Cmd {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]runner.Cmd(nil), s.calls...)
}

func (s *script) runner() *runner.Fake { return &runner.Fake{Handler: s.handle} }

// prints is a run that prints lines and then ends, like a follow whose
// containers were removed.
func prints(lines ...string) func(context.Context, io.Writer) error {
	return func(_ context.Context, out io.Writer) error {
		_, err := io.WriteString(out, strings.Join(lines, "\n")+"\n")
		return err
	}
}

// printsAndStays is a run that prints lines and keeps following.
func printsAndStays(lines ...string) func(context.Context, io.Writer) error {
	return func(ctx context.Context, out io.Writer) error {
		if _, err := io.WriteString(out, strings.Join(lines, "\n")+"\n"); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

// composeLine formats a line the way Compose prints it.
func composeLine(container string, ts time.Time, text string) string {
	return fmt.Sprintf("%-14s  | %s %s", container, ts.UTC().Format("2006-01-02T15:04:05.000000000Z"), text)
}

func oneApp(app App) func(context.Context) ([]App, error) {
	return func(context.Context) ([]App, error) { return []App{app}, nil }
}

var blog = App{Slug: "blog", Project: "paas-blog"}

// dayContent returns the raw lines of one day of a service, gzip part first.
func dayContent(t *testing.T, dir, slug, service, date string) []string {
	t.Helper()
	stream, err := openDay(dayFiles{dir: filepath.Join(dir, slug, service), date: date})
	if err != nil {
		t.Fatalf("open day %s: %v", date, err)
	}
	defer stream.Close()
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read day %s: %v", date, err)
	}
	return splitLines(string(data))
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// serviceContent returns every stored line of a service, oldest day first.
func serviceContent(t *testing.T, dir, slug, service string) []string {
	t.Helper()
	days, err := listDays(filepath.Join(dir, slug, service))
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, d := range days {
		lines = append(lines, dayContent(t, dir, slug, service, d.date)...)
	}
	return lines
}

// texts strips the timestamps from stored lines.
func texts(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		_, text, _ := strings.Cut(line, " ")
		out = append(out, text)
	}
	return out
}

func lineTexts(lines []Line) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, line.Text)
	}
	return out
}

func fileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func wantStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got  %q\n want %q", what, got, want)
	}
}

// put writes lines straight into the files, as the collector would.
func (f *fixture) put(slug, service string, ts time.Time, text string) {
	f.t.Helper()
	if err := f.c.write(slug, []Line{{Service: service, Time: ts, Text: text}}); err != nil {
		f.t.Fatalf("write: %v", err)
	}
}

// objectLines returns the decompressed lines of a stored object.
func objectLines(t *testing.T, store objstore.Store, key string) []string {
	t.Helper()
	body, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get %s: %v", key, err)
	}
	defer body.Close()
	zr, err := gzip.NewReader(body)
	if err != nil {
		t.Fatalf("object %s is not gzip: %v", key, err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return splitLines(string(data))
}

func objectKeys(t *testing.T, store objstore.Store) []string {
	t.Helper()
	objects, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.Key)
	}
	return keys
}
