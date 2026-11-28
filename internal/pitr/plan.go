package pitr

import (
	"errors"
	"fmt"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

// Why a time cannot be restored to.
var (
	ErrFutureTarget = errors.New("the time is in the future")
	ErrNoBase       = errors.New("no base backup reaches back to that time")
	ErrInGap        = errors.New("the time falls in a period that cannot be recovered to")
)

// RestorePlan is what a recovery to a time reads: the base backup it
// starts from and the WAL shipped since (ARCHITECTURE 10.9).
type RestorePlan struct {
	Base    store.PITRBase
	Batches []store.PITRWALBatch
}

// PlanRestore chooses the newest base backup finished by target that no
// gap separates from target, of the running server's major version, and
// the WAL batches shipped from its start until one reaches past target.
// What has not been shipped yet is read from the host during the restore.
func PlanRestore(target, now time.Time, version int, bases []store.PITRBase, batches []store.PITRWALBatch, gaps []store.PITRGap) (RestorePlan, error) {
	if target.After(now) {
		return RestorePlan{}, ErrFutureTarget
	}
	for _, g := range gaps {
		if !target.Before(g.StartedAt) && (g.EndedAt == nil || target.Before(*g.EndedAt)) {
			return RestorePlan{}, fmt.Errorf("%w (%s)", ErrInGap, g.Reason)
		}
	}
	var base *store.PITRBase
	for i := len(bases) - 1; i >= 0; i-- {
		b := bases[i]
		if b.FinishedAt.After(target) || b.ServerVersionNum/10000 != version/10000 {
			continue
		}
		if separated(b.StartedAt, target, gaps) {
			continue
		}
		base = &b
		break
	}
	if base == nil {
		return RestorePlan{}, ErrNoBase
	}
	plan := RestorePlan{Base: *base}
	for _, batch := range batches {
		if batch.CreatedAt.Before(base.StartedAt) {
			continue
		}
		plan.Batches = append(plan.Batches, batch)
		if !batch.LastModifiedAt.Before(target) {
			break
		}
	}
	return plan, nil
}

// separated reports whether a gap lies between from and to.
func separated(from, to time.Time, gaps []store.PITRGap) bool {
	for _, g := range gaps {
		if g.StartedAt.Before(to) && (g.EndedAt == nil || g.EndedAt.After(from)) {
			return true
		}
	}
	return false
}
