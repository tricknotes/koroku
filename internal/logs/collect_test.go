package logs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tricknotes/koroku/internal/runner"
)

func TestCollectWritesOneFilePerServiceAndDay(t *testing.T) {
	s := &script{}
	// 15:00 UTC is midnight in the fixture's location.
	s.add(printsAndStays(
		"web-1           | 2026-11-18T14:59:58.000000001Z hello",
		"job-worker-2-2  | 2026-11-18T14:59:59.5Z w2",
		"job-worker-2-1  | 2026-11-18T14:59:59.25Z w1",
		"\r\x1b[Kweb-1 has been recreated",
		"web-1           | 2026-11-18T15:00:00.000000000Z ",
		"web-1           | 2026-11-18T15:00:01.123456789Z   indented | pipe",
		"db1  | 2026-11-18T15:00:02Z named",
		"",
	))
	f := newFixture(t, func(o *Options) { o.Runner = s.runner(); o.Apps = oneApp(blog) })
	f.startRun()
	waitFor(t, "the last line", func() bool { return len(serviceContent(t, f.dir, "blog", "db1")) == 1 })

	wantStrings(t, "web, 18th", dayContent(t, f.dir, "blog", "web", "2026-11-18"),
		[]string{"2026-11-18T14:59:58.000000001Z hello"})
	wantStrings(t, "web, 19th", dayContent(t, f.dir, "blog", "web", "2026-11-19"),
		[]string{"2026-11-18T15:00:00.000000000Z ", "2026-11-18T15:00:01.123456789Z   indented | pipe"})
	wantStrings(t, "job-worker-2", dayContent(t, f.dir, "blog", "job-worker-2", "2026-11-18"),
		[]string{"2026-11-18T14:59:59.500000000Z w2", "2026-11-18T14:59:59.250000000Z w1"})
	wantStrings(t, "db1", dayContent(t, f.dir, "blog", "db1", "2026-11-19"),
		[]string{"2026-11-18T15:00:02.000000000Z named"})
	// The marker is there because the stream has not paused yet: for all
	// the collector knows, it is still delivering old lines.
	wantStrings(t, "app directory", fileNames(t, filepath.Join(f.dir, "blog")),
		[]string{resumeMarkerName, "db1", "job-worker-2", "web"})
	wantStrings(t, "web files", fileNames(t, filepath.Join(f.dir, "blog", "web")),
		[]string{"2026-11-18.log", "2026-11-19.log"})

	calls := s.seen()
	if len(calls) != 1 {
		t.Fatalf("follow command ran %d times, want 1", len(calls))
	}
	wantArgs := []string{"compose", "-p", "koroku-blog", "logs", "--follow", "--timestamps", "--no-color"}
	if calls[0].Name != "docker" || !reflect.DeepEqual(calls[0].Args, wantArgs) {
		t.Errorf("command = %s %q, want docker %q", calls[0].Name, calls[0].Args, wantArgs)
	}

	services, err := f.c.Services("blog")
	if err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "Services", services, []string{"db1", "job-worker-2", "web"})
}

// resumeLines is what a first run wrote before it ended. Two replicas
// printed an identical line at the same instant, and a third line shares
// their timestamp.
func resumeLines(base time.Time) []string {
	return []string{
		composeLine("web-1", base, "one"),
		composeLine("web-1", base.Add(time.Second), "two"),
		composeLine("web-2", base.Add(time.Second), "two"),
		composeLine("web-1", base.Add(time.Second), "other text, same instant"),
	}
}

// afterResume is what Docker answers to the inclusive --since: everything
// from before, a line of another replica from inside the overlap that never
// made it to disk, and new lines.
func afterResume(base time.Time) []string {
	return append(resumeLines(base),
		composeLine("web-2", base.Add(500*time.Millisecond), "late line of another replica"),
		composeLine("web-1", base.Add(2*time.Second), "three"),
	)
}

var wantAfterResume = []string{
	"one", "two", "two", "other text, same instant", "late line of another replica", "three",
}

func wantSince(t *testing.T, cmd runner.Cmd, since time.Time) {
	t.Helper()
	n := len(cmd.Args)
	if n < 2 || cmd.Args[n-2] != "--since" || cmd.Args[n-1] != since.Format(time.RFC3339Nano) {
		t.Errorf("args = %q, want them to end with --since %s", cmd.Args, since.Format(time.RFC3339Nano))
	}
}

