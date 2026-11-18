package logs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/objstore"
)

// flakyStore is a bucket whose uploads can be made to fail.
type flakyStore struct {
	*objstore.Memory

	mu sync.Mutex
	// fail decides the fate of an upload: a non-nil error fails it, and
	// stored says whether the object arrived all the same (a reply that
	// got lost).
	fail func(key string) (stored bool, err error)
	puts []string
}

func newFlakyStore() *flakyStore { return &flakyStore{Memory: objstore.NewMemory()} }

func (s *flakyStore) Put(ctx context.Context, key string, body []byte, contentType string) error {
	s.mu.Lock()
	s.puts = append(s.puts, key)
	fail := s.fail
	s.mu.Unlock()
	if fail != nil {
		if stored, err := fail(key); err != nil {
			if stored {
				_ = s.Memory.Put(ctx, key, body, contentType)
			}
			return err
		}
	}
	return s.Memory.Put(ctx, key, body, contentType)
}

func (s *flakyStore) failWith(fail func(key string) (bool, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func (s *flakyStore) uploads() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.puts...)
}

func newArchiver(t *testing.T, f *fixture, store objstore.Store, mod func(*ArchiverOptions)) *Archiver {
	t.Helper()
	opts := ArchiverOptions{
		Collector:     f.c,
		Store:         store,
		PollInterval:  2 * time.Millisecond,
		RetryInterval: 10 * time.Minute,
	}
	if mod != nil {
		mod(&opts)
	}
	a, err := NewArchiver(opts)
	if err != nil {
		t.Fatalf("NewArchiver: %v", err)
	}
	return a
}

func mustArchive(t *testing.T, a *Archiver, windowEnd time.Time) {
	t.Helper()
	if err := a.ArchiveOnce(context.Background(), windowEnd); err != nil {
		t.Fatalf("ArchiveOnce(%s): %v", windowEnd.Format(time.RFC3339), err)
	}
}

// archivedTexts returns the texts of all archived lines of a service, with
// the keys in order.
func archivedTexts(t *testing.T, store objstore.Store, slug, service string) []string {
	t.Helper()
	objects, err := store.List(context.Background(), "logs/"+slug+"/"+service+"/")
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, object := range objects {
		all = append(all, texts(objectLines(t, store, object.Key))...)
	}
	return all
}

func TestArchiveGrid(t *testing.T) {
	f := newFixture(t, nil)
	every := func(hours int) *Archiver {
		return newArchiver(t, f, objstore.NewMemory(), func(o *ArchiverOptions) { o.IntervalHours = hours })
	}
	cases := []struct {
		hours      int
		now        time.Time
		last, next time.Time
	}{
		// The default: 00:10, 04:10, ... in the configured location.
		{0, at(18, 12, 0), at(18, 8, 10), at(18, 12, 10)},
		{4, at(18, 0, 10), at(18, 0, 10), at(18, 4, 10)},
		{4, at(18, 0, 9), at(17, 20, 10), at(18, 0, 10)},
		{4, at(18, 23, 59), at(18, 20, 10), at(19, 0, 10)},
		{1, at(18, 12, 9), at(18, 11, 10), at(18, 12, 10)},
		{24, at(18, 12, 0), at(18, 0, 10), at(19, 0, 10)},
		// An interval that does not divide the day starts over at 00:10.
		{5, at(18, 23, 59), at(18, 20, 10), at(19, 0, 10)},
		{5, at(19, 0, 10), at(19, 0, 10), at(19, 5, 10)},
	}
	for _, tc := range cases {
		a := every(tc.hours)
		// The grid is the same whatever zone the instant is expressed in.
		for _, now := range []time.Time{tc.now, tc.now.UTC()} {
			if got := a.LastWindowEnd(now); !got.Equal(tc.last) {
				t.Errorf("every %dh: LastWindowEnd(%s) = %s, want %s", tc.hours, now.Format(time.RFC3339), got.Format(time.RFC3339), tc.last.Format(time.RFC3339))
			}
			if got := a.NextWindowEnd(now); !got.Equal(tc.next) {
				t.Errorf("every %dh: NextWindowEnd(%s) = %s, want %s", tc.hours, now.Format(time.RFC3339), got.Format(time.RFC3339), tc.next.Format(time.RFC3339))
			}
		}
	}
	for _, hours := range []int{-1, 25} {
		if _, err := NewArchiver(ArchiverOptions{Collector: f.c, Store: objstore.NewMemory(), IntervalHours: hours}); err == nil {
			t.Errorf("an interval of %d hours was accepted", hours)
		}
	}
	if _, err := NewArchiver(ArchiverOptions{Collector: f.c}); err == nil {
		t.Error("an archiver without a store was accepted")
	}
}

