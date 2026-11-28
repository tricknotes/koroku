package backup

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata" // the tests must not depend on the host's zone database

	"github.com/tricknotes/koroku/internal/runner"
)

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestNextRun(t *testing.T) {
	tokyo := mustLocation(t, "Asia/Tokyo")
	newYork := mustLocation(t, "America/New_York")
	utc := func(s string) time.Time {
		t.Helper()
		at, err := time.Parse("2006-01-02 15:04:05", s)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	cases := []struct {
		name         string
		now          string
		hour, minute int
		loc          *time.Location
		want         string
	}{
		// 03:00 JST is 18:00 UTC of the day before; there is no DST.
		{"Tokyo, earlier the same local day", "2026-11-18 16:00:00", 3, 0, tokyo, "2026-11-18 18:00:00"},
		{"Tokyo, one second before", "2026-11-18 17:59:59", 3, 0, tokyo, "2026-11-18 18:00:00"},
		{"Tokyo, exactly at the time: the next day", "2026-11-18 18:00:00", 3, 0, tokyo, "2026-11-19 18:00:00"},
		{"Tokyo, later the same local day", "2026-11-18 20:00:00", 3, 0, tokyo, "2026-11-19 18:00:00"},
		{"Tokyo, local date ahead of UTC", "2026-11-18 15:30:00", 3, 0, tokyo, "2026-11-18 18:00:00"},
		{"Tokyo, across the end of the month", "2026-11-30 18:00:01", 3, 0, tokyo, "2026-12-01 18:00:00"},
		{"Tokyo, across the end of the year", "2026-12-31 19:00:00", 3, 0, tokyo, "2027-01-01 18:00:00"},
		{"Tokyo, 23:59 reached exactly: across midnight to the next day", "2026-11-18 14:59:00", 23, 59, tokyo, "2026-11-19 14:59:00"},
		{"Tokyo, 00:00 from one minute before midnight", "2026-11-18 14:59:00", 0, 0, tokyo, "2026-11-18 15:00:00"},
		{"UTC when no location is given", "2026-11-18 03:00:00", 3, 0, nil, "2026-11-19 03:00:00"},
		// New York: EST is UTC-5, EDT is UTC-4. Clocks go forward on
		// 2027-03-14 at 02:00 and back on 2026-11-01 at 02:00.
		{"New York, winter", "2026-12-01 12:00:00", 3, 0, newYork, "2026-12-02 08:00:00"},
		{"New York, the night the clocks go forward: 23 hours later", "2027-03-13 08:00:00", 3, 0, newYork, "2027-03-14 07:00:00"},
		{"New York, summer", "2027-03-14 07:00:00", 3, 0, newYork, "2027-03-15 07:00:00"},
		{"New York, the night the clocks go back: 25 hours later", "2026-10-31 07:00:00", 3, 0, newYork, "2026-11-01 08:00:00"},
		{"New York, a time that occurs twice runs at its first occurrence", "2026-10-31 05:30:00", 1, 30, newYork, "2026-11-01 05:30:00"},
		{"New York, and not again at its second occurrence", "2026-11-01 05:30:00", 1, 30, newYork, "2026-11-02 06:30:00"},
		{"New York, between the two occurrences", "2026-11-01 06:00:00", 1, 30, newYork, "2026-11-02 06:30:00"},
	}
	for _, c := range cases {
		now := utc(c.now)
		got := NextRun(now, c.hour, c.minute, c.loc)
		want := utc(c.want)
		if !got.Equal(want) {
			t.Errorf("%s: NextRun(%s) = %s, want %s", c.name, c.now, got.UTC().Format(time.DateTime), want.Format(time.DateTime))
		}
		if !got.After(now) {
			t.Errorf("%s: NextRun did not move forward", c.name)
		}
	}
}

// A time inside the hour that is skipped when the clocks go forward still
// yields one run on that day, and the schedule goes on normally after it.
func TestNextRunInsideTheSkippedHour(t *testing.T) {
	newYork := mustLocation(t, "America/New_York")
	now := time.Date(2027, 3, 13, 12, 0, 0, 0, time.UTC)
	var runs []time.Time
	for range 3 {
		now = NextRun(now, 2, 30, newYork)
		runs = append(runs, now)
	}
	// 2027-03-14 02:30 does not exist; 01:30 EST and 03:30 EDT are the
	// candidates, one hour apart.
	gapDay := runs[0].In(newYork)
	if y, m, d := gapDay.Date(); y != 2027 || m != time.March || d != 14 {
		t.Fatalf("first run = %s, want one on 2027-03-14", gapDay)
	}
	if h := gapDay.Hour(); h != 1 && h != 3 || gapDay.Minute() != 30 {
		t.Errorf("run on the day of the change = %s", gapDay)
	}
	for i, wantDay := range []int{15, 16} {
		local := runs[i+1].In(newYork)
		if local.Day() != wantDay || local.Hour() != 2 || local.Minute() != 30 {
			t.Errorf("run %d = %s, want 02:30 on March %d", i+1, local, wantDay)
		}
	}
}

// waitFor polls until cond holds; the scheduler runs on its own goroutine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSchedulerRunsDailyAndChecksWeekly(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	writeFilesInTrash := func(name string) {
		writeFiles(t, e.m.opts.TrashDir, map[string]string{name + "/file": "x"})
	}
	writeFilesInTrash("blog-uploads-20261101T000000Z") // expired on every run below

	tokyo := mustLocation(t, "Asia/Tokyo")
	// Friday 2026-11-20 12:00 JST. The first run is Saturday 03:00 JST.
	e.clock.Set(time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC))
	s := &Scheduler{Manager: e.m, Clock: e.clock, Hour: 3, Location: tokyo, PollInterval: time.Millisecond, Logger: e.m.opts.Logger}
	if got, want := s.Next(e.clock.Now()), time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Next = %s, want %s", got, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	e.waitForSchedule()

	// Nothing happens before the time has come.
	e.clock.Set(time.Date(2026, 11, 20, 17, 59, 59, 0, time.UTC))
	time.Sleep(20 * time.Millisecond)
	if n := len(e.rec.finishedRuns()); n != 0 {
		t.Fatalf("%d runs before the scheduled time", n)
	}

	// Saturday 03:00 JST: backup and retention, no check.
	e.clock.Set(time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC))
	waitFor(t, "the first run", func() bool { return len(e.rec.finishedRuns()) == 1 })
	assertLines(t, "Saturday's commands", e.host.calls(), []string{
		"restic cat config",
		"restic unlock",
		"docker volume ls --filter label=com.docker.compose.project=koroku-blog --format {{.Name}}",
		"restic unlock",
		"restic forget --group-by tags --keep-within 48h --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune --retry-lock 1m0s",
	})
	if entries, _ := readDirNames(e.m.opts.TrashDir); len(entries) != 0 {
		t.Errorf("expired restore trash was kept: %v", entries)
	}
	// One run per day, however often the clock is read.
	time.Sleep(20 * time.Millisecond)
	if n := len(e.rec.finishedRuns()); n != 1 {
		t.Fatalf("%d runs for one scheduled time", n)
	}

	// Sunday 03:00 JST (slightly late): the weekly check is part of the run.
	e.host.run.Reset()
	e.clock.Set(time.Date(2026, 11, 21, 18, 0, 20, 0, time.UTC))
	waitFor(t, "the second run", func() bool { return len(e.rec.finishedRuns()) == 2 })
	assertLines(t, "Sunday's commands", e.host.calls(), []string{
		"restic unlock",
		"docker volume ls --filter label=com.docker.compose.project=koroku-blog --format {{.Name}}",
		"restic unlock",
		"restic forget --group-by tags --keep-within 48h --keep-daily 7 --keep-weekly 4 --keep-monthly 6 --prune --retry-lock 1m0s",
		"restic unlock",
		"restic check --retry-lock 1m0s",
	})
	e.assertCleanCommands()
	if got := e.rec.startedRuns(); len(got) != 2 || got[0] != TriggerScheduled || got[1] != TriggerScheduled {
		t.Errorf("recorded triggers = %v", got)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when its context was canceled")
	}
}

