package backup

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tricknotes/koroku/internal/clock"
)

// NextRun returns the first instant strictly after now at which a wall clock
// in loc shows hour:minute.
//
// Days are counted on the calendar, not in 24 hour steps, so the result
// stays at the same wall clock time across daylight saving changes. On the
// day the clocks go forward a time inside the skipped hour still yields
// exactly one instant, up to an hour off the nominal time (time.Date picks
// it); a time that occurs twice on the day the clocks go back runs once.
func NextRun(now time.Time, hour, minute int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	y, mo, d := local.Date()
	for i := 0; ; i++ {
		next := time.Date(y, mo, d+i, hour, minute, 0, 0, loc)
		if next.After(now) {
			return next
		}
	}
}

// Scheduler starts the daily run.
//
// Every scheduled run backs up the system and all apps, applies the
// retention policy and removes expired restore trash. The run that falls on
// CheckWeekday also verifies the repository (`restic check`); a failed check
// fails that run, which is how it reaches the UI.
//
// A run that was due while koroku was not running is not made up for; the
// next one happens at the next scheduled time.
type Scheduler struct {
	Manager *Manager
	// Clock defaults to the wall clock.
	Clock clock.Clock
	// Hour and Minute are the time of day in Location (production: 03:00
	// Asia/Tokyo). A nil Location means UTC.
	Hour     int
	Minute   int
	Location *time.Location
	// CheckWeekday is the day of the weekly check, in Location. The zero
	// value is Sunday.
	CheckWeekday time.Weekday
	// PollInterval is how often the clock is read. Default 30s; a run
	// starts at most that much late.
	PollInterval time.Duration
	Logger       *slog.Logger
	// DumpApps lists the apps whose dumps may be due between the daily
	// runs (ARCHITECTURE 10.8). Nil means none: every dump waits for the
	// daily run.
	DumpApps func(ctx context.Context) ([]App, error)
}

// NextSlot returns the first instant strictly after now at which a wall
// clock in loc shows the given minute: the hours at which the dumps between
// the daily runs fall.
func NextSlot(now time.Time, minute int, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	local := now.In(loc)
	y, mo, d := local.Date()
	for h := local.Hour(); ; h++ {
		next := time.Date(y, mo, d, h, minute, 0, 0, loc)
		if next.After(now) {
			return next
		}
	}
}

// DumpDue reports whether the app's dump falls at slot, a run between the
// daily ones: its interval counts the hours from the daily run at
// dailyHour, which takes every dump itself. A sleeping app takes none: its
// data does not change.
func DumpDue(app App, slot time.Time, dailyHour int, loc *time.Location) bool {
	interval := app.DumpIntervalHours
	if interval <= 0 || interval >= 24 || app.DumpService == "" || app.Sleeping {
		return false
	}
	if loc == nil {
		loc = time.UTC
	}
	offset := ((slot.In(loc).Hour()-dailyHour)%24 + 24) % 24
	return offset != 0 && offset%interval == 0
}

// Next returns when the next run is due after now.
func (s *Scheduler) Next(now time.Time) time.Time {
	return NextRun(now, s.Hour, s.Minute, s.Location)
}

// checkDue reports whether the run due at t includes the weekly check.
func (s *Scheduler) checkDue(t time.Time) bool {
	loc := s.Location
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc).Weekday() == s.CheckWeekday
}

// Run blocks until ctx is done, starting a run whenever one is due. When the
// repository is busy at that moment (a manual run, a restore), the run starts
// as soon as it is free again.
func (s *Scheduler) Run(ctx context.Context) error {
	clk := s.Clock
	if clk == nil {
		clk = clock.System{}
	}
	log := s.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	poll := s.PollInterval
	if poll <= 0 {
		poll = 30 * time.Second
	}

	next := s.Next(clk.Now())
	log.Info("backup schedule", "next", next.UTC().Format(time.RFC3339))
	slot := NextSlot(clk.Now(), s.Minute, s.Location)
	// pending holds the slots whose dumps have not started yet: the
	// repository was busy then. They are taken together, once, when it is
	// free again; a dump is not taken twice in a row to catch up.
	var pending []time.Time
	timer := time.NewTimer(poll)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if s.DumpApps != nil {
			for now := clk.Now(); !now.Before(slot); slot = NextSlot(slot, s.Minute, s.Location) {
				pending = append(pending, slot)
			}
			pending = s.runDumps(ctx, pending, log)
		}
		if !clk.Now().Before(next) {
			run, err := s.Manager.start(ctx, TriggerScheduled, plan{
				system: s.Manager.opts.System != nil,
				apps:   s.Manager.opts.Apps,
				forget: true,
				check:  s.checkDue(next),
				trash:  true,
			})
			switch {
			case errors.Is(err, ErrBusy):
				// Keep next as it is: try again at the next poll.
				log.Info("scheduled backup waits: another backup operation is in progress")
			case err != nil:
				log.Error("scheduled backup could not start", "error", err)
				next = s.Next(clk.Now())
			default:
				if _, err := run.Wait(); err != nil {
					log.Error("scheduled backup failed", "run", run.ID, "error", err)
				}
				next = s.Next(clk.Now())
				log.Info("backup schedule", "next", next.UTC().Format(time.RFC3339))
			}
		}
		timer.Reset(poll)
	}
}

// runDumps starts the run of the dumps due at the pending slots and returns
// the slots still pending: all of them when the repository is busy, none
// otherwise.
func (s *Scheduler) runDumps(ctx context.Context, pending []time.Time, log *slog.Logger) []time.Time {
	if len(pending) == 0 {
		return nil
	}
	apps, err := s.DumpApps(ctx)
	if err != nil {
		log.Error("listing the apps for the interval dumps failed", "error", err)
		return nil
	}
	var due []App
	for _, app := range apps {
		for _, at := range pending {
			if DumpDue(app, at, s.Hour, s.Location) {
				due = append(due, app)
				break
			}
		}
	}
	if len(due) == 0 {
		return nil
	}
	run, err := s.Manager.start(ctx, TriggerInterval, plan{
		apps:     func(context.Context) ([]App, error) { return due, nil },
		dumpOnly: true,
	})
	switch {
	case errors.Is(err, ErrBusy):
		return pending
	case err != nil:
		log.Error("interval dumps could not start", "error", err)
		return nil
	}
	go func() {
		if _, err := run.Wait(); err != nil {
			log.Error("interval dumps failed", "run", run.ID, "error", err)
		}
	}()
	return nil
}
