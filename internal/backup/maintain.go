package backup

import (
	"context"
	"strconv"
	"time"
)

// Forget applies the retention policy and prunes the repository. The error
// matches ErrBusy when another operation is in progress. A scheduled run
// does this by itself.
func (m *Manager) Forget(ctx context.Context) error {
	return m.maintain(ctx, m.forget)
}

// Check verifies the structure of the repository. The error matches ErrBusy
// when another operation is in progress. The Scheduler does this weekly.
func (m *Manager) Check(ctx context.Context) error {
	return m.maintain(ctx, m.check)
}

func (m *Manager) maintain(ctx context.Context, step func(context.Context) error) error {
	release, err := m.begin(OpMaintenance, "")
	if err != nil {
		return err
	}
	defer release()
	if err := m.ensureRepository(ctx, false); err != nil {
		return err
	}
	return step(ctx)
}

// forgetArgs is the retention policy.
//
// --group-by tags makes every distinct tag set its own group, and the tag
// set of a snapshot is exactly (app, kind, target) or (system). Each volume,
// each dump and the system therefore keep their own daily, weekly and
// monthly generations. restic's default grouping (host,paths) would tie the
// generations to things that change for reasons unrelated to the data: the
// host name after the machine is replaced, the staging directory of the
// system backup, Docker's data root.
//
// Every snapshot of the recent period (Retention.Within, 48 hours by
// default) is kept besides those generations: the dumps taken between the
// daily runs (ARCHITECTURE 10.8), which is when a mistake is usually
// noticed.
func (m *Manager) forgetArgs() []string {
	k := m.opts.Keep
	args := []string{"forget", "--group-by", "tags"}
	if hours := int(k.Within.Hours()); hours > 0 {
		args = append(args, "--keep-within", strconv.Itoa(hours)+"h")
	}
	// The snapshots of point-in-time recovery have a retention of their own
	// (pitrForgetArgs); the daily generations would thin the hourly WAL to
	// one snapshot a day and break the chain.
	args = append(args, "--keep-tag", tagKindPrefix+string(KindPITRBase), "--keep-tag", tagKindPrefix+string(KindWAL))
	return append(args,
		"--keep-daily", strconv.Itoa(k.Daily),
		"--keep-weekly", strconv.Itoa(k.Weekly),
		"--keep-monthly", strconv.Itoa(k.Monthly),
		"--prune",
		"--retry-lock", m.lockWait(),
	)
}

// PITRWindow is how long point-in-time recovery reaches back, and
// pitrKeep how long its snapshots are kept: one day more, so that a base
// backup older than the window's start is still there (ARCHITECTURE 10.9).
const (
	PITRWindow = 7 * 24 * time.Hour
	pitrKeep   = PITRWindow + 24*time.Hour
)

// pitrForgetArgs is the retention of the base backups and the WAL of
// point-in-time recovery. The prune of forgetArgs removes their data.
func (m *Manager) pitrForgetArgs() []string {
	return []string{
		"forget",
		"--tag", tagKindPrefix + string(KindPITRBase),
		"--tag", tagKindPrefix + string(KindWAL),
		"--group-by", "tags",
		"--keep-within", strconv.Itoa(int(pitrKeep.Hours())) + "h",
		"--retry-lock", m.lockWait(),
	}
}

// forget needs an exclusive lock, so locks of killed restic processes are
// cleared first (see unlockStale).
func (m *Manager) forget(ctx context.Context) error {
	if err := m.unlockStale(ctx); err != nil {
		return err
	}
	if err := m.restic(ctx, resticCall{args: m.pitrForgetArgs(), timeout: m.opts.Timeouts.Maintenance}); err != nil {
		return err
	}
	return m.restic(ctx, resticCall{args: m.forgetArgs(), timeout: m.opts.Timeouts.Maintenance})
}

func (m *Manager) check(ctx context.Context) error {
	if err := m.unlockStale(ctx); err != nil {
		return err
	}
	return m.restic(ctx, resticCall{
		args:    []string{"check", "--retry-lock", m.lockWait()},
		timeout: m.opts.Timeouts.Maintenance,
	})
}
