package logs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/objstore"
	"github.com/tricknotes/koroku/internal/runner"
)

// entrySize is what an entry with this text takes in a day file.
func entrySize(text string) int64 { return int64(len(appendEntry(nil, t0, text))) }

// roomFor returns a daily limit that holds exactly n entries like sample.
func roomFor(n int, sample string) int64 { return int64(n) * entrySize(sample) }

func drain(ch <-chan Line) []Line {
	var got []Line
	for {
		select {
		case line := <-ch:
			got = append(got, line)
		default:
			return got
		}
	}
}

func TestDailyLimitDropsLinesAndMarksTheGap(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxDayBytes = roomFor(3, "line 0") })
	live, cancel := f.c.Subscribe("blog")
	defer cancel()

	for i := 0; i < 6; i++ {
		f.put("blog", "web", at(18, 10, i), fmt.Sprintf("line %d", i))
	}
	f.put("blog", "db", at(18, 10, 30), "another service")

	marker := f.c.fullMarker
	if !strings.HasPrefix(marker, MarkerPrefix) || !strings.Contains(marker, "daily_limit_mb") {
		t.Fatalf("marker = %q", marker)
	}
	stored := dayContent(t, f.dir, "blog", "web", "2026-11-18")
	wantStrings(t, "web", texts(stored), []string{"line 0", "line 1", "line 2", marker})
	// The marker stands where the first dropped line would be.
	if want := string(appendEntry(nil, at(18, 10, 3), marker)); len(stored) == 4 && stored[3]+"\n" != want {
		t.Errorf("marker entry = %q, want %q", stored[3], want)
	}
	wantStrings(t, "db", texts(serviceContent(t, f.dir, "blog", "db")), []string{"another service"})

	// Readers see the marker in place of the dropped lines.
	wantStrings(t, "live", lineTexts(drain(live)), []string{"line 0", "line 1", "line 2", marker, "another service"})
	tail, err := f.c.Tail("blog", "web", 10)
	if err != nil {
		t.Fatal(err)
	}
	wantStrings(t, "tail", lineTexts(tail), []string{"line 0", "line 1", "line 2", marker})

	// The rest of the day is dropped without another marker.
	f.clock.Advance(dropReportEvery)
	f.put("blog", "web", at(18, 23, 59), "line 6")
	wantStrings(t, "web later that day", texts(dayContent(t, f.dir, "blog", "web", "2026-11-18")), []string{"line 0", "line 1", "line 2", marker})
	wantStrings(t, "live later that day", lineTexts(drain(live)), nil)

	// The next day is written again.
	f.put("blog", "web", at(19, 0, 0), "line 7")
	wantStrings(t, "web the next day", texts(dayContent(t, f.dir, "blog", "web", "2026-11-19")), []string{"line 7"})
	wantStrings(t, "live the next day", lineTexts(drain(live)), []string{"line 7"})

	// koroku's own log counts the dropped lines and never shows them.
	own := f.logs.String()
	for _, want := range []string{
		`msg="dropping log lines" app=blog service=web reason=daily_limit day=2026-11-18`,
		`msg="still dropping log lines" app=blog service=web reason=daily_limit day=2026-11-18 dropped_lines=4`,
		`msg="log lines were dropped" app=blog service=web reason=daily_limit day=2026-11-18 dropped_lines=4`,
	} {
		if !strings.Contains(own, want) {
			t.Errorf("koroku's log lacks %q:\n%s", want, own)
		}
	}
	if strings.Contains(own, "line ") || strings.Contains(own, "service=db") {
		t.Errorf("koroku's log shows line content or an unaffected service:\n%s", own)
	}
}

// A line that does not fit ends the day even when a shorter one would still
// fit: after the marker nothing is added, so the marker stays the last entry.
func TestDailyLimitIsNotRefilledByShortLines(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxDayBytes = roomFor(2, "line 0") + 5 })
	f.put("blog", "web", at(18, 10, 0), "line 0")
	f.put("blog", "web", at(18, 10, 1), strings.Repeat("x", 200))
	f.put("blog", "web", at(18, 10, 2), "ok")
	wantStrings(t, "web", texts(serviceContent(t, f.dir, "blog", "web")), []string{"line 0", f.c.fullMarker})
}

func TestDailyLimitAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	limit := roomFor(3, "line 0")
	withLimit := func(n int64) *fixture {
		return newFixtureIn(t, dir, func(o *Options) { o.MaxDayBytes = n })
	}
	stored := func() []string { return texts(dayContent(t, dir, "blog", "web", "2026-11-18")) }

	f1 := withLimit(limit)
	for i := 0; i < 5; i++ {
		f1.put("blog", "web", at(18, 10, i), fmt.Sprintf("line %d", i))
	}
	f1.c.closeWriters("blog")
	full := []string{"line 0", "line 1", "line 2", f1.c.fullMarker}
	wantStrings(t, "first process", stored(), full)

	// The same limit: the day stays closed, and is not marked again.
	f2 := withLimit(limit)
	f2.put("blog", "web", at(18, 11, 0), "line 5")
	f2.c.closeWriters("blog")
	wantStrings(t, "second process", stored(), full)

	// A higher limit takes lines again, up to that limit.
	f3 := withLimit(limit + roomFor(1, f1.c.fullMarker) + roomFor(1, "line 6"))
	f3.put("blog", "web", at(18, 12, 0), "line 6")
	f3.put("blog", "web", at(18, 12, 1), "line 7")
	f3.c.closeWriters("blog")
	raised := append(append([]string{}, full...), "line 6", f3.c.fullMarker)
	wantStrings(t, "higher limit", stored(), raised)

	// A lower limit than what the day holds: marked once.
	f4 := withLimit(limit)
	f4.put("blog", "web", at(18, 13, 0), "line 8")
	f4.put("blog", "web", at(18, 13, 1), "line 9")
	wantStrings(t, "lower limit", stored(), append(raised, f4.c.fullMarker))
}

// Lines that arrive for a day that is compressed already count on top of
// what the compressed file holds.
func TestDailyLimitCountsTheCompressedPart(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxDayBytes = roomFor(3, "line 0") })
	f.put("blog", "web", at(18, 10, 0), "line 0")
	f.put("blog", "web", at(18, 10, 1), "line 1")
	f.clock.Set(at(19, 9, 0))
	mustMaintain(t, f.c)
	wantStrings(t, "files", fileNames(t, filepath.Join(f.dir, "blog", "web")), []string{"2026-11-18.log.gz"})

	f.put("blog", "web", at(18, 10, 2), "line 2")
	f.put("blog", "web", at(18, 10, 3), "line 3")
	mustMaintain(t, f.c)
	wantStrings(t, "files", fileNames(t, filepath.Join(f.dir, "blog", "web")), []string{"2026-11-18.log.gz"})
	wantStrings(t, "web", texts(dayContent(t, f.dir, "blog", "web", "2026-11-18")), []string{"line 0", "line 1", "line 2", f.c.fullMarker})
}

func TestCollectDropsBeyondDailyLimitAcrossRestarts(t *testing.T) {
	const n, room = 50, 20
	var burst []held
	for i := 0; i < n; i++ {
		burst = append(burst, held{"web-1", t0.Add(time.Duration(i) * time.Second), fmt.Sprintf("line %03d", i)})
	}
	dir := t.TempDir()
	limit := func(o *Options) { o.MaxDayBytes = roomFor(room, "line 000") }
	day := func(date string) []string { return texts(dayContent(t, dir, "blog", "web", date)) }

	f1 := newFixtureIn(t, dir, func(o *Options) {
		limit(o)
		o.Runner = &runner.Fake{Handler: daemon(burst)}
		o.Apps = oneApp(blog)
	})
	stop := f1.startRun()
	waitFor(t, "the marker", func() bool { return len(day("2026-11-18")) == room+1 })
	stop()
	// The count is logged when the collection of the app ends.
	if want := fmt.Sprintf(`msg="log lines were dropped" app=blog service=web reason=daily_limit day=2026-11-18 dropped_lines=%d`, n-room); !strings.Contains(f1.logs.String(), want) {
		t.Errorf("koroku's log lacks %q:\n%s", want, f1.logs.String())
	}

	// Docker still holds everything, and has more: the rest of that day
	// stays dropped, the next day is collected.
	more := append(append([]held{}, burst...),
		held{"web-1", t0.Add(time.Hour), "still the same day"},
		held{"web-1", t0.Add(13 * time.Hour), "the next day"})
	f2 := newFixtureIn(t, dir, func(o *Options) {
		limit(o)
		o.Runner = &runner.Fake{Handler: daemon(more)}
		o.Apps = oneApp(blog)
	})
	f2.startRun()
	waitFor(t, "the next day", func() bool { return len(day("2026-11-19")) == 1 })

	want := make([]string, 0, room+1)
	for i := 0; i < room; i++ {
		want = append(want, fmt.Sprintf("line %03d", i))
	}
	wantStrings(t, "the full day", day("2026-11-18"), append(want, f2.c.fullMarker))
	wantStrings(t, "the next day", day("2026-11-19"), []string{"the next day"})
}