// waitForSchedule returns once the scheduler has read the clock and computed
// its first run; moving the fake clock before that would move the schedule.
func (e *env) waitForSchedule() {
	e.t.Helper()
	waitFor(e.t, "the scheduler to start", func() bool { return strings.Contains(e.logs.String(), "backup schedule") })
}

func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, err
}

func TestSchedulerReportsAFailedCheckThroughTheRun(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	e.host.set("restic check", func(_ context.Context, cmd runner.Cmd) (string, error) {
		cmd.Stderr.Write([]byte("Fatal: repository contains errors\n"))
		return "", exit("restic", 1)
	})
	// Sunday 2026-11-22 02:59 UTC; the schedule is 03:00 UTC.
	e.clock.Set(time.Date(2026, 11, 22, 2, 59, 0, 0, time.UTC))
	s := &Scheduler{Manager: e.m, Clock: e.clock, Hour: 3, PollInterval: time.Millisecond, Logger: e.m.opts.Logger}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	e.waitForSchedule()

	e.clock.Advance(time.Minute)
	waitFor(t, "the run", func() bool { return len(e.rec.finishedRuns()) == 1 })
	if got, want := e.rec.finishedRuns()[0], "1 failed repository check: restic check: exit status 1: Fatal: repository contains errors"; got != want {
		t.Errorf("recorded run = %q, want %q", got, want)
	}
}

func TestSchedulerWaitsWhileBusyAndThenRuns(t *testing.T) {
	e := newEnv(t)
	e.apps = []App{blog}
	e.volume(blog, "uploads", nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	e.host.set("restic backup", func(ctx context.Context, cmd runner.Cmd) (string, error) {
		once.Do(func() {
			close(entered)
			<-release
		})
		return e.host.backup(ctx, cmd)
	})
	manual, err := e.m.Start(context.Background(), TriggerManual)
	if err != nil {
		t.Fatal(err)
	}
	<-entered

	e.clock.Set(time.Date(2026, 11, 18, 2, 59, 0, 0, time.UTC))
	s := &Scheduler{Manager: e.m, Clock: e.clock, Hour: 3, PollInterval: time.Millisecond, Logger: e.m.opts.Logger}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	e.waitForSchedule()

	// The scheduled time passes while the manual run holds the repository.
	e.clock.Advance(2 * time.Minute)
	waitFor(t, "the scheduler to notice", func() bool { return strings.Contains(e.logs.String(), "scheduled backup waits") })
	if got := e.rec.startedRuns(); len(got) != 1 {
		t.Fatalf("runs started while busy: %v", got)
	}
	close(release)
	if _, err := manual.Wait(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the delayed scheduled run", func() bool { return len(e.rec.finishedRuns()) == 2 })
	if got := e.rec.startedRuns(); got[1] != TriggerScheduled {
		t.Errorf("recorded triggers = %v", got)
	}
}