func TestCollectRestartsFollowWithoutDuplicates(t *testing.T) {
	base := t0.Add(123456789 * time.Nanosecond)
	s := &script{}
	s.add(prints(resumeLines(base)...), printsAndStays(afterResume(base)...))
	f := newFixture(t, func(o *Options) { o.Runner = s.runner(); o.Apps = oneApp(blog) })
	f.startRun()
	waitFor(t, "the line after the restart", func() bool {
		return len(serviceContent(t, f.dir, "blog", "web")) >= len(wantAfterResume)
	})
	wantStrings(t, "stored lines", texts(serviceContent(t, f.dir, "blog", "web")), wantAfterResume)

	calls := s.seen()
	if len(calls) != 2 {
		t.Fatalf("follow command ran %d times, want 2", len(calls))
	}
	for _, arg := range calls[0].Args {
		if arg == "--since" {
			t.Errorf("first run has --since although nothing was collected before: %q", calls[0].Args)
		}
	}
	wantSince(t, calls[1], base.Add(time.Second).Add(-resumeOverlap))
}

func TestCollectResumesFromFilesAfterProcessRestart(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		name := "plain files"
		if compressed {
			name = "compressed files"
		}
		t.Run(name, func(t *testing.T) {
			base := t0.Add(123456789 * time.Nanosecond)
			dir := t.TempDir()

			first := &script{}
			lines := resumeLines(base)
			first.add(printsThenLive(lines[:1], lines[1:]))
			f1 := newFixtureIn(t, dir, func(o *Options) { o.Runner = first.runner(); o.Apps = oneApp(blog) })
			stop := f1.startRun()
			waitFor(t, "the first run", func() bool { return len(serviceContent(t, dir, "blog", "web")) == 4 })
			stop()
			if _, err := os.Stat(filepath.Join(dir, "blog", resumeMarkerName)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("marker after the stream went live: %v, want it removed", err)
			}

			if compressed {
				// The app was silent until the next day and the files of
				// the day were compressed.
				f1.clock.Advance(24 * time.Hour)
				if err := f1.c.Maintain(context.Background()); err != nil {
					t.Fatal(err)
				}
				wantStrings(t, "files", fileNames(t, filepath.Join(dir, "blog", "web")), []string{"2026-11-18.log.gz"})
			}

			second := &script{}
			second.add(printsAndStays(afterResume(base)...))
			f2 := newFixtureIn(t, dir, func(o *Options) { o.Runner = second.runner(); o.Apps = oneApp(blog) })
			f2.startRun()
			waitFor(t, "the line after the restart", func() bool {
				return len(serviceContent(t, dir, "blog", "web")) >= len(wantAfterResume)
			})
			wantStrings(t, "stored lines", texts(serviceContent(t, dir, "blog", "web")), wantAfterResume)
			wantSince(t, second.seen()[0], base.Add(time.Second).Add(-resumeOverlap))
		})
	}
}

// A restart that hits while the previous start was still catching up must
// not trust the newest entry: a slower container may be far behind it.
func TestCollectStartsInterruptedBacklogAgain(t *testing.T) {
	base := t0
	dir := t.TempDir()
	all := []held{
		{"web-1", base, "a1"},
		{"web-1", base.Add(60 * time.Second), "a2"},
		{"web-2", base.Add(10 * time.Second), "b1"},
		{"web-2", base.Add(20 * time.Second), "b2"},
	}
	marker := filepath.Join(dir, "blog", resumeMarkerName)
	content := func() []string { return texts(serviceContent(t, dir, "blog", "web")) }
	runUntil := func(handler func(context.Context, runner.Cmd) (string, error), want int) runner.Cmd {
		t.Helper()
		fake := &runner.Fake{Handler: handler}
		f := newFixtureIn(t, dir, func(o *Options) { o.Runner = fake; o.Apps = oneApp(blog) })
		stop := f.startRun()
		waitFor(t, "the lines of this run", func() bool { return len(content()) >= want })
		stop()
		return fake.Calls()[0]
	}
	hasSince := func(cmd runner.Cmd) bool {
		for _, arg := range cmd.Args {
			if arg == "--since" {
				return true
			}
		}
		return false
	}

	// First process: killed after web-1 was delivered completely but
	// web-2 only in part.
	runUntil(daemon(all[:3]), 3)
	if data, err := os.ReadFile(marker); err != nil || strings.TrimSpace(string(data)) != "0001-01-01T00:00:00Z" {
		t.Fatalf("marker = %q, %v; want the start of the unfinished stream", data, err)
	}

	// Second process: the newest entry is a2, 40s after the missing b2.
	// It asks for everything again and is killed during the backlog too.
	cmd := runUntil(daemon(all), 4)
	if hasSince(cmd) {
		t.Errorf("second start used %q although the first never caught up", cmd.Args)
	}
	wantStrings(t, "after the second start", content(), []string{"a1", "a2", "b1", "b2"})
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker after a second interrupted start: %v", err)
	}

	// Third process: catches up and goes live.
	cmd = runUntil(daemon(all, held{"web-1", base.Add(70 * time.Second), "live"}), 5)
	if hasSince(cmd) {
		t.Errorf("third start used %q although the second never caught up", cmd.Args)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker after the stream went live: %v, want it removed", err)
	}

	// Fourth process: an ordinary resume from the newest entry, which
	// leaves a marker with its own start until it is live.
	cmd = runUntil(daemon(append(all, held{"web-1", base.Add(70 * time.Second), "live"}, held{"web-2", base.Add(80 * time.Second), "b3"})), 6)
	since := base.Add(70 * time.Second).Add(-resumeOverlap)
	wantSince(t, cmd, since)
	if data, _ := os.ReadFile(marker); strings.TrimSpace(string(data)) != since.Format(time.RFC3339Nano) {
		t.Errorf("marker = %q, want %s", data, since.Format(time.RFC3339Nano))
	}
	wantStrings(t, "in the end", content(), []string{"a1", "a2", "b1", "b2", "live", "b3"})
}

