package pitr

import (
	"errors"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

func TestPlanRestore(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	h := func(n int) time.Time { return now.Add(-time.Duration(n) * time.Hour) }
	base := func(id int64, at time.Time, version int) store.PITRBase {
		return store.PITRBase{ID: id, SnapshotID: "b", StartedAt: at.Add(-time.Minute), FinishedAt: at, ServerVersionNum: version}
	}
	bases := []store.PITRBase{base(1, h(72), 170000), base(2, h(48), 170000), base(3, h(24), 170000)}
	batch := func(id int64, at time.Time) store.PITRWALBatch {
		return store.PITRWALBatch{ID: id, CreatedAt: at, LastModifiedAt: at.Add(-5 * time.Minute)}
	}
	var batches []store.PITRWALBatch
	for i := 72; i >= 1; i-- {
		batches = append(batches, batch(int64(73-i), h(i)))
	}

	plan, err := PlanRestore(h(30), now, 170004, bases, batches, nil)
	if err != nil || plan.Base.ID != 2 {
		t.Fatalf("30 hours ago: %+v, %v", plan.Base, err)
	}
	first, last := plan.Batches[0], plan.Batches[len(plan.Batches)-1]
	if first.CreatedAt.Before(plan.Base.StartedAt) || last.LastModifiedAt.Before(h(30)) || len(plan.Batches) != 20 {
		t.Errorf("batches %v .. %v (%d)", first.CreatedAt, last.CreatedAt, len(plan.Batches))
	}

	// A gap between the newest base and the time: the one before it.
	gapEnd := h(26)
	gaps := []store.PITRGap{{StartedAt: h(28), EndedAt: &gapEnd, Reason: store.PITRGapDropped}}
	if plan, err := PlanRestore(h(20), now, 170004, bases, batches, gaps); err != nil || plan.Base.ID != 3 {
		t.Errorf("after the gap: %+v, %v", plan.Base, err)
	}
	if _, err := PlanRestore(h(30), now, 170004, bases, batches, gaps); err != nil {
		t.Errorf("before the gap: %v", err)
	}
	if _, err := PlanRestore(h(27), now, 170004, bases, batches, gaps); !errors.Is(err, ErrInGap) {
		t.Errorf("in the gap: %v", err)
	}
	// Across a gap and with no base after it.
	gaps = []store.PITRGap{{StartedAt: h(50), EndedAt: &gapEnd, Reason: store.PITRGapMountMissing}}
	if _, err := PlanRestore(h(25), now, 170004, bases, batches, gaps); !errors.Is(err, ErrNoBase) {
		t.Errorf("no base after the gap: %v", err)
	}

	if _, err := PlanRestore(h(80), now, 170004, bases, batches, nil); !errors.Is(err, ErrNoBase) {
		t.Errorf("before the oldest base: %v", err)
	}
	if _, err := PlanRestore(now.Add(time.Minute), now, 170004, bases, batches, nil); !errors.Is(err, ErrFutureTarget) {
		t.Errorf("the future: %v", err)
	}
	// A base of an older major version is not used.
	if _, err := PlanRestore(h(30), now, 180001, bases, batches, nil); !errors.Is(err, ErrNoBase) {
		t.Errorf("older version: %v", err)
	}
}
