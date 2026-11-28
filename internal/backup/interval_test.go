package backup

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/runner"
)

func TestDumpDue(t *testing.T) {
	tokyo := mustLocation(t, "Asia/Tokyo")
	at := func(hour int) time.Time { return time.Date(2026, 11, 20, hour, 0, 0, 0, tokyo) }
	hourly := App{Slug: "blog", DumpService: "db", DumpIntervalHours: 1}
	sixHourly := App{Slug: "blog", DumpService: "db", DumpIntervalHours: 6}
	cases := []struct {
		name string
		app  App
		hour int
		want bool
	}{
		{"hourly, an hour after the daily run", hourly, 4, true},
		{"hourly, just before the daily run", hourly, 2, true},
		{"the daily run takes it itself", hourly, 3, false},
		{"every six hours from 03:00", sixHourly, 9, true},
		{"every six hours, across midnight", sixHourly, 21, true},
		{"every six hours, 03:00 the next day is the daily run", sixHourly, 3, false},
		{"every six hours, not at 10:00", sixHourly, 10, false},
		{"daily only", App{DumpService: "db", DumpIntervalHours: 24}, 15, false},
		{"not set", App{DumpService: "db"}, 15, false},
		{"no dump hook", App{DumpIntervalHours: 1}, 15, false},
		{"sleeping", App{DumpService: "db", DumpIntervalHours: 1, Sleeping: true}, 15, false},
	}
	for _, c := range cases {
		if got := DumpDue(c.app, at(c.hour), 3, tokyo); got != c.want {
			t.Errorf("%s: DumpDue at %02d:00 = %v, want %v", c.name, c.hour, got, c.want)
		}
	}
}

func TestNextSlot(t *testing.T) {
	tokyo := mustLocation(t, "Asia/Tokyo")
	cases := []struct{ now, want time.Time }{
		{time.Date(2026, 11, 20, 12, 30, 0, 0, tokyo), time.Date(2026, 11, 20, 13, 0, 0, 0, tokyo)},
		{time.Date(2026, 11, 20, 13, 0, 0, 0, tokyo), time.Date(2026, 11, 20, 14, 0, 0, 0, tokyo)},
		{time.Date(2026, 11, 20, 23, 59, 0, 0, tokyo), time.Date(2026, 11, 21, 0, 0, 0, 0, tokyo)},
	}
	for _, c := range cases {
		if got := NextSlot(c.now, 0, tokyo); !got.Equal(c.want) {
			t.Errorf("NextSlot(%s) = %s, want %s", c.now, got.In(tokyo), c.want)
		}
	}
}

// intervalEnv is a scheduler whose apps all have a dump hook, the blog
// every six hours and the shop daily.
func intervalEnv(t *testing.T) (*env, *Scheduler, *time.Location) {
	t.Helper()
	e := newEnv(t)
	blog6 := blogWithHook
	blog6.DumpIntervalHours = 6
	shop := App{Slug: "shop", Project: "koroku-shop", DumpService: "db", DumpCommand: "pg_dump app", DumpIntervalHours: 24}
	e.apps = []App{blog6, shop}
	e.host.set("docker compose", func(context.Context, runner.Cmd) (string, error) { return "dump\n", nil })
	tokyo := mustLocation(t, "Asia/Tokyo")
	s := &Scheduler{
		Manager: e.m, Clock: e.clock, Hour: 3, Location: tokyo,
		PollInterval: time.Millisecond, Logger: e.m.opts.Logger,
		DumpApps: func(context.Context) ([]App, error) { return e.apps, nil },
	}
	return e, s, tokyo
}

func runScheduler(t *testing.T, e *env, s *Scheduler) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	e.waitForSchedule()
}