func TestCollectIgnoresLinesOlderThanSince(t *testing.T) {
	base := t0
	s := &script{}
	s.add(
		prints(composeLine("web-1", base, "now")),
		// A daemon that ignored --since must not cause old lines to be
		// written again.
		printsAndStays(
			composeLine("web-1", base.Add(-time.Hour), "ancient"),
			composeLine("web-1", base.Add(time.Second), "next"),
		),
	)
	f := newFixture(t, func(o *Options) { o.Runner = s.runner(); o.Apps = oneApp(blog) })
	f.startRun()
	waitFor(t, "the second run", func() bool { return len(serviceContent(t, f.dir, "blog", "web")) >= 2 })
	wantStrings(t, "stored lines", texts(serviceContent(t, f.dir, "blog", "web")), []string{"now", "next"})
}

func TestCollectFollowsTheSetOfApps(t *testing.T) {
	var mu sync.Mutex
	apps := []App{blog}
	running := map[string]int{}
	starts := map[string]int{}
	handler := func(ctx context.Context, cmd runner.Cmd) (string, error) {
		project := cmd.Args[2]
		mu.Lock()
		running[project]++
		starts[project]++
		mu.Unlock()
		<-ctx.Done()
		mu.Lock()
		running[project]--
		mu.Unlock()
		return "", ctx.Err()
	}
	state := func(project string) (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return running[project], starts[project]
	}
	setApps := func(list ...App) {
		mu.Lock()
		defer mu.Unlock()
		apps = list
	}
	f := newFixture(t, func(o *Options) {
		o.Runner = &runner.Fake{Handler: handler}
		o.Apps = func(context.Context) ([]App, error) {
			mu.Lock()
			defer mu.Unlock()
			return append([]App(nil), apps...), nil
		}
	})
	stop := f.startRun()

	waitFor(t, "blog to be followed", func() bool { n, _ := state("koroku-blog"); return n == 1 })

	shop := App{Slug: "shop", Project: "koroku-shop"}
	setApps(blog, shop, App{Slug: "../etc", Project: "koroku-bad"}, App{Slug: "noproject"})
	waitFor(t, "shop to be followed", func() bool { n, _ := state("koroku-shop"); return n == 1 })

	setApps(shop)
	waitFor(t, "blog to be released", func() bool { n, _ := state("koroku-blog"); return n == 0 })

	// A changed project restarts the follower with the new name.
	setApps(App{Slug: "shop", Project: "koroku-shop-v2"})
	waitFor(t, "shop to move to the new project", func() bool {
		oldRunning, _ := state("koroku-shop")
		newRunning, _ := state("koroku-shop-v2")
		return oldRunning == 0 && newRunning == 1
	})

	if _, n := state("koroku-blog"); n != 1 {
		t.Errorf("blog was started %d times, want 1", n)
	}
	if _, n := state("koroku-bad"); n != 0 {
		t.Errorf("an app with an invalid slug was followed")
	}
	stop()
	for _, project := range []string{"koroku-blog", "koroku-shop", "koroku-shop-v2"} {
		if n, _ := state(project); n != 0 {
			t.Errorf("%s still has %d follow commands after Run returned", project, n)
		}
	}
}

