package logs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// accessEntry is a line as Caddy's JSON encoder writes it, shortened.
func accessEntry(ts float64, uri string) string {
	return fmt.Sprintf(`{"level":"info","ts":%.6f,"logger":"http.log.access.blog.app","msg":"handled request","request":{"method":"GET","host":"blog.dev.example.com","uri":%q},"status":200}`, ts, uri)
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func uris(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		_, rest, _ := strings.Cut(line, `"uri":"`)
		uri, _, _ := strings.Cut(rest, `"`)
		out = append(out, uri)
	}
	return out
}

// newAccessTail returns a tail that the test drives by calling poll.
func newAccessTail(f *fixture, path string) *accessTail {
	a := &accessTail{c: f.c, app: App{Slug: "blog", Project: "koroku-blog", AccessLogPath: path}}
	a.sp = splitter{max: f.c.maxLine, emit: a.line}
	a.chunk = make([]byte, 64<<10)
	f.t.Cleanup(a.closeFile)
	return a
}

func mustPoll(t *testing.T, a *accessTail) {
	t.Helper()
	if err := a.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func TestAccessTailUsesEntryTime(t *testing.T) {
	f := newFixture(t, nil)
	path := filepath.Join(t.TempDir(), "blog.log")
	a := newAccessTail(f, path)

	mustPoll(t, a) // no file yet: nothing to do, no error

	// 1795000000 is 2026-11-18T11:06:40Z.
	appendFile(t, path, accessEntry(1795000000.25, "/first")+"\n"+accessEntry(1795000001.5, "/second")+"\n")
	appendFile(t, path, `{"msg":"entry without a time"}`+"\n"+"not json at all\n\n")
	appendFile(t, path, `{"ts":"2026-11-18T11:30:00.5Z","uri":"/string-time"}`+"\n")
	appendFile(t, path, accessEntry(1795000002, "/half")[:40])
	mustPoll(t, a)

	lines, err := f.c.Tail("blog", AccessService, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 5 {
		t.Fatalf("got %d lines, want 5: %+v", len(lines), lines)
	}
	byText := map[string]time.Time{}
	for _, line := range lines {
		if line.Service != AccessService {
			t.Errorf("service = %q", line.Service)
		}
		byText[line.Text] = line.Time
	}
	for text, want := range map[string]time.Time{
		accessEntry(1795000000.25, "/first"):                   time.Date(2026, 11, 18, 11, 6, 40, 250000000, time.UTC),
		accessEntry(1795000001.5, "/second"):                   time.Date(2026, 11, 18, 11, 6, 41, 500000000, time.UTC),
		`{"ts":"2026-11-18T11:30:00.5Z","uri":"/string-time"}`: time.Date(2026, 11, 18, 11, 30, 0, 500000000, time.UTC),
		// Entries without a usable time get the time they were read.
		`{"msg":"entry without a time"}`: t0,
		"not json at all":                t0,
	} {
		if got, ok := byText[text]; !ok || !got.Equal(want) {
			t.Errorf("time of %q = %v (found %v), want %v", text, got, ok, want)
		}
	}

	// The unfinished line is taken once it is complete.
	appendFile(t, path, accessEntry(1795000002, "/half")[40:]+"\n")
	mustPoll(t, a)
	got := uris(serviceContent(t, f.dir, "blog", AccessService))
	if len(got) != 6 || got[5] != "/half" {
		t.Errorf("after completing the line: %q", got)
	}
}

func TestAccessTailFollowsRotationAndTruncation(t *testing.T) {
	f := newFixture(t, nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "blog.log")
	a := newAccessTail(f, path)

	appendFile(t, path, accessEntry(1795000000, "/a")+"\n"+accessEntry(1795000001, "/b")+"\n")
	mustPoll(t, a)

	// Caddy renames the full file and starts a new one. An entry written
	// just before the rename must not be lost.
	appendFile(t, path, accessEntry(1795000002, "/before-rotation")+"\n")
	if err := os.Rename(path, filepath.Join(dir, "blog-2026-11-18T11-06-42.000.log")); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, accessEntry(1795000003, "/c")+"\n"+accessEntry(1795000004, "/d")+"\n")
	mustPoll(t, a)
	wantStrings(t, "after rotation", uris(serviceContent(t, f.dir, "blog", AccessService)),
		[]string{"/a", "/b", "/before-rotation", "/c", "/d"})

	// Rotated away without a successor yet.
	if err := os.Rename(path, filepath.Join(dir, "blog-2026-11-18T11-06-45.000.log")); err != nil {
		t.Fatal(err)
	}
	mustPoll(t, a)
	appendFile(t, path, accessEntry(1795000005, "/e")+"\n"+accessEntry(1795000005.5, "/e2")+"\n")
	mustPoll(t, a)

	// Truncated in place (copytruncate style rotation). It shows as a
	// file that is shorter than what was read from it.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, accessEntry(1795000006, "/f")+"\n")
	mustPoll(t, a)
	mustPoll(t, a)
	wantStrings(t, "after truncation", uris(serviceContent(t, f.dir, "blog", AccessService)),
		[]string{"/a", "/b", "/before-rotation", "/c", "/d", "/e", "/e2", "/f"})
}

func TestAccessTailResumesWithoutDuplicates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "blog.log")
	appendFile(t, path, accessEntry(1795000000, "/a")+"\n"+accessEntry(1795000001, "/b")+"\n"+"no time, first pass\n")

	f1 := newFixtureIn(t, dir, nil)
	a1 := newAccessTail(f1, path)
	mustPoll(t, a1)
	a1.closeFile()
	f1.c.closeWriters("blog")
	if n := len(serviceContent(t, dir, "blog", AccessService)); n != 3 {
		t.Fatalf("first pass stored %d lines, want 3", n)
	}

	// While koroku was down: one request, and one entry that sorts into the
	// overlap but was never collected.
	appendFile(t, path, accessEntry(1795000002, "/while-down")+"\n"+accessEntry(1795000000.5, "/late")+"\n")

	f2 := newFixtureIn(t, dir, nil)
	// The timestamp-less line was stored with the clock's time, which is
	// older than the entries; move on so that the overlap is well defined.
	a2 := newAccessTail(f2, path)
	mustPoll(t, a2)
	mustPoll(t, a2)
	appendFile(t, path, accessEntry(1795000003, "/after")+"\n")
	mustPoll(t, a2)

	got := serviceContent(t, dir, "blog", AccessService)
	var seen []string
	for _, line := range got {
		if strings.Contains(line, "no time") {
			seen = append(seen, "no-time")
			continue
		}
		seen = append(seen, uris([]string{line})[0])
	}
	wantStrings(t, "stored entries", seen, []string{"/a", "/b", "no-time", "/while-down", "/late", "/after"})
}

func TestAccessLogIsCollectedByRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blog.log")
	appendFile(t, path, accessEntry(1795000000, "/a")+"\n")
	app := App{Slug: "blog", Project: "koroku-blog", AccessLogPath: path}
	s := &script{}
	f := newFixture(t, func(o *Options) { o.Runner = s.runner(); o.Apps = oneApp(app) })
	lines, cancel := f.c.Subscribe("blog")
	defer cancel()
	f.startRun()

	select {
	case line := <-lines:
		if line.Service != AccessService || !strings.Contains(line.Text, `"uri":"/a"`) {
			t.Errorf("got %+v", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the access log entry was not collected")
	}
	appendFile(t, path, accessEntry(1795000001, "/b")+"\n")
	waitFor(t, "the appended entry", func() bool {
		return len(serviceContent(t, f.dir, "blog", AccessService)) == 2
	})
	// The access log does not take part in the resume point of the
	// container logs.
	for _, call := range s.seen() {
		for _, arg := range call.Args {
			if arg == "--since" {
				t.Errorf("follow command got --since from access log entries: %q", call.Args)
			}
		}
	}
}
