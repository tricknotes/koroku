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
// A run that was due while paasd was not running is not made up for; the
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
	timer := time.NewTimer(poll)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
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
