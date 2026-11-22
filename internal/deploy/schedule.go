package deploy

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/tricknotes/koroku/internal/cron"
	"github.com/tricknotes/koroku/internal/store"
)

// Scheduled registered tasks (ARCHITECTURE 9.6): koroku runs the tasks
// that have a schedule at their times, one at a time across all apps.

const (
	// TaskSchedulePoll is how often the scheduler reads the clock.
	TaskSchedulePoll = 15 * time.Second
	// TaskBusyWait is how long a scheduled run waits for its app to be
	// free (a deployment, another operation) before it is skipped.
	TaskBusyWait = 10 * time.Minute
	// maxSkipDetail bounds the problem recorded with a skip.
	maxSkipDetail = 500
)

// scheduleKey identifies a task with its schedule: changing the schedule
// makes it a new entry, counted from the time of the change.
type scheduleKey struct {
	appID    int64
	name     string
	schedule string
}

// queuedRun is a scheduled time waiting in the queue or running.
type queuedRun struct {
	key scheduleKey
	due time.Time
}

// TaskScheduler runs registered tasks on their schedules. Every poll it
// queues the tasks whose time has come; a single worker runs the queue in
// order, waiting for each run to end. Times that passed while koroku was
// not running are not made up for.
type TaskScheduler struct {
	engine   *Engine
	location *time.Location
	// Poll and BusyWait default to TaskSchedulePoll and TaskBusyWait.
	Poll     time.Duration
	BusyWait time.Duration

	mu sync.Mutex
	// next is the next time of each scheduled task.
	next map[scheduleKey]time.Time
	// queue holds the runs waiting or running, oldest first. A run stays
	// in it until it has ended, so that its next time sees it.
	queue []queuedRun
	wake  chan struct{}
}

// NewTaskScheduler returns the scheduler of registered tasks, reading the
// schedules on the wall clock of loc (nil means UTC).
func (e *Engine) NewTaskScheduler(loc *time.Location) *TaskScheduler {
	if loc == nil {
		loc = time.UTC
	}
	return &TaskScheduler{
		engine:   e,
		location: loc,
		next:     map[scheduleKey]time.Time{},
		wake:     make(chan struct{}, 1),
	}
}

func (s *TaskScheduler) poll() time.Duration {
	if s.Poll > 0 {
		return s.Poll
	}
	return TaskSchedulePoll
}

func (s *TaskScheduler) busyWait() time.Duration {
	if s.BusyWait > 0 {
		return s.BusyWait
	}
	return TaskBusyWait
}

// Run schedules and runs tasks until ctx is canceled.
func (s *TaskScheduler) Run(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.work(ctx)
	}()
	ticker := time.NewTicker(s.poll())
	defer ticker.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			<-done
			return
		case <-ticker.C:
		}
	}
}

// tick queues the tasks whose time has come.
func (s *TaskScheduler) tick(ctx context.Context) {
	log := s.engine.opts.Logger
	tasks, err := s.engine.opts.Store.ListScheduledTasks(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Error("reading the scheduled tasks failed", "error", err)
		}
		return
	}
	now := s.engine.opts.Clock.Now()
	seen := map[scheduleKey]bool{}
	var due []queuedRun
	s.mu.Lock()
	for _, t := range tasks {
		key := scheduleKey{appID: t.AppID, name: t.Name, schedule: t.Schedule}
		seen[key] = true
		sched, err := cron.Parse(t.Schedule)
		if err != nil {
			// The API validates schedules; this is a database edited by hand.
			continue
		}
		next, ok := s.next[key]
		if !ok {
			s.next[key] = sched.Next(now, s.location)
			continue
		}
		if next.IsZero() || now.Before(next) {
			continue
		}
		s.next[key] = sched.Next(now, s.location)
		due = append(due, queuedRun{key: key, due: next.In(s.location)})
	}
	for key := range s.next {
		if !seen[key] {
			delete(s.next, key)
		}
	}
	s.mu.Unlock()
	for _, run := range due {
		s.enqueue(ctx, run)
	}
}