// The idle tracker counts requests from the access log (ARCHITECTURE 8.4):
// it must hear of every entry, kept or not.
func TestAccessTailReportsDroppedEntries(t *testing.T) {
	sample := accessEntry(1795000000.25, "/0")
	reported := 0
	f := newFixture(t, func(o *Options) {
		o.MaxDayBytes = roomFor(2, sample)
		o.OnAccess = func(string, []byte) { reported++ }
	})
	path := filepath.Join(t.TempDir(), "blog.log")
	a := newAccessTail(f, path)
	mustPoll(t, a) // no file yet; what comes from now on is new

	for i := 0; i < 5; i++ {
		appendFile(t, path, accessEntry(1795000000.25+float64(i), fmt.Sprintf("/%d", i))+"\n")
	}
	mustPoll(t, a)
	got := serviceContent(t, f.dir, "blog", AccessService)
	if len(got) != 3 || !strings.HasSuffix(got[2], f.c.fullMarker) {
		t.Fatalf("stored = %q", got)
	}
	wantStrings(t, "kept entries", uris(got[:2]), []string{"/0", "/1"})
	if reported != 5 {
		t.Errorf("%d entries were reported, want 5", reported)
	}

	// After a restart the entries of the file are read again: nothing is
	// added, and they are not reported as new requests.
	a2 := newAccessTail(f, path)
	mustPoll(t, a2)
	wantStrings(t, "after a restart", serviceContent(t, f.dir, "blog", AccessService), got)
	if reported != 5 {
		t.Errorf("%d entries were reported after the restart, want 5", reported)
	}
}

// The archive follows the day files byte by byte. Dropped lines never reach
// a file, so the offsets stay what the files say and the marker is archived
// like any entry.
func TestArchiveAfterDroppedLines(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxDayBytes = roomFor(3, "line 0") })
	store := objstore.NewMemory()
	a := newArchiver(t, f, store, nil)
	archived := func() []string { return archivedTexts(t, store, "blog", "web") }
	offset := func(a *Archiver, date string) int64 {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.state.Services["blog/web"].Offsets[date]
	}
	fileSize := func(date string) int64 {
		info, err := os.Stat(filepath.Join(f.dir, "blog", "web", date+plainExt))
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}

	for i := 0; i < 2; i++ {
		f.put("blog", "web", at(18, 1, i), fmt.Sprintf("line %d", i))
	}
	mustArchive(t, a, at(18, 4, 10))
	wantStrings(t, "first window", archived(), []string{"line 0", "line 1"})

	// The day fills up in the second window.
	for i := 2; i < 6; i++ {
		f.put("blog", "web", at(18, 5, i), fmt.Sprintf("line %d", i))
	}
	mustArchive(t, a, at(18, 8, 10))
	full := []string{"line 0", "line 1", "line 2", f.c.fullMarker}
	wantStrings(t, "second window", archived(), full)
	if got, want := offset(a, "2026-11-18"), fileSize("2026-11-18"); got != want {
		t.Errorf("archived offset = %d, file size = %d", got, want)
	}

	// Nothing but dropped lines in the third window: no object, and the
	// offset does not move. A new process agrees.
	f.put("blog", "web", at(18, 9, 0), "line 6")
	a = newArchiver(t, f, store, nil)
	mustArchive(t, a, at(18, 12, 10))
	wantStrings(t, "objects", objectKeys(t, store), []string{
		"logs/blog/web/2026/11/18/00.log.gz",
		"logs/blog/web/2026/11/18/04.log.gz",
	})

	// The next day is archived from its start.
	f.put("blog", "web", at(19, 1, 0), "line 7")
	mustArchive(t, a, at(19, 4, 10))
	wantStrings(t, "next day", archived(), append(full, "line 7"))
	wantStrings(t, "stored", texts(serviceContent(t, f.dir, "blog", "web")), append(full, "line 7"))
	if got, want := offset(a, "2026-11-19"), fileSize("2026-11-19"); got != want {
		t.Errorf("archived offset of the next day = %d, file size = %d", got, want)
	}
}

