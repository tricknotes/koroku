package logs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// historyFixture has three days of two services; the first two days are
// compressed, the last is the plain file of the current day.
func historyFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, nil)
	f.clock.Set(at(20, 10, 0))
	for day := 18; day <= 20; day++ {
		for hour := 1; hour <= 3; hour++ {
			f.put("blog", "web", at(day, hour, 0), fmt.Sprintf("web d%d h%d GET /posts", day, hour))
			f.put("blog", "db", at(day, hour, 30), fmt.Sprintf("db d%d h%d checkpoint", day, hour))
		}
	}
	f.put("blog", "web", at(20, 3, 0), "web d20 h3 again, same instant")
	f.put("shop", "web", at(20, 1, 0), "another app")
	mustMaintain(t, f.c)
	wantStrings(t, "web files", fileNames(t, filepath.Join(f.dir, "blog", "web")),
		[]string{"2026-11-18.log.gz", "2026-11-19.log.gz", "2026-11-20.log"})
	return f
}

func search(t *testing.T, f *fixture, q Query) Result {
	t.Helper()
	res, err := f.c.Search(context.Background(), "blog", q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	for i := 1; i < len(res.Lines); i++ {
		if res.Lines[i].Time.Before(res.Lines[i-1].Time) {
			t.Errorf("Search(%+v): line %d is older than the one before", q, i)
		}
	}
	return res
}

func TestSearch(t *testing.T) {
	f := historyFixture(t)

	all := search(t, f, Query{})
	if len(all.Lines) != 19 || all.Truncated {
		t.Fatalf("everything: %d lines, truncated=%v; want 19, false", len(all.Lines), all.Truncated)
	}
	if first := all.Lines[0]; first.Service != "web" || first.Text != "web d18 h1 GET /posts" || !first.Time.Equal(at(18, 1, 0)) {
		t.Errorf("first line = %+v", first)
	}
	if second := all.Lines[1]; second.Service != "db" || !second.Time.Equal(at(18, 1, 30)) {
		t.Errorf("second line = %+v, want services merged by time", second)
	}
	if all.Lines[0].Time.Location() != time.UTC {
		t.Errorf("times are in %v, want UTC", all.Lines[0].Time.Location())
	}

	wantStrings(t, "one service", lineTexts(search(t, f, Query{Service: "db", From: at(19, 3, 0)}).Lines),
		[]string{"db d19 h3 checkpoint", "db d20 h1 checkpoint", "db d20 h2 checkpoint", "db d20 h3 checkpoint"})

	// From is inclusive, To is exclusive; the range crosses a compressed
	// and the plain file.
	wantStrings(t, "time range", lineTexts(search(t, f, Query{Service: "web", From: at(19, 2, 0), To: at(20, 2, 0)}).Lines),
		[]string{"web d19 h2 GET /posts", "web d19 h3 GET /posts", "web d20 h1 GET /posts"})

	wantStrings(t, "substring", lineTexts(search(t, f, Query{Contains: "h3 again"}).Lines),
		[]string{"web d20 h3 again, same instant"})
	wantStrings(t, "substring is case-sensitive", lineTexts(search(t, f, Query{Contains: "get /posts"}).Lines), nil)
	wantStrings(t, "substring and range", lineTexts(search(t, f, Query{Contains: "checkpoint", From: at(20, 2, 0), To: at(20, 3, 0)}).Lines),
		[]string{"db d20 h2 checkpoint"})

	// Lines with the same timestamp stay in the order they were written.
	wantStrings(t, "same instant", lineTexts(search(t, f, Query{Service: "web", From: at(20, 3, 0)}).Lines),
		[]string{"web d20 h3 GET /posts", "web d20 h3 again, same instant"})

	limited := search(t, f, Query{Limit: 5})
	if len(limited.Lines) != 5 || !limited.Truncated {
		t.Errorf("limit 5: %d lines, truncated=%v; want 5, true", len(limited.Lines), limited.Truncated)
	}
	wantStrings(t, "limit keeps the oldest", lineTexts(limited.Lines), lineTexts(all.Lines[:5]))
	if exact := search(t, f, Query{Limit: 19}); len(exact.Lines) != 19 || exact.Truncated {
		t.Errorf("limit equal to the number of matches: %d lines, truncated=%v", len(exact.Lines), exact.Truncated)
	}

	if res := search(t, f, Query{Service: "nosuch"}); len(res.Lines) != 0 || res.Truncated {
		t.Errorf("unknown service: %+v", res)
	}
	if res, err := f.c.Search(context.Background(), "nosuchapp", Query{}); err != nil || len(res.Lines) != 0 {
		t.Errorf("unknown app: %+v, %v", res, err)
	}
}

func TestSearchStopsWhenCanceled(t *testing.T) {
	f := historyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.c.Search(ctx, "blog", Query{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Search with a canceled context: %v", err)
	}
}

func TestTail(t *testing.T) {
	f := historyFixture(t)

	lines, err := f.c.Tail("blog", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "all services", lineTexts(lines),
		[]string{"web d20 h3 GET /posts", "web d20 h3 again, same instant", "db d20 h3 checkpoint"})
	if lines[2].Service != "db" || !lines[2].Time.Equal(at(20, 3, 30)) {
		t.Errorf("last line = %+v", lines[2])
	}

	// More than the current day holds: continues into compressed days.
	lines, err = f.c.Tail("blog", "web", 6)
	if err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "into the compressed day", lineTexts(lines), []string{
		"web d19 h2 GET /posts", "web d19 h3 GET /posts",
		"web d20 h1 GET /posts", "web d20 h2 GET /posts", "web d20 h3 GET /posts", "web d20 h3 again, same instant",
	})

	lines, err = f.c.Tail("blog", "db", 1000)
	if err != nil || len(lines) != 9 {
		t.Errorf("more than exists: %d lines, %v; want 9", len(lines), err)
	}
	if lines, err := f.c.Tail("blog", "web", 0); err != nil || len(lines) != 0 {
		t.Errorf("n = 0: %d lines, %v", len(lines), err)
	}
	if lines, err := f.c.Tail("nosuchapp", "", 10); err != nil || len(lines) != 0 {
		t.Errorf("unknown app: %d lines, %v", len(lines), err)
	}
}

func TestTailReadsBackwardsAcrossBlocks(t *testing.T) {
	f := newFixture(t, nil)
	// Enough to span several 64 kB blocks, with lines of varying length.
	const n = 5000
	batch := make([]Line, 0, n)
	for i := 0; i < n; i++ {
		batch = append(batch, Line{Service: "web", Time: t0.Add(time.Duration(i) * time.Millisecond), Text: fmt.Sprintf("line %d %s", i, strings.Repeat("x", i%97))})
	}
	if err := f.c.write("blog", batch); err != nil {
		t.Fatal(err)
	}
	// A line that is still being written is not returned.
	path := filepath.Join(f.dir, "blog", "web", "2026-11-18.log")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("2026-11-18T04:00:00.000000000Z unfinished")
	file.Close()

	for _, want := range []int{1, 2, 700, n} {
		lines, err := f.c.Tail("blog", "web", want)
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != want {
			t.Fatalf("Tail(%d) returned %d lines", want, len(lines))
		}
		for i, line := range lines {
			if line != batch[n-want+i] {
				t.Fatalf("Tail(%d): line %d = %+v, want %+v", want, i, line, batch[n-want+i])
			}
		}
	}
}

func TestReadsRejectUnsafeNames(t *testing.T) {
	f := historyFixture(t)
	ctx := context.Background()
	for _, slug := range []string{"", "..", "../blog", "blog/web", ".archive-state.json", "Blog", "a b"} {
		if _, err := f.c.Tail(slug, "", 1); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Tail(slug %q): %v, want ErrInvalidName", slug, err)
		}
		if _, err := f.c.Search(ctx, slug, Query{}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Search(slug %q): %v, want ErrInvalidName", slug, err)
		}
		if _, err := f.c.Services(slug); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Services(slug %q): %v, want ErrInvalidName", slug, err)
		}
	}
	for _, service := range []string{"..", "../web", "web/..", "/etc", ".hidden", "a b", "web\x00"} {
		if _, err := f.c.Tail("blog", service, 1); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Tail(service %q): %v, want ErrInvalidName", service, err)
		}
		if _, err := f.c.Search(ctx, "blog", Query{Service: service}); !errors.Is(err, ErrInvalidName) {
			t.Errorf("Search(service %q): %v, want ErrInvalidName", service, err)
		}
	}
}