// enqueue adds a run to the queue, unless the previous run of the same
// task is still queued or running.
func (s *TaskScheduler) enqueue(ctx context.Context, run queuedRun) {
	s.mu.Lock()
	for _, q := range s.queue {
		if q.key == run.key {
			s.mu.Unlock()
			s.skip(ctx, run, store.TaskSkipOverlap, "")
			return
		}
	}
	s.queue = append(s.queue, run)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *TaskScheduler) remove(run queuedRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, q := range s.queue {
		if q == run {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return
		}
	}
}

func (s *TaskScheduler) queued() []queuedRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]queuedRun(nil), s.queue...)
}

// work runs the queue until ctx is canceled.
func (s *TaskScheduler) work(ctx context.Context) {
	for ctx.Err() == nil {
		if s.step(ctx) {
			continue
		}
		select {
		case <-ctx.Done():
		case <-s.wake:
		case <-time.After(s.poll()):
		}
	}
}

// step takes the first queued run whose app is free and runs it to its
// end. Runs whose app is busy stay queued, so that the apps behind them
// are not held up. It reports whether it removed anything from the queue.
func (s *TaskScheduler) step(ctx context.Context) bool {
	for _, run := range s.queued() {
		if ctx.Err() != nil {
			return false
		}
		if s.attempt(ctx, run) {
			s.remove(run)
			return true
		}
	}
	return false
}

// attempt runs one queued run and reports whether it is done with (run,
// skipped or dropped), or false when it waits for its app to be free.
func (s *TaskScheduler) attempt(ctx context.Context, run queuedRun) bool {
	log := s.engine.opts.Logger
	st := s.engine.opts.Store
	app, err := st.GetAppByID(ctx, run.key.appID)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		log.Error("reading the app of a scheduled task failed", "error", err)
		return ctx.Err() == nil
	}
	task, err := st.GetTask(ctx, app.ID, run.key.name)
	if errors.Is(err, store.ErrNotFound) {
		// Removed since it was queued.
		return true
	}
	if err != nil {
		log.Error("reading a scheduled task failed", "app", app.Slug, "task", run.key.name, "error", err)
		return ctx.Err() == nil
	}
	switch {
	case app.DesiredState == store.DesiredStopped:
		s.skip(ctx, run, store.TaskSkipStopped, "")
		return true
	case app.CurrentDeploymentID == nil:
		s.skip(ctx, run, store.TaskSkipNotDeployed, "")
		return true
	}
	started, err := s.engine.RunTask(ctx, app, TaskSpec{
		TaskName: task.Name, Service: task.Service, Command: task.Command,
		Scheduled: true, DueAt: run.due,
	})
	switch {
	case errors.Is(err, ErrBusy):
		if s.engine.opts.Clock.Now().Sub(run.due) < s.busyWait() {
			return false
		}
		s.skip(ctx, run, store.TaskSkipBusy, "")
		return true
	case errors.Is(err, ErrNotDeployed):
		s.skip(ctx, run, store.TaskSkipNotDeployed, "")
		return true
	case err != nil:
		if ctx.Err() != nil {
			return false
		}
		s.skip(ctx, run, store.TaskSkipError, truncate(err.Error(), maxSkipDetail))
		return true
	}
	log.Info("scheduled task started", "app", app.Slug, "task", task.Name, "task_run", started.ID,
		"due", run.due.Format(time.RFC3339))
	if err := s.engine.WaitTask(ctx, started.ID); err != nil {
		// koroku is stopping; the run is recorded as interrupted.
		return true
	}
	return true
}

// skip records that a scheduled time was not run.
func (s *TaskScheduler) skip(ctx context.Context, run queuedRun, reason, detail string) {
	s.engine.opts.Logger.Info("scheduled task skipped", "app_id", run.key.appID, "task", run.key.name,
		"due", run.due.Format(time.RFC3339), "reason", reason, "detail", detail)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := s.engine.opts.Store.RecordTaskSkip(ctx, run.key.appID, run.key.name, run.key.schedule,
		run.due, reason, detail); err != nil {
		s.engine.opts.Logger.Error("recording a skipped scheduled task failed", "task", run.key.name, "error", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Cut on a rune boundary.
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}