func TestMaxDayBytesIsValidated(t *testing.T) {
	for _, n := range []int64{-1, MaxDayBytesLimit + 1} {
		if _, err := New(Options{Dir: t.TempDir(), MaxDayBytes: n}); err == nil {
			t.Errorf("MaxDayBytes %d was accepted", n)
		}
	}
	c, err := New(Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if c.maxDayBytes != 100<<20 || !strings.Contains(c.fullMarker, "100 MiB") {
		t.Errorf("default limit = %d, marker = %q", c.maxDayBytes, c.fullMarker)
	}
}

// fakeDisk answers Options.DiskUsedPercent.
type fakeDisk struct {
	mu    sync.Mutex
	used  float64
	err   error
	calls int
}

func (d *fakeDisk) percent() (float64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	return d.used, d.err
}

func (d *fakeDisk) set(used float64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.used, d.err = used, err
}

func (d *fakeDisk) asked() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func watchingDisk(disk *fakeDisk) func(*Options) {
	return func(o *Options) {
		o.DiskUsedPercent = disk.percent
		o.DiskStopPercent = 90
		o.DiskCheckInterval = 10 * time.Second
	}
}

func repeated(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestDiskUsageStopsAndResumesWriting(t *testing.T) {
	disk := &fakeDisk{used: 50}
	f := newFixture(t, watchingDisk(disk))
	live, cancel := f.c.Subscribe("blog")
	defer cancel()
	web := func(date string) []string { return texts(dayContent(t, f.dir, "blog", "web", date)) }
	marker := f.c.diskMarker
	if !strings.HasPrefix(marker, MarkerPrefix) || !strings.Contains(marker, "90%") {
		t.Fatalf("marker = %q", marker)
	}

	f.put("blog", "web", at(18, 10, 0), "line 0")
	// The disk is asked once per interval, not for every line.
	disk.set(90, nil)
	for i := 0; i < 50; i++ {
		f.put("blog", "web", at(18, 10, 1), "line 1")
	}
	if got := disk.asked(); got != 1 {
		t.Fatalf("the disk was asked %d times within one interval", got)
	}
	kept := append([]string{"line 0"}, repeated("line 1", 50)...)
	wantStrings(t, "before the next look", web("2026-11-18"), kept)
	drain(live)

	// At the threshold: every service stops, each with one marker.
	f.clock.Advance(10 * time.Second)
	f.put("blog", "web", at(18, 10, 2), "line 2")
	f.put("blog", "web", at(18, 10, 3), "line 3")
	f.put("blog", "db", at(18, 10, 4), "db 0")
	kept = append(kept, marker)
	stored := dayContent(t, f.dir, "blog", "web", "2026-11-18")
	wantStrings(t, "disk full", texts(stored), kept)
	if want := string(appendEntry(nil, at(18, 10, 2), marker)); stored[len(stored)-1]+"\n" != want {
		t.Errorf("marker entry = %q, want %q", stored[len(stored)-1], want)
	}
	wantStrings(t, "db", texts(serviceContent(t, f.dir, "blog", "db")), []string{marker})
	wantStrings(t, "live", lineTexts(drain(live)), []string{marker, marker})

	// Just below it again: writing resumes at the next look.
	disk.set(89.9, nil)
	f.put("blog", "web", at(18, 10, 5), "line 5")
	wantStrings(t, "before the next look", web("2026-11-18"), kept)
	f.clock.Advance(10 * time.Second)
	f.put("blog", "web", at(18, 10, 6), "line 6")
	kept = append(kept, "line 6")
	wantStrings(t, "room again", web("2026-11-18"), kept)

	// A second time gets a marker of its own, and so does a new day.
	disk.set(99, nil)
	f.clock.Advance(10 * time.Second)
	f.put("blog", "web", at(18, 10, 7), "line 7")
	f.put("blog", "web", at(19, 0, 0), "line 8")
	wantStrings(t, "full again", web("2026-11-18"), append(kept, marker))
	wantStrings(t, "the next day, still full", web("2026-11-19"), []string{marker})
	disk.set(10, nil)
	f.clock.Advance(10 * time.Second)
	f.put("blog", "web", at(19, 0, 1), "line 9")
	wantStrings(t, "the next day, room again", web("2026-11-19"), []string{marker, "line 9"})

	own := f.logs.String()
	for _, want := range []string{
		`msg="the disk is almost full; log lines are dropped until space is freed" used_percent=90 stop_percent=90`,
		`msg="dropping log lines" app=blog service=web reason=disk_usage day=2026-11-18`,
		`msg="dropping log lines" app=blog service=db reason=disk_usage day=2026-11-18`,
		`msg="log lines were dropped" app=blog service=web reason=disk_usage day=2026-11-18 dropped_lines=3`,
		`msg="the disk has room again; log lines are written" used_percent=89 stop_percent=90`,
		`msg="log lines were dropped" app=blog service=web reason=disk_usage day=2026-11-18 dropped_lines=1`,
		`msg="log lines were dropped" app=blog service=web reason=disk_usage day=2026-11-19 dropped_lines=1`,
	} {
		if !strings.Contains(own, want) {
			t.Errorf("koroku's log lacks %q:\n%s", want, own)
		}
	}
	if strings.Contains(own, "line ") || strings.Contains(own, "db 0") {
		t.Errorf("koroku's log shows line content:\n%s", own)
	}
}

func TestDiskUsageStopAcrossRestartsAndWithAFullDay(t *testing.T) {
	dir := t.TempDir()
	disk := &fakeDisk{used: 95}
	open := func() *fixture {
		return newFixtureIn(t, dir, func(o *Options) {
			watchingDisk(disk)(o)
			o.MaxDayBytes = 2*entrySize("line 0") + 400
		})
	}
	stored := func() []string { return texts(dayContent(t, dir, "blog", "web", "2026-11-18")) }

	f1 := open()
	f1.put("blog", "web", at(18, 10, 0), "line 0")
	f1.c.closeWriters("blog")
	wantStrings(t, "first process", stored(), []string{f1.c.diskMarker})

	// A new process in the same state does not mark the gap again.
	f2 := open()
	f2.put("blog", "web", at(18, 10, 1), "line 1")
	wantStrings(t, "second process", stored(), []string{f2.c.diskMarker})

	// With room again the day fills up to its own limit. From there on
	// the day is closed whatever the disk says, and marked once.
	disk.set(20, nil)
	f2.clock.Advance(10 * time.Second)
	f2.put("blog", "web", at(18, 10, 2), "line 2")
	f2.put("blog", "web", at(18, 10, 3), "line 3")
	f2.put("blog", "web", at(18, 10, 4), strings.Repeat("x", 400))
	want := []string{f2.c.diskMarker, "line 2", "line 3", f2.c.fullMarker}
	wantStrings(t, "full day", stored(), want)
	disk.set(95, nil)
	f2.clock.Advance(10 * time.Second)
	f2.put("blog", "web", at(18, 10, 5), "line 5")
	wantStrings(t, "full day on a full disk", stored(), want)
}

func TestDiskUsageThatCannotBeReadKeepsWriting(t *testing.T) {
	disk := &fakeDisk{used: 99}
	f := newFixture(t, watchingDisk(disk))
	f.put("blog", "web", at(18, 10, 0), "line 0")
	disk.set(0, errors.New("statfs failed"))
	for i := 1; i <= 3; i++ {
		f.clock.Advance(10 * time.Second)
		f.put("blog", "web", at(18, 10, i), fmt.Sprintf("line %d", i))
	}
	wantStrings(t, "web", texts(serviceContent(t, f.dir, "blog", "web")), []string{f.c.diskMarker, "line 1", "line 2", "line 3"})
	if n := strings.Count(f.logs.String(), "disk usage unavailable"); n != 1 {
		t.Errorf("the failure was logged %d times, want once:\n%s", n, f.logs.String())
	}

	// Without a threshold the disk is not looked at.
	off := newFixture(t, func(o *Options) { o.DiskUsedPercent = disk.percent })
	before := disk.asked()
	off.put("blog", "web", at(18, 10, 0), "line 0")
	if disk.asked() != before {
		t.Error("the disk was asked although no threshold is set")
	}
	if _, err := New(Options{Dir: t.TempDir(), DiskStopPercent: 101}); err == nil {
		t.Error("a threshold above 100% was accepted")
	}
}