func TestCollectBacksOffQuietlyWhileFollowFails(t *testing.T) {
	const secret = "s3cr3t-token-in-app-output"
	var mu sync.Mutex
	calls := 0
	healthy := false
	handler := func(ctx context.Context, cmd runner.Cmd) (string, error) {
		mu.Lock()
		calls++
		ok := healthy
		mu.Unlock()
		if !ok {
			return "", &runner.ExitError{Cmd: "docker", Code: 1}
		}
		_, _ = io.WriteString(cmd.Stdout, composeLine("web-1", t0, secret)+"\n")
		<-ctx.Done()
		return "", ctx.Err()
	}
	f := newFixture(t, func(o *Options) {
		o.Runner = &runner.Fake{Handler: handler}
		o.Apps = oneApp(blog)
		o.RestartDelay = 10 * time.Millisecond
		o.MaxRestartDelay = 40 * time.Millisecond
	})
	stop := f.startRun()

	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	failed := calls
	healthy = true
	mu.Unlock()
	// 10 + 20 + 40 + 40 + ... ms: about nine runs. A busy loop would make
	// thousands.
	if failed < 3 || failed > 14 {
		t.Errorf("follow command ran %d times in 300ms, want a backoff between 10ms and 40ms", failed)
	}

	waitFor(t, "the line after recovery", func() bool { return len(serviceContent(t, f.dir, "blog", "web")) == 1 })
	stop()
	logged := f.logs.String()
	if n := strings.Count(logged, "log follow failed"); n != 1 {
		t.Errorf("failure was logged %d times, want once:\n%s", n, logged)
	}
	if strings.Contains(logged, secret) {
		t.Errorf("line content leaked into koroku's log:\n%s", logged)
	}
}

func TestCollectBoundsLongAndBinaryLines(t *testing.T) {
	stamp := "web-1  | 2026-11-18T03:00:0"
	s := &script{}
	s.add(func(ctx context.Context, out io.Writer) error {
		// A line of 200 kB without a newline, delivered in pieces.
		_, _ = io.WriteString(out, stamp+"1Z ")
		for i := 0; i < 200; i++ {
			_, _ = io.WriteString(out, strings.Repeat("x", 1000))
		}
		_, _ = io.WriteString(out, "\n"+stamp+"2Z \x00\xff\xfebin\x1b[31m\x07\r\n")
		_, _ = io.WriteString(out, stamp+"3Z ログ after\n")
		<-ctx.Done()
		return ctx.Err()
	})
	f := newFixture(t, func(o *Options) {
		o.Runner = s.runner()
		o.Apps = oneApp(blog)
		o.MaxLineBytes = 64
	})
	f.startRun()
	waitFor(t, "three lines", func() bool { return len(serviceContent(t, f.dir, "blog", "web")) == 3 })

	data, err := os.ReadFile(filepath.Join(f.dir, "blog", "web", "2026-11-18.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(data) {
		t.Error("the day file is not valid UTF-8")
	}
	wantStrings(t, "stored lines", texts(splitLines(string(data))), []string{
		strings.Repeat("x", 64) + truncatedMark,
		"���bin\x1b[31m�",
		"ログ after",
	})
	lines, err := f.c.Tail("blog", "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 3 || lines[2].Text != "ログ after" || !lines[2].Time.Equal(time.Date(2026, 11, 18, 3, 0, 3, 0, time.UTC)) {
		t.Errorf("Tail = %+v", lines)
	}
}

func TestCollectEndsHalfWrittenLineOfCrashedProcess(t *testing.T) {
	f := newFixture(t, nil)
	dir := filepath.Join(f.dir, "blog", "web")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A process died in the middle of a write.
	half := "2026-11-18T03:00:00.000000000Z complete\n2026-11-18T03:00:01.000000000Z half a li"
	if err := os.WriteFile(filepath.Join(dir, "2026-11-18.log"), []byte(half), 0o600); err != nil {
		t.Fatal(err)
	}
	f.put("blog", "web", t0.Add(2*time.Second), "next")
	wantStrings(t, "stored lines", texts(dayContent(t, f.dir, "blog", "web", "2026-11-18")),
		[]string{"complete", "half a li", "next"})
}

func TestCollectStartsOverAfterWriteError(t *testing.T) {
	// Docker still has both lines every time the command is started.
	handler := func(ctx context.Context, cmd runner.Cmd) (string, error) {
		return "", printsAndStays(
			composeLine("web-1", t0, "lost on a broken disk"),
			composeLine("web-1", t0.Add(time.Second), "after"),
		)(ctx, cmd.Stdout)
	}
	f := newFixture(t, func(o *Options) { o.Runner = &runner.Fake{Handler: handler}; o.Apps = oneApp(blog) })
	// A file where the service directory should be makes the first write fail.
	if err := os.MkdirAll(filepath.Join(f.dir, "blog"), 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(f.dir, "blog", "web")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.startRun()
	waitFor(t, "the failed write to be reported", func() bool {
		return strings.Contains(f.logs.String(), "log follow failed")
	})
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the lines after the disk recovered", func() bool {
		return len(serviceContent(t, f.dir, "blog", "web")) >= 2
	})
	wantStrings(t, "stored lines", texts(serviceContent(t, f.dir, "blog", "web")), []string{"lost on a broken disk", "after"})
	if strings.Contains(f.logs.String(), "lost on a broken disk") {
		t.Error("line content leaked into koroku's log")
	}
}