func TestArchiveNextRun(t *testing.T) {
	f := newFixture(t, nil)
	a := newArchiver(t, f, objstore.NewMemory(), nil)

	f.clock.Set(at(18, 3, 0))
	if got, want := a.NextRun(), at(18, 4, 10); !got.Equal(want) {
		t.Errorf("NextRun at 03:00 = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	// On a grid point the next run is the following one.
	f.clock.Set(at(18, 20, 10))
	if got, want := a.NextRun(), at(19, 0, 10); !got.Equal(want) {
		t.Errorf("NextRun at 20:10 = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestArchiveUploadsEachWindowOnce(t *testing.T) {
	f := newFixture(t, nil)
	store := newFlakyStore()
	a := newArchiver(t, f, store, nil)

	f.put("blog", "web", at(18, 0, 5), "before the first window")
	f.put("blog", "web", at(18, 0, 30), "w1 a")
	f.put("blog", "web", at(18, 4, 9).Add(59*time.Second+999*time.Millisecond), "w1 last instant")
	f.put("blog", "web", at(18, 4, 10), "w2 first instant")
	f.put("blog", "web", at(18, 5, 0), "w2 b")
	f.put("blog", "db", at(18, 2, 0), "db w1")
	f.put("blog", AccessService, at(18, 1, 0), `{"ts":1,"uri":"/"}`)
	f.put("shop", "web", at(18, 6, 0), "shop w2")

	// Window 00:10 to 04:10.
	mustArchive(t, a, at(18, 4, 10))
	wantStrings(t, "objects after the first window", objectKeys(t, store), []string{
		"logs/blog/_access/2026/11/18/00.log.gz",
		"logs/blog/db/2026/11/18/00.log.gz",
		"logs/blog/web/2026/11/18/00.log.gz",
	})
	wantStrings(t, "web, first window", objectLines(t, store, "logs/blog/web/2026/11/18/00.log.gz"), []string{
		"2026-11-17T15:05:00.000000000Z before the first window",
		"2026-11-17T15:30:00.000000000Z w1 a",
		"2026-11-17T19:09:59.999000000Z w1 last instant",
	})
	if ct, _ := store.ContentType("logs/blog/web/2026/11/18/00.log.gz"); ct != "application/gzip" {
		t.Errorf("content type = %q", ct)
	}

	// Again for the same window, also after a line arrived late for it:
	// the object is not written a second time.
	mustArchive(t, a, at(18, 4, 10))
	f.put("blog", "web", at(18, 3, 0), "late for w1")
	mustArchive(t, a, at(18, 4, 10))
	if n := len(store.uploads()); n != 3 {
		t.Errorf("%d uploads after repeating the window, want 3: %q", n, store.uploads())
	}

	// Window 04:10 to 08:10 takes what came since, the late line included.
	mustArchive(t, a, at(18, 8, 10))
	wantStrings(t, "web, second window", texts(objectLines(t, store, "logs/blog/web/2026/11/18/04.log.gz")),
		[]string{"w2 first instant", "w2 b", "late for w1"})
	wantStrings(t, "shop, second window", texts(objectLines(t, store, "logs/shop/web/2026/11/18/04.log.gz")), []string{"shop w2"})

	// A window without lines uploads nothing.
	before := len(store.uploads())
	mustArchive(t, a, at(18, 12, 10))
	if n := len(store.uploads()); n != before {
		t.Errorf("an empty window caused %d uploads", n-before)
	}

	// The window that starts at 20:10 ends on the next day and is named
	// after its start.
	f.put("blog", "web", at(18, 22, 0), "w6 evening")
	f.put("blog", "web", at(19, 0, 5), "w6 after midnight")
	f.put("blog", "web", at(19, 0, 10), "w7")
	mustArchive(t, a, at(19, 0, 10))
	wantStrings(t, "web, window across midnight", texts(objectLines(t, store, "logs/blog/web/2026/11/18/20.log.gz")),
		[]string{"w6 evening", "w6 after midnight"})

	wantStrings(t, "everything archived for web, once", archivedTexts(t, store, "blog", "web"), []string{
		"before the first window", "w1 a", "w1 last instant",
		"w2 first instant", "w2 b", "late for w1",
		"w6 evening", "w6 after midnight",
	})
}

func TestArchiveContinuesAfterRestartAndCompression(t *testing.T) {
	dir := t.TempDir()
	store := newFlakyStore()
	f1 := newFixtureIn(t, dir, nil)
	f1.put("blog", "web", at(18, 21, 0), "evening")
	f1.put("blog", "web", at(18, 23, 0), "night")
	mustArchive(t, newArchiver(t, f1, store, nil), at(19, 0, 10))
	f1.c.closeWriters("blog")

	// koroku restarts; yesterday's file is compressed; a line arrives late
	// for the compressed day.
	f2 := newFixtureIn(t, dir, nil)
	f2.clock.Set(at(19, 1, 0))
	a2 := newArchiver(t, f2, store, nil)
	mustArchive(t, a2, at(19, 0, 10))
	if n := len(store.uploads()); n != 1 {
		t.Fatalf("%d uploads after a restart in the same window, want 1", n)
	}
	mustMaintain(t, f2.c)
	wantStrings(t, "files", fileNames(t, filepath.Join(dir, "blog", "web")), []string{"2026-11-18.log.gz"})
	mustArchive(t, a2, at(19, 4, 10))
	if n := len(store.uploads()); n != 1 {
		t.Fatalf("compressing a day caused an upload: %q", store.uploads())
	}

	f2.put("blog", "web", at(18, 23, 30), "late for yesterday")
	f2.put("blog", "web", at(19, 0, 30), "today")
	mustArchive(t, a2, at(19, 4, 10))
	wantStrings(t, "second object", texts(objectLines(t, store, "logs/blog/web/2026/11/19/00.log.gz")),
		[]string{"late for yesterday", "today"})

	// Merging the late line into the compressed day changes nothing.
	mustMaintain(t, f2.c)
	f3 := newFixtureIn(t, dir, nil)
	mustArchive(t, newArchiver(t, f3, store, nil), at(19, 8, 10))
	wantStrings(t, "everything archived, once", archivedTexts(t, store, "blog", "web"),
		[]string{"evening", "night", "late for yesterday", "today"})
	if n := len(store.uploads()); n != 2 {
		t.Errorf("%d uploads in total, want 2: %q", n, store.uploads())
	}
}

func TestArchiveRetriesFailedUpload(t *testing.T) {
	for _, stored := range []bool{false, true} {
		name := "upload never arrived"
		if stored {
			name = "upload arrived but the reply was lost"
		}
		t.Run(name, func(t *testing.T) {
			const secret = "s3cr3t-in-a-log-line"
			dir := t.TempDir()
			f := newFixtureIn(t, dir, nil)
			store := newFlakyStore()
			a := newArchiver(t, f, store, nil)
			f.put("blog", "web", at(18, 1, 0), "w1 "+secret)
			f.put("blog", "db", at(18, 1, 0), "db w1")

			store.failWith(func(key string) (bool, error) {
				if strings.Contains(key, "/web/") {
					return stored, errors.New("bucket unreachable")
				}
				return false, nil
			})
			err := a.ArchiveOnce(context.Background(), at(18, 4, 10))
			if err == nil {
				t.Fatal("ArchiveOnce succeeded although the upload failed")
			}
			if !strings.Contains(err.Error(), "logs/blog/web/2026/11/18/00.log.gz") || strings.Contains(err.Error(), secret) {
				t.Errorf("error = %v; want the key and no line content", err)
			}
			// The failure of one service does not hold up the others.
			wantStrings(t, "db", archivedTexts(t, store, "blog", "db"), []string{"db w1"})

			// The next run, by a new process, first repeats the upload
			// that failed, under its own name, and then does its own.
			store.failWith(nil)
			f.put("blog", "web", at(18, 5, 0), "w2")
			f.c.closeWriters("blog")
			f2 := newFixtureIn(t, dir, nil)
			a2 := newArchiver(t, f2, store, nil)
			mustArchive(t, a2, at(18, 8, 10))
			wantStrings(t, "missed window", texts(objectLines(t, store, "logs/blog/web/2026/11/18/00.log.gz")), []string{"w1 " + secret})
			wantStrings(t, "next window", texts(objectLines(t, store, "logs/blog/web/2026/11/18/04.log.gz")), []string{"w2"})
			wantStrings(t, "everything archived, once", archivedTexts(t, store, "blog", "web"), []string{"w1 " + secret, "w2"})
			if strings.Contains(f.logs.String()+f2.logs.String(), secret) {
				t.Error("line content leaked into koroku's log")
			}
		})
	}
}

func TestArchiveCoversSeveralMissedWindows(t *testing.T) {
	f := newFixture(t, nil)
	store := newFlakyStore()
	a := newArchiver(t, f, store, nil)
	// The bucket is down for two windows.
	store.failWith(func(string) (bool, error) { return false, errors.New("bucket unreachable") })
	f.put("blog", "web", at(18, 1, 0), "w1")
	if err := a.ArchiveOnce(context.Background(), at(18, 4, 10)); err == nil {
		t.Fatal("want an error")
	}
	f.put("blog", "web", at(18, 5, 0), "w2")
	if err := a.ArchiveOnce(context.Background(), at(18, 8, 10)); err == nil {
		t.Fatal("want an error")
	}
	f.put("blog", "web", at(18, 9, 0), "w3")
	store.failWith(nil)
	mustArchive(t, a, at(18, 12, 10))
	// The upload that failed first is repeated as it was; the object of
	// the current window holds the rest.
	wantStrings(t, "objects", objectKeys(t, store), []string{
		"logs/blog/web/2026/11/18/00.log.gz",
		"logs/blog/web/2026/11/18/08.log.gz",
	})
	wantStrings(t, "first", texts(objectLines(t, store, "logs/blog/web/2026/11/18/00.log.gz")), []string{"w1"})
	wantStrings(t, "catch-up", texts(objectLines(t, store, "logs/blog/web/2026/11/18/08.log.gz")), []string{"w2", "w3"})
}

func TestArchiveBoundsObjectSize(t *testing.T) {
	f := newFixture(t, nil)
	store := newFlakyStore()
	a := newArchiver(t, f, store, func(o *ArchiverOptions) { o.MaxObjectBytes = 100 })
	for i := 0; i < 6; i++ {
		f.put("blog", "web", at(18, 1, i), fmt.Sprintf("line %d", i)) // 38 bytes each
	}
	mustArchive(t, a, at(18, 4, 10))
	wantStrings(t, "first object", texts(objectLines(t, store, "logs/blog/web/2026/11/18/00.log.gz")), []string{"line 0", "line 1"})
	mustArchive(t, a, at(18, 8, 10))
	mustArchive(t, a, at(18, 12, 10))
	wantStrings(t, "after three windows", archivedTexts(t, store, "blog", "web"),
		[]string{"line 0", "line 1", "line 2", "line 3", "line 4", "line 5"})
}

func TestArchiveStartsOverWithUnreadableState(t *testing.T) {
	f := newFixture(t, nil)
	store := newFlakyStore()
	f.put("blog", "web", at(18, 1, 0), "w1")
	if err := os.WriteFile(filepath.Join(f.dir, archiveStateName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustArchive(t, newArchiver(t, f, store, nil), at(18, 4, 10))
	wantStrings(t, "archived", archivedTexts(t, store, "blog", "web"), []string{"w1"})
	if !strings.Contains(f.logs.String(), "archive state is unreadable") {
		t.Error("no warning about the unreadable state")
	}
}

func TestArchiveRunFollowsTheGrid(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Set(at(18, 3, 0))
	store := newFlakyStore()
	appsDir := t.TempDir()
	a := newArchiver(t, f, store, func(o *ArchiverOptions) { o.AppsDir = appsDir })
	deployLog := filepath.Join(appsDir, "blog", "deploys", "1.log")
	if err := os.MkdirAll(filepath.Dir(deployLog), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(deployLog, []byte("==> deploy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(deployLog, at(18, 1, 0), at(18, 1, 0)); err != nil {
		t.Fatal(err)
	}
	f.put("blog", "web", at(18, 0, 5), "left over from the last window")
	f.put("blog", "web", at(18, 2, 0), "w1")

	has := func(key string) func() bool {
		return func() bool {
			for _, k := range objectKeys(t, store) {
				if k == key {
					return true
				}
			}
			return false
		}
	}
	stop := start(t, a.Run)

	// At start it catches up with the window that ended at 00:10.
	waitFor(t, "the catch-up at start", has("logs/blog/web/2026/11/17/20.log.gz"))
	waitFor(t, "the deploy log", has("logs/blog/_deploy/1.log.gz"))
	time.Sleep(20 * time.Millisecond)
	if has("logs/blog/web/2026/11/18/00.log.gz")() {
		t.Fatal("the running window was archived before it ended")
	}

	// The upload at 04:10 fails; it is repeated ten minutes later.
	store.failWith(func(string) (bool, error) { return false, errors.New("bucket unreachable") })
	f.clock.Set(at(18, 4, 10))
	waitFor(t, "the failed run to be reported", func() bool {
		return strings.Contains(f.logs.String(), "log archive run failed")
	})
	store.failWith(nil)
	f.clock.Set(at(18, 4, 19))
	time.Sleep(20 * time.Millisecond)
	if has("logs/blog/web/2026/11/18/00.log.gz")() {
		t.Fatal("retried before the retry interval")
	}
	f.clock.Set(at(18, 4, 21))
	waitFor(t, "the retry", has("logs/blog/web/2026/11/18/00.log.gz"))
	wantStrings(t, "window object", texts(objectLines(t, store, "logs/blog/web/2026/11/18/00.log.gz")), []string{"w1"})

	// A clock that jumps over several grid points archives once, for the
	// latest window.
	f.put("blog", "web", at(18, 5, 0), "w2")
	f.put("blog", "web", at(18, 13, 0), "w4")
	f.clock.Set(at(18, 16, 30))
	waitFor(t, "the run after the jump", has("logs/blog/web/2026/11/18/12.log.gz"))
	stop()
	wantStrings(t, "everything archived, once", archivedTexts(t, store, "blog", "web"),
		[]string{"left over from the last window", "w1", "w2", "w4"})
}

func TestArchiveDeployLogsOnce(t *testing.T) {
	f := newFixture(t, nil)
	store := newFlakyStore()
	appsDir := t.TempDir()
	a := newArchiver(t, f, store, func(o *ArchiverOptions) { o.AppsDir = appsDir })
	now := f.clock.Now()
	writeLog := func(slug, name, content string, age time.Duration) {
		t.Helper()
		path := filepath.Join(appsDir, slug, "deploys", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
	writeLog("blog", "1.log", "==> first deploy\ndone\n", time.Hour)
	writeLog("blog", "2.log", "==> still running\n", time.Minute)
	writeLog("blog", "3.log", "==> ancient\n", 20*24*time.Hour)
	writeLog("blog", "notes.log", "not a deploy log\n", time.Hour)
	writeLog("blog", "04.log", "not a canonical id\n", time.Hour)
	writeLog("blog", "5.txt", "wrong extension\n", time.Hour)
	writeLog("shop", "7.log", "==> shop deploy\n", time.Hour)
	if err := os.MkdirAll(filepath.Join(appsDir, "never-deployed"), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := a.ArchiveDeployLogs(ctx); err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "objects", objectKeys(t, store), []string{"logs/blog/_deploy/1.log.gz", "logs/shop/_deploy/7.log.gz"})
	wantStrings(t, "content", objectLines(t, store, "logs/blog/_deploy/1.log.gz"), []string{"==> first deploy", "done"})

	// Each log is uploaded once.
	if err := a.ArchiveDeployLogs(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(store.uploads()); n != 2 {
		t.Errorf("%d uploads after a second pass, want 2", n)
	}

	// The running deployment finishes; a failed upload is tried again.
	f.clock.Advance(31 * time.Minute)
	store.failWith(func(string) (bool, error) { return false, errors.New("bucket unreachable") })
	if err := a.ArchiveDeployLogs(ctx); err == nil || !strings.Contains(err.Error(), "logs/blog/_deploy/2.log.gz") {
		t.Errorf("error = %v, want the failed key", err)
	}
	store.failWith(nil)
	if err := a.ArchiveDeployLogs(ctx); err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "objects in the end", objectKeys(t, store),
		[]string{"logs/blog/_deploy/1.log.gz", "logs/blog/_deploy/2.log.gz", "logs/shop/_deploy/7.log.gz"})

	// Without AppsDir there is nothing to do.
	if err := newArchiver(t, f, store, nil).ArchiveDeployLogs(ctx); err != nil {
		t.Errorf("ArchiveDeployLogs without AppsDir: %v", err)
	}
}

func TestListArchives(t *testing.T) {
	f := newFixture(t, nil)
	store := objstore.NewMemory()
	store.Clock = f.clock
	ctx := context.Background()
	for i, key := range []string{
		"logs/blog/web/2026/11/18/04.log.gz",
		"logs/blog/web/2026/11/18/00.log.gz",
		"logs/blog/db/2026/11/18/04.log.gz",
		"logs/blog/_access/2026/11/17/20.log.gz",
		"logs/blog/_deploy/12.log.gz",
		// Not archives, or not of this app.
		"logs/blog/web/2026/11/18/notes.txt",
		"logs/blog/web/2026/13/40/99.log.gz",
		"logs/blog/web/extra/2026/11/18/04.log.gz",
		"logs/blog2/web/2026/11/18/04.log.gz",
		"backups/blog/web/2026/11/18/04.log.gz",
	} {
		if err := store.Put(ctx, key, make([]byte, 10+i), "application/gzip"); err != nil {
			t.Fatal(err)
		}
	}
	a := newArchiver(t, f, store, nil)

	got, err := a.List(ctx, "blog", "")
	if err != nil {
		t.Fatal(err)
	}
	// The deploy log was uploaded at 12:00 in the fixture's location.
	want := []Archive{
		{Key: "logs/blog/_deploy/12.log.gz", Service: DeployService, Hour: at(18, 12, 0), Size: 14, DeployID: 12},
		{Key: "logs/blog/db/2026/11/18/04.log.gz", Service: "db", Hour: at(18, 4, 0), Size: 12},
		{Key: "logs/blog/web/2026/11/18/04.log.gz", Service: "web", Hour: at(18, 4, 0), Size: 10},
		{Key: "logs/blog/web/2026/11/18/00.log.gz", Service: "web", Hour: at(18, 0, 0), Size: 11},
		{Key: "logs/blog/_access/2026/11/17/20.log.gz", Service: AccessService, Hour: at(17, 20, 0), Size: 13},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d archives, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g := got[i]
		if g.Key != want[i].Key || g.Service != want[i].Service || g.Size != want[i].Size || g.DeployID != want[i].DeployID || !g.Hour.Equal(want[i].Hour) {
			t.Errorf("archive %d = %+v, want %+v", i, g, want[i])
		}
		if _, offset := g.Hour.Zone(); offset != 9*60*60 {
			t.Errorf("archive %d: hour %s is not in the configured location", i, g.Hour)
		}
		if err := ValidateArchiveKey("blog", g.Key); err != nil {
			t.Errorf("listed key %q does not validate: %v", g.Key, err)
		}
	}

	web, err := ListArchives(ctx, store, jst, "blog", "web")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, archive := range web {
		keys = append(keys, archive.Key)
	}
	wantStrings(t, "one service", keys, []string{"logs/blog/web/2026/11/18/04.log.gz", "logs/blog/web/2026/11/18/00.log.gz"})

	if list, err := ListArchives(ctx, store, nil, "nosuch", ""); err != nil || len(list) != 0 {
		t.Errorf("unknown app: %+v, %v", list, err)
	}
	if _, err := ListArchives(ctx, store, jst, "../blog", ""); !errors.Is(err, ErrInvalidName) {
		t.Errorf("unsafe slug: %v", err)
	}
	if _, err := ListArchives(ctx, store, jst, "blog", "../web"); !errors.Is(err, ErrInvalidName) {
		t.Errorf("unsafe service: %v", err)
	}
}

func TestValidateArchiveKey(t *testing.T) {
	valid := []string{
		"logs/blog/web/2026/11/18/04.log.gz",
		"logs/blog/job-worker-2/2026/01/01/00.log.gz",
		"logs/blog/_access/2026/11/18/20.log.gz",
		"logs/blog/_deploy/12.log.gz",
	}
	for _, key := range valid {
		if err := ValidateArchiveKey("blog", key); err != nil {
			t.Errorf("ValidateArchiveKey(blog, %q) = %v", key, err)
		}
	}
	invalid := []string{
		"",
		"logs/blog/",
		"logs/blog/web",
		"logs/blog/web/2026/11/18/04.log",
		"logs/blog/web/2026/11/18/4.log.gz",
		"logs/blog/web/2026/11/18/24.log.gz",
		"logs/blog/web/2026/02/30/04.log.gz",
		"logs/blog/web/2026/11/18/04.log.gz/extra",
		"logs/blog/web/2026/11/18/../../../../../shop/web/2026/11/18/04.log.gz",
		"logs/blog/../shop/web/2026/11/18/04.log.gz",
		"logs/blog/./web/2026/11/18/04.log.gz",
		"logs/blog//web/2026/11/18/04.log.gz",
		"/logs/blog/web/2026/11/18/04.log.gz",
		"logs/shop/web/2026/11/18/04.log.gz",
		"logs/blog2/web/2026/11/18/04.log.gz",
		"logs/blo/web/2026/11/18/04.log.gz",
		"backups/blog/web/2026/11/18/04.log.gz",
		"koroku.db",
		"logs/blog/_deploy/0.log.gz",
		"logs/blog/_deploy/012.log.gz",
		"logs/blog/_deploy/../web/2026/11/18/04.log.gz",
		"logs/blog/_deploy/2026/11/18/04.log.gz",
		"logs/blog/web/2026/11/18/04.log.gz\n",
		"logs/blog/we b/2026/11/18/04.log.gz",
	}
	for _, key := range invalid {
		if err := ValidateArchiveKey("blog", key); !errors.Is(err, ErrInvalidArchiveKey) {
			t.Errorf("ValidateArchiveKey(blog, %q) = %v, want ErrInvalidArchiveKey", key, err)
		}
	}
	if err := ValidateArchiveKey("../blog", "logs/../blog/web/2026/11/18/04.log.gz"); err == nil {
		t.Error("an unsafe slug was accepted")
	}
	// Every key the archiver produces passes.
	generated := []string{windowKey("blog", "web", at(18, 20, 10), jst), deployKey("blog", 7)}
	sort.Strings(generated)
	wantStrings(t, "generated keys", generated, []string{"logs/blog/_deploy/7.log.gz", "logs/blog/web/2026/11/18/20.log.gz"})
	for _, key := range generated {
		if err := ValidateArchiveKey("blog", key); err != nil {
			t.Errorf("generated key %q does not validate: %v", key, err)
		}
	}
}
