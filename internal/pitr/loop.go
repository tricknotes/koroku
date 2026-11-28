package pitr

import (
	"context"
	"errors"
	"time"

	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/store"
)

// DeploymentSucceeded queues the preparation of an app after one of its
// deployments succeeded (ARCHITECTURE 10.9). It returns at once.
func (s *Service) DeploymentSucceeded(appID int64) {
	select {
	case s.queue <- appID:
	default:
		s.opts.Logger.Warn("point-in-time recovery: too many preparations queued; this one waits for the next deployment", "app_id", appID)
	}
}

func (s *Service) wantBase(appID int64) {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	s.needBase[appID] = true
}

// Run blocks until ctx is done. It prepares the apps whose deployments
// succeeded, ships the WAL every hour and takes the base backups after the
// daily backup run. Work that finds the repository busy is taken again at
// the next reading of the clock.
func (s *Service) Run(ctx context.Context) error {
	loc := s.opts.Location
	now := s.opts.Clock.Now()
	nextHour := backup.NextSlot(now, 0, loc)
	nextDaily := backup.NextRun(now, s.opts.Hour, s.opts.Minute, loc)
	archivePending := false
	timer := time.NewTimer(s.opts.PollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case id := <-s.queue:
			app, err := s.opts.Store.GetAppByID(ctx, id)
			if err == nil && app.PITRService != "" {
				if err := s.Prepare(ctx, app); err != nil {
					s.opts.Logger.Warn("point-in-time recovery could not be prepared", "app", app.Slug, "error", err)
				}
			}
			continue
		case <-timer.C:
		}
		now := s.opts.Clock.Now()
		if !now.Before(nextHour) {
			archivePending = true
			nextHour = backup.NextSlot(now, 0, loc)
		}
		if !now.Before(nextDaily) {
			s.wantBases(ctx)
			nextDaily = backup.NextRun(now, s.opts.Hour, s.opts.Minute, loc)
		}
		if archivePending {
			archivePending = s.archiveAll(ctx)
		}
		s.takeBases(ctx)
		timer.Reset(s.opts.PollInterval)
	}
}

// readyApps lists the apps whose archiving is set up.
func (s *Service) readyApps(ctx context.Context) []store.App {
	apps, err := s.opts.Store.ListPITRApps(ctx)
	if err != nil {
		s.opts.Logger.Error("listing the apps with point-in-time recovery failed", "error", err)
		return nil
	}
	var out []store.App
	for _, app := range apps {
		st, err := s.opts.Store.GetPITRState(ctx, app.ID)
		if err == nil && st.Status == store.PITRReady && st.Service == app.PITRService {
			out = append(out, app)
		}
	}
	return out
}

func (s *Service) wantBases(ctx context.Context) {
	for _, app := range s.readyApps(ctx) {
		s.wantBase(app.ID)
	}
}

// archiveAll ships the WAL of every app; it reports whether the repository
// was busy, so that the hour is tried again.
func (s *Service) archiveAll(ctx context.Context) (busy bool) {
	if s.opts.Repo == nil {
		return false
	}
	for _, app := range s.readyApps(ctx) {
		err := s.Archive(ctx, app)
		switch {
		case errors.Is(err, backup.ErrBusy):
			busy = true
		case err != nil:
			s.opts.Logger.Error("shipping WAL failed", "app", app.Slug, "error", err)
		}
	}
	return busy
}

// takeBases takes the base backups that are due. One that finds the
// repository busy stays due; one that fails otherwise waits for the next
// day, or for the next event that asks for one.
func (s *Service) takeBases(ctx context.Context) {
	if s.opts.Repo == nil {
		return
	}
	s.pendMu.Lock()
	ids := make([]int64, 0, len(s.needBase))
	for id := range s.needBase {
		ids = append(ids, id)
	}
	s.pendMu.Unlock()
	for _, id := range ids {
		app, err := s.opts.Store.GetAppByID(ctx, id)
		var st store.PITRState
		if err == nil {
			st, err = s.opts.Store.GetPITRState(ctx, id)
		}
		if err != nil || app.PITRService == "" || st.Status != store.PITRReady || app.DesiredState != store.DesiredRunning {
			s.donePending(id)
			continue
		}
		s.mu.Lock()
		err = s.base(ctx, app)
		s.mu.Unlock()
		if errors.Is(err, backup.ErrBusy) {
			continue
		}
		if err != nil {
			s.opts.Logger.Error("base backup failed", "app", app.Slug, "error", err)
		}
		s.donePending(id)
	}
}

func (s *Service) donePending(id int64) {
	s.pendMu.Lock()
	defer s.pendMu.Unlock()
	delete(s.needBase, id)
}

// --- status -------------------------------------------------------------

// States of Status.State.
const (
	StateOff         = "off"
	StateUnavailable = "unavailable"
	StateWaiting     = "waiting"
	StatePreparing   = store.PITRPreparing
	StateReady       = store.PITRReady
	StateFailed      = store.PITRFailed
)

// Status is what GET /api/apps/{slug}/pitr reports.
type Status struct {
	Service string
	// State is off, unavailable (backups are not configured), waiting
	// (for a deployment that mounts the archive directory), preparing,
	// ready or failed.
	State   string
	Error   string
	Warning string
	// EarliestAt is the oldest time the database can be brought back to:
	// the end of the oldest base backup kept. Nil without one.
	EarliestAt *time.Time
	// ArchivedUntil is how far the WAL in the repository reaches; what is
	// newer is on the host only.
	ArchivedUntil *time.Time
	Gaps          []store.PITRGap
}

// Status reports the point-in-time recovery of an app.
func (s *Service) Status(ctx context.Context, app store.App) (Status, error) {
	out := Status{Service: app.PITRService, State: StateOff}
	if app.PITRService == "" {
		return out, nil
	}
	if s.opts.Repo == nil {
		out.State = StateUnavailable
		return out, nil
	}
	st, err := s.opts.Store.GetPITRState(ctx, app.ID)
	switch {
	case errors.Is(err, store.ErrNotFound) || err == nil && st.Service != app.PITRService:
		out.State = StateWaiting
		return out, nil
	case err != nil:
		return Status{}, err
	}
	out.State, out.Error, out.Warning = st.Status, st.Error, st.Warning
	since := s.opts.Clock.Now().Add(-backup.PITRWindow - 24*time.Hour)
	gaps, err := s.opts.Store.ListPITRGaps(ctx, app.ID, since)
	if err != nil {
		return Status{}, err
	}
	out.Gaps = gaps
	// Base backups of an older major version cannot be recovered with the
	// running server.
	for _, g := range gaps {
		if g.Reason == store.PITRGapVersionChanged && g.EndedAt != nil && g.EndedAt.After(since) {
			since = *g.EndedAt
		}
	}
	bases, err := s.opts.Store.ListPITRBases(ctx, app.ID, since)
	if err != nil {
		return Status{}, err
	}
	if len(bases) > 0 {
		at := bases[0].FinishedAt
		out.EarliestAt = &at
	}
	batches, err := s.opts.Store.ListPITRWALBatches(ctx, app.ID, since)
	if err != nil {
		return Status{}, err
	}
	if n := len(batches); n > 0 {
		at := batches[n-1].LastModifiedAt
		out.ArchivedUntil = &at
	}
	return out, nil
}