func TestRunNeedsRunnerAndApps(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.c.Run(context.Background()); err == nil {
		t.Error("Run without Runner and Apps returned nil")
	}
}

func TestRunSurvivesFailingAppList(t *testing.T) {
	var mu sync.Mutex
	fail := true
	s := &script{}
	s.add(printsAndStays(composeLine("web-1", t0, "up")))
	f := newFixture(t, func(o *Options) {
		o.Runner = s.runner()
		o.Apps = func(context.Context) ([]App, error) {
			mu.Lock()
			defer mu.Unlock()
			if fail {
				return nil, errors.New("database is locked")
			}
			return []App{blog}, nil
		}
	})
	f.startRun()
	waitFor(t, "the failure to be reported", func() bool {
		return strings.Contains(f.logs.String(), "cannot list apps")
	})
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	fail = false
	mu.Unlock()
	waitFor(t, "collection to start", func() bool { return len(serviceContent(t, f.dir, "blog", "web")) == 1 })
	if n := strings.Count(f.logs.String(), "cannot list apps"); n != 1 {
		t.Errorf("failure was logged %d times, want once", n)
	}
}

func TestSubscribeNeverBlocksCollection(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.SubscriberBuffer = 2 })
	slow, cancelSlow := f.c.Subscribe("blog")
	fast, cancelFast := f.c.Subscribe("blog")
	other, cancelOther := f.c.Subscribe("shop")
	defer cancelOther()

	var got []Line
	for i := 0; i < 10; i++ {
		// write returns although nobody reads from the slow subscriber.
		f.put("blog", "web", t0.Add(time.Duration(i)*time.Second), strings.Repeat("l", i+1))
		got = append(got, <-fast)
	}
	if len(got) != 10 || got[9].Service != "web" || got[9].Text != "llllllllll" || !got[9].Time.Equal(t0.Add(9*time.Second)) {
		t.Errorf("fast subscriber got %+v", got)
	}

	cancelSlow()
	cancelSlow() // safe to repeat
	var kept []string
	for line := range slow {
		kept = append(kept, line.Text)
	}
	wantStrings(t, "slow subscriber", kept, []string{"l", "ll"})

	select {
	case line := <-other:
		t.Errorf("subscriber of another app got %+v", line)
	default:
	}

	cancelFast()
	if _, open := <-fast; open {
		t.Error("channel still open after cancel")
	}
	f.put("blog", "web", t0.Add(time.Minute), "nobody listens") // must not panic
	if len(f.c.subs) != 1 {
		t.Errorf("%d apps still have subscribers, want only shop", len(f.c.subs))
	}
}

func TestSubscribeReceivesCollectedLines(t *testing.T) {
	s := &script{}
	release := make(chan struct{})
	s.add(func(ctx context.Context, out io.Writer) error {
		<-release
		return printsAndStays(composeLine("web-1", t0, "live"))(ctx, out)
	})
	f := newFixture(t, func(o *Options) { o.Runner = s.runner(); o.Apps = oneApp(blog) })
	lines, cancel := f.c.Subscribe("blog")
	defer cancel()
	f.startRun()
	close(release)
	select {
	case line := <-lines:
		if line.Service != "web" || line.Text != "live" || !line.Time.Equal(t0) {
			t.Errorf("got %+v", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no line arrived")
	}
}
