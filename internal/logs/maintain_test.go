package logs

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// at returns the given wall-clock time in the fixture's location.
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, jst)
}

func mustMaintain(t *testing.T, c *Collector) {
	t.Helper()
	if err := c.Maintain(context.Background()); err != nil {
		t.Fatalf("Maintain: %v", err)
	}
}

func gzipBytes(t *testing.T, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMaintainCompressesPastDaysAndExpiresOldOnes(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Set(at(20, 10, 0))
	// Retention is 14 days: the 6th is the oldest day that stays.
	f.put("blog", "web", at(5, 12, 0), "expired")
	f.put("blog", "web", at(6, 12, 0), "oldest kept")
	f.put("blog", "web", at(18, 9, 0), "two days ago")
	f.put("blog", "web", at(19, 23, 59), "yesterday")
	f.put("blog", "web", at(20, 0, 0), "today, first minute")
	f.put("blog", "web", at(20, 9, 0), "today")
	f.put("blog", AccessService, at(19, 8, 0), "access yesterday")
	f.put("gone", "web", at(1, 8, 0), "app deleted long ago")

	mustMaintain(t, f.c)

	webDir := filepath.Join(f.dir, "blog", "web")
	wantStrings(t, "web files", fileNames(t, webDir),
		[]string{"2026-11-06.log.gz", "2026-11-18.log.gz", "2026-11-19.log.gz", "2026-11-20.log"})
	wantStrings(t, "access files", fileNames(t, filepath.Join(f.dir, "blog", AccessService)), []string{"2026-11-19.log.gz"})
	wantStrings(t, "apps", fileNames(t, f.dir), []string{"blog"})
	wantStrings(t, "compressed day", texts(dayContent(t, f.dir, "blog", "web", "2026-11-19")), []string{"yesterday"})

	// The file of the current day is written to, not replaced.
	f.put("blog", "web", at(20, 10, 0), "today, later")
	wantStrings(t, "today", texts(dayContent(t, f.dir, "blog", "web", "2026-11-20")),
		[]string{"today, first minute", "today", "today, later"})

	// A second pass has nothing to do.
	mustMaintain(t, f.c)
	wantStrings(t, "web files after a second pass", fileNames(t, webDir),
		[]string{"2026-11-06.log.gz", "2026-11-18.log.gz", "2026-11-19.log.gz", "2026-11-20.log"})

	// Compressed files expire as well.
	f.clock.Set(at(21, 0, 5))
	mustMaintain(t, f.c)
	wantStrings(t, "web files a day later", fileNames(t, webDir),
		[]string{"2026-11-18.log.gz", "2026-11-19.log.gz", "2026-11-20.log.gz"})
}

func TestMaintainMergesLinesThatArriveForACompressedDay(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Set(at(19, 0, 30))
	f.put("blog", "web", at(18, 23, 58), "before the outage")
	mustMaintain(t, f.c)

	// After an outage across midnight, Docker hands over the lines of
	// yesterday; that day is compressed already.
	f.put("blog", "web", at(18, 23, 59), "filled in after the outage")
	dir := filepath.Join(f.dir, "blog", "web")
	wantStrings(t, "files with a late line", fileNames(t, dir), []string{"2026-11-18.log", "2026-11-18.log.gz"})
	both := []string{"before the outage", "filled in after the outage"}
	wantStrings(t, "day content before the merge", texts(dayContent(t, f.dir, "blog", "web", "2026-11-18")), both)
	lines, err := f.c.Tail("blog", "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "Tail before the merge", lineTexts(lines), both)

	mustMaintain(t, f.c)
	wantStrings(t, "files after the merge", fileNames(t, dir), []string{"2026-11-18.log.gz"})
	wantStrings(t, "day content after the merge", texts(dayContent(t, f.dir, "blog", "web", "2026-11-18")), both)
}

func TestNewCompletesInterruptedCompressions(t *testing.T) {
	dir := t.TempDir()
	web := filepath.Join(dir, "blog", "web")
	if err := os.MkdirAll(web, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := func(day int, text string) string {
		return string(appendEntry(nil, at(day, 12, 0), text))
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(web, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Day 10: died while writing the temporary file. The plain file counts.
	write("2026-11-10.log", []byte(entry(10, "ten")))
	write("2026-11-10.log.gz.tmp", []byte("half a gzip stre"))
	// Day 11: died after the plain file was set aside. The temporary file
	// is complete and counts, also over an older gzip file.
	write("2026-11-11.log.done", []byte(entry(11, "late")))
	write("2026-11-11.log.gz", gzipBytes(t, entry(11, "early")))
	write("2026-11-11.log.gz.tmp", gzipBytes(t, entry(11, "early")+entry(11, "late")))
	// Day 12: died just before removing the plain file that was set aside.
	write("2026-11-12.log.done", []byte(entry(12, "twelve")))
	write("2026-11-12.log.gz", gzipBytes(t, entry(12, "twelve")))

	f := newFixtureIn(t, dir, nil)
	wantStrings(t, "files after recovery", fileNames(t, web),
		[]string{"2026-11-10.log", "2026-11-11.log.gz", "2026-11-12.log.gz"})
	wantStrings(t, "all lines", texts(serviceContent(t, f.dir, "blog", "web")), []string{"ten", "early", "late", "twelve"})
}

func TestMaintainNeverLosesLinesWrittenWhileCompressing(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Set(at(19, 0, 30))
	const n = 300
	for i := 0; i < n; i++ {
		f.put("blog", "web", at(18, 12, 0).Add(time.Duration(i)*time.Millisecond), fmt.Sprintf("early %d", i))
	}
	// Late lines for yesterday keep arriving while maintenance runs.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < n; i++ {
			_ = f.c.write("blog", []Line{{Service: "web", Time: at(18, 13, 0).Add(time.Duration(i) * time.Millisecond), Text: fmt.Sprintf("late %d", i)}})
		}
	}()
	for i := 0; i < 20; i++ {
		mustMaintain(t, f.c)
	}
	<-done
	mustMaintain(t, f.c)

	wantStrings(t, "files", fileNames(t, filepath.Join(f.dir, "blog", "web")), []string{"2026-11-18.log.gz"})
	got := texts(dayContent(t, f.dir, "blog", "web", "2026-11-18"))
	if len(got) != 2*n {
		t.Fatalf("%d lines survived, want %d", len(got), 2*n)
	}
	seen := map[string]int{}
	for _, text := range got {
		seen[text]++
	}
	for i := 0; i < n; i++ {
		for _, kind := range []string{"early", "late"} {
			if c := seen[fmt.Sprintf("%s %d", kind, i)]; c != 1 {
				t.Fatalf("line %q is stored %d times", fmt.Sprintf("%s %d", kind, i), c)
			}
		}
	}
}

func TestRunMaintenanceFollowsTheClockAndStops(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Set(at(18, 23, 0))
	f.put("blog", "web", at(18, 22, 0), "evening")
	stop := start(t, f.c.RunMaintenance)

	dir := filepath.Join(f.dir, "blog", "web")
	time.Sleep(30 * time.Millisecond)
	wantStrings(t, "files before midnight", fileNames(t, dir), []string{"2026-11-18.log"})

	f.clock.Set(at(19, 0, 1))
	waitFor(t, "yesterday's file to be compressed", func() bool {
		names := fileNames(t, dir)
		return len(names) == 1 && names[0] == "2026-11-18.log.gz"
	})
	stop()
}

func TestMaintainRemovesMarkerOfAppWithoutFiles(t *testing.T) {
	f := newFixture(t, nil)
	f.clock.Set(at(20, 10, 0))
	f.put("gone", "web", at(1, 8, 0), "expired")
	marker := filepath.Join(f.dir, "gone", resumeMarkerName)
	if err := os.WriteFile(marker, []byte("2026-11-01T00:00:00Z\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustMaintain(t, f.c)
	if _, err := os.Stat(filepath.Join(f.dir, "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("directory of an app without log files still exists: %v (%s)", err, strings.Join(fileNames(t, f.dir), ", "))
	}
}