func TestSchedulerTakesTheDumpsBetweenTheDailyRuns(t *testing.T) {
	e, s, tokyo := intervalEnv(t)
	e.clock.Set(time.Date(2026, 11, 20, 12, 30, 0, 0, tokyo))
	runScheduler(t, e, s)

	// 13:00 is no slot of the blog's (every six hours from 03:00).
	e.clock.Set(time.Date(2026, 11, 20, 13, 0, 0, 0, tokyo))
	time.Sleep(20 * time.Millisecond)
	if n := len(e.rec.startedRuns()); n != 0 {
		t.Fatalf("%d runs at 13:00", n)
	}

	e.clock.Set(time.Date(2026, 11, 20, 15, 0, 0, 0, tokyo))
	waitFor(t, "the interval dump", func() bool { return len(e.rec.finishedRuns()) == 1 })
	if got := e.rec.startedRuns(); len(got) != 1 || got[0] != TriggerInterval {
		t.Fatalf("runs = %v", got)
	}
	// The blog's dump only: no volume, no system, no retention.
	targets := e.rec.targets[0]
	if len(targets) != 1 || targets[0].App != "blog" || targets[0].Kind != KindDump || targets[0].Status != TargetSucceeded {
		t.Fatalf("targets = %+v", targets)
	}
	for _, call := range e.host.calls() {
		for _, refused := range []string{"docker volume", "restic forget", "restic check"} {
			if len(call) >= len(refused) && call[:len(refused)] == refused {
				t.Errorf("the interval run ran %q", call)
			}
		}
	}
	// Once per slot, however often the clock is read.
	time.Sleep(20 * time.Millisecond)
	if n := len(e.rec.startedRuns()); n != 1 {
		t.Errorf("%d runs for one slot", n)
	}
}

// A slot that passes while another operation holds the repository is taken
// once it is free, and slots that passed meanwhile are taken together.
func TestSchedulerTakesMissedDumpsOnceWhenFree(t *testing.T) {
	e, s, tokyo := intervalEnv(t)
	hourly := e.apps[0]
	hourly.DumpIntervalHours = 1
	e.apps = []App{hourly}
	e.clock.Set(time.Date(2026, 11, 20, 12, 30, 0, 0, tokyo))

	release, err := e.m.begin(OpRestore, "")
	if err != nil {
		t.Fatal(err)
	}
	runScheduler(t, e, s)
	e.clock.Set(time.Date(2026, 11, 20, 13, 0, 0, 0, tokyo))
	time.Sleep(10 * time.Millisecond)
	e.clock.Set(time.Date(2026, 11, 20, 14, 0, 0, 0, tokyo))
	time.Sleep(10 * time.Millisecond)
	if n := len(e.rec.startedRuns()); n != 0 {
		t.Fatalf("%d runs while the repository was busy", n)
	}
	release()
	waitFor(t, "the missed dump", func() bool { return len(e.rec.finishedRuns()) == 1 })
	time.Sleep(20 * time.Millisecond)
	if n := len(e.rec.startedRuns()); n != 1 {
		t.Errorf("%d runs for two missed slots, want one", n)
	}
	if n := len(e.host.calls("restic backup")); n != 1 {
		t.Errorf("%d dumps for two missed slots, want one", n)
	}
}

// When the repository cannot be opened, an interval run records the dumps
// it was to take as failed, so that it concerns those apps only and not
// every app as a failed daily run does.
func TestIntervalRunFailsItsOwnDumps(t *testing.T) {
	e, _, _ := intervalEnv(t)
	e.host.set("restic cat", func(_ context.Context, cmd runner.Cmd) (string, error) {
		io.WriteString(cmd.Stderr, `{"message_type":"exit_error","code":12,"message":"Fatal: wrong password or no key found"}`+"\n")
		return "", exit("restic", 12)
	})
	run, err := e.m.start(context.Background(), TriggerInterval, plan{
		apps:     func(context.Context) ([]App, error) { return e.apps[:1], nil },
		dumpOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.Wait(); !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err = %v", err)
	}
	targets := e.rec.targets[0]
	if len(targets) != 1 || targets[0].App != "blog" || targets[0].Kind != KindDump || targets[0].Status != TargetFailed {
		t.Errorf("targets = %+v", targets)
	}
}

// The duration of a dump is recorded, so that a dump that takes too long
// for its interval can be pointed out.
func TestDumpDurationIsRecorded(t *testing.T) {
	e := newEnv(t)
	var once sync.Once
	e.host.set("docker compose", func(context.Context, runner.Cmd) (string, error) {
		once.Do(func() { e.clock.Advance(90 * time.Second) })
		return "dump\n", nil
	})
	res, err := e.m.BackupApp(context.Background(), blogWithHook)
	if err != nil {
		t.Fatal(err)
	}
	if got := dumpTarget(t, res).Duration; got != 90*time.Second {
		t.Errorf("duration = %s, want 1m30s", got)
	}
	var recorded time.Duration
	for _, r := range e.rec.targets[0] {
		if r.Kind == KindDump {
			recorded = r.Duration
		}
	}
	if recorded != 90*time.Second {
		t.Errorf("recorded duration = %s", recorded)
	}
}
