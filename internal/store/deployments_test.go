package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCanTransition(t *testing.T) {
	all := []string{StatusQueued, StatusRunning, StatusSucceeded, StatusFailed, StatusCanceled}
	allowed := map[[2]string]bool{
		{StatusQueued, StatusRunning}:    true,
		{StatusQueued, StatusCanceled}:   true,
		{StatusRunning, StatusSucceeded}: true,
		{StatusRunning, StatusFailed}:    true,
		{StatusRunning, StatusCanceled}:  true,
	}
	for _, from := range all {
		for _, to := range all {
			if got := CanTransition(from, to); got != allowed[[2]string{from, to}] {
				t.Errorf("CanTransition(%s, %s) = %v", from, to, got)
			}
		}
	}
	for _, s := range all {
		want := s == StatusSucceeded || s == StatusFailed || s == StatusCanceled
		if IsTerminal(s) != want {
			t.Errorf("IsTerminal(%s) = %v", s, IsTerminal(s))
		}
	}
}

func enqueue(t *testing.T, s *Store, appID int64, sha string) Deployment {
	t.Helper()
	d, _, err := s.EnqueueDeployment(context.Background(), appID, "main", sha, "", testNow)
	if err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}
	return d
}

func TestDeploymentHappyPath(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")

	d := enqueue(t, s, app.ID, "aaa")
	if d.Status != StatusQueued || d.StartedAt != nil || !d.QueuedAt.Equal(testNow) {
		t.Fatalf("enqueued = %+v", d)
	}

	next, err := s.NextQueuedDeployment(ctx)
	if err != nil || next.ID != d.ID {
		t.Fatalf("NextQueuedDeployment = %+v, %v", next, err)
	}
	started := testNow.Add(time.Second)
	if err := s.StartDeployment(ctx, d.ID, started); err != nil {
		t.Fatalf("StartDeployment: %v", err)
	}
	if _, err := s.NextQueuedDeployment(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("queue should be empty, err = %v", err)
	}
	if err := s.SetDeploymentCommit(ctx, d.ID, "aaa-full", "Initial commit"); err != nil {
		t.Fatalf("SetDeploymentCommit: %v", err)
	}
	finished := testNow.Add(time.Minute)
	if err := s.FinishDeployment(ctx, d.ID, StatusSucceeded, "", finished); err != nil {
		t.Fatalf("FinishDeployment: %v", err)
	}

	got, err := s.GetDeployment(ctx, d.ID)
	if err != nil {
		t.Fatalf("GetDeployment: %v", err)
	}
	if got.Status != StatusSucceeded || got.CommitSHA != "aaa-full" || got.CommitMessage != "Initial commit" {
		t.Errorf("deployment = %+v", got)
	}
	if got.StartedAt == nil || !got.StartedAt.Equal(started) || got.FinishedAt == nil || !got.FinishedAt.Equal(finished) {
		t.Errorf("timestamps = %+v", got)
	}
	updated, _ := s.GetApp(ctx, "blog")
	if updated.CurrentDeploymentID == nil || *updated.CurrentDeploymentID != d.ID {
		t.Errorf("current deployment not updated: %+v", updated.CurrentDeploymentID)
	}
}

func TestFailedDeploymentKeepsCurrent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")

	good := enqueue(t, s, app.ID, "good")
	_ = s.StartDeployment(ctx, good.ID, testNow)
	_ = s.FinishDeployment(ctx, good.ID, StatusSucceeded, "", testNow)

	bad := enqueue(t, s, app.ID, "bad")
	_ = s.StartDeployment(ctx, bad.ID, testNow)
	if err := s.FinishDeployment(ctx, bad.ID, StatusFailed, "health check failed", testNow); err != nil {
		t.Fatalf("FinishDeployment: %v", err)
	}
	got, _ := s.GetDeployment(ctx, bad.ID)
	if got.Status != StatusFailed || got.Error != "health check failed" {
		t.Errorf("deployment = %+v", got)
	}
	current, _ := s.GetApp(ctx, "blog")
	if current.CurrentDeploymentID == nil || *current.CurrentDeploymentID != good.ID {
		t.Errorf("current deployment must stay on the last success")
	}
	succeeded, err := s.SucceededDeployments(ctx, app.ID, 5)
	if err != nil || len(succeeded) != 1 || succeeded[0].ID != good.ID {
		t.Errorf("SucceededDeployments = %+v, %v", succeeded, err)
	}
}

func TestInvalidTransitionsAreRejected(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	d := enqueue(t, s, app.ID, "aaa")

	if err := s.FinishDeployment(ctx, d.ID, StatusSucceeded, "", testNow); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("finishing a queued deployment: err = %v", err)
	}
	if err := s.FinishDeployment(ctx, d.ID, StatusQueued, "", testNow); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("finishing as queued: err = %v", err)
	}
	_ = s.StartDeployment(ctx, d.ID, testNow)
	if err := s.StartDeployment(ctx, d.ID, testNow); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("starting twice: err = %v", err)
	}
	if err := s.CancelQueuedDeployment(ctx, d.ID, testNow); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("canceling a running deployment as queued: err = %v", err)
	}
	if err := s.FinishDeployment(ctx, d.ID, StatusCanceled, "canceled", testNow); err != nil {
		t.Fatalf("canceling a running deployment: %v", err)
	}
	if err := s.FinishDeployment(ctx, d.ID, StatusFailed, "", testNow); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("finishing twice: err = %v", err)
	}
	if app, _ := s.GetApp(ctx, "blog"); app.CurrentDeploymentID != nil {
		t.Error("a canceled deployment must not become current")
	}
}

func TestEnqueueSupersedesOlderQueued(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "other")

	running := enqueue(t, s, app.ID, "running")
	_ = s.StartDeployment(ctx, running.ID, testNow)
	waiting := enqueue(t, s, app.ID, "waiting")
	otherQueued := enqueue(t, s, other.ID, "other")

	newest, superseded, err := s.EnqueueDeployment(ctx, app.ID, "main", "newest", "", testNow)
	if err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}
	if len(superseded) != 1 || superseded[0] != waiting.ID {
		t.Errorf("superseded = %v, want [%d]", superseded, waiting.ID)
	}
	if got, _ := s.GetDeployment(ctx, waiting.ID); got.Status != StatusCanceled || got.Error != SupersededError || got.FinishedAt == nil {
		t.Errorf("superseded deployment = %+v", got)
	}
	if got, _ := s.GetDeployment(ctx, running.ID); got.Status != StatusRunning {
		t.Errorf("the running deployment must not be touched: %+v", got)
	}
	if got, _ := s.GetDeployment(ctx, otherQueued.ID); got.Status != StatusQueued {
		t.Errorf("another app's queue must not be touched: %+v", got)
	}

	// The queue is global and first-in first-out.
	next, _ := s.NextQueuedDeployment(ctx)
	if next.ID != otherQueued.ID {
		t.Errorf("next = %d, want %d", next.ID, otherQueued.ID)
	}
	list, err := s.ListDeployments(ctx, app.ID, 10)
	if err != nil || len(list) != 3 || list[0].ID != newest.ID {
		t.Errorf("ListDeployments = %+v, %v", list, err)
	}
	if limited, _ := s.ListDeployments(ctx, app.ID, 1); len(limited) != 1 {
		t.Errorf("limit ignored: %d rows", len(limited))
	}
}

func TestCancelQueuedDeployment(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	d := enqueue(t, s, app.ID, "aaa")

	if err := s.CancelQueuedDeployment(ctx, d.ID, testNow); err != nil {
		t.Fatalf("CancelQueuedDeployment: %v", err)
	}
	if got, _ := s.GetDeployment(ctx, d.ID); got.Status != StatusCanceled {
		t.Errorf("status = %q", got.Status)
	}
	if err := s.StartDeployment(ctx, d.ID, testNow); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("a canceled deployment must not start: %v", err)
	}
}

func TestFailInterruptedDeployments(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "other")

	running := enqueue(t, s, app.ID, "running")
	_ = s.StartDeployment(ctx, running.ID, testNow)
	queued := enqueue(t, s, other.ID, "queued")

	ids, err := s.FailInterruptedDeployments(ctx, testNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("FailInterruptedDeployments: %v", err)
	}
	if len(ids) != 1 || ids[0] != running.ID {
		t.Errorf("ids = %v", ids)
	}
	if got, _ := s.GetDeployment(ctx, running.ID); got.Status != StatusFailed || got.Error != InterruptedError {
		t.Errorf("interrupted deployment = %+v", got)
	}
	if got, _ := s.GetDeployment(ctx, queued.ID); got.Status != StatusQueued {
		t.Errorf("queued deployments must survive a restart: %+v", got)
	}
	if ids, _ := s.FailInterruptedDeployments(ctx, testNow); len(ids) != 0 {
		t.Errorf("second call found %v", ids)
	}
}

func TestRollbackDeployment(t *testing.T) {
	ctx := context.Background()
	st := openTest(t)
	app := mustCreateApp(t, st, "blog")
	now := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	succeed := func(d Deployment) {
		t.Helper()
		if err := st.StartDeployment(ctx, d.ID, now); err != nil {
			t.Fatal(err)
		}
		if err := st.FinishDeployment(ctx, d.ID, StatusSucceeded, "", now); err != nil {
			t.Fatal(err)
		}
	}
	first, _, _ := st.EnqueueDeployment(ctx, app.ID, "main", strings.Repeat("a", 40), "First", now)
	succeed(first)
	second, _, _ := st.EnqueueDeployment(ctx, app.ID, "main", strings.Repeat("b", 40), "Second", now)
	succeed(second)

	queued, _, _ := st.EnqueueDeployment(ctx, app.ID, "main", strings.Repeat("c", 40), "Third", now)
	rb, superseded, err := st.EnqueueRollback(ctx, first, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(superseded) != 1 || superseded[0] != queued.ID {
		t.Errorf("superseded = %v, want the queued deployment", superseded)
	}
	if rb.Ref != RollbackRef || rb.CommitSHA != first.CommitSHA || rb.CommitMessage != "First" ||
		rb.SourceDeploymentID == nil || *rb.SourceDeploymentID != first.ID {
		t.Errorf("rollback = %+v", rb)
	}
	succeed(rb)
	got, _ := st.GetApp(ctx, "blog")
	if got.CurrentDeploymentID == nil || *got.CurrentDeploymentID != first.ID {
		t.Errorf("current deployment = %v, want the rolled back release %d", got.CurrentDeploymentID, first.ID)
	}
	again, _ := st.GetDeployment(ctx, rb.ID)
	if again.SourceDeploymentID == nil || *again.SourceDeploymentID != first.ID {
		t.Errorf("read back: %+v", again)
	}
}

func TestPromotionReferences(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	staging := mustCreateApp(t, s, "shop-staging")
	prod := mustCreateApp(t, s, "shop")

	prod.PromoteFromAppID = &staging.ID
	if err := s.UpdateApp(ctx, prod, testNow); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	got, err := s.GetApp(ctx, "shop")
	if err != nil || got.PromoteFromAppID == nil || *got.PromoteFromAppID != staging.ID {
		t.Fatalf("PromoteFromAppID = %v, %v", got.PromoteFromAppID, err)
	}

	source := enqueue(t, s, staging.ID, "aaa")
	d, _, err := s.EnqueuePromotion(ctx, prod.ID, "", "aaa", "msg", &source.ID, testNow)
	if err != nil {
		t.Fatalf("EnqueuePromotion: %v", err)
	}
	if d.PromotedFromID == nil || *d.PromotedFromID != source.ID {
		t.Fatalf("PromotedFromID = %v", d.PromotedFromID)
	}
	if plain := enqueue(t, s, staging.ID, "bbb"); plain.PromotedFromID != nil {
		t.Errorf("ordinary deployment has PromotedFromID %v", *plain.PromotedFromID)
	}

	// A rollback to the promoted release is a rollback, not a promotion.
	rb, _, err := s.EnqueueRollback(ctx, d, testNow)
	if err != nil {
		t.Fatalf("EnqueueRollback: %v", err)
	}
	if rb.PromotedFromID != nil || rb.SourceDeploymentID == nil || *rb.SourceDeploymentID != d.ID {
		t.Errorf("rollback of a promotion = %+v", rb)
	}

	// Deleting the staging app removes the pairing and the reference.
	if err := s.DeleteApp(ctx, staging.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	got, err = s.GetApp(ctx, "shop")
	if err != nil || got.PromoteFromAppID != nil {
		t.Errorf("after delete PromoteFromAppID = %v, %v", got.PromoteFromAppID, err)
	}
	after, err := s.GetDeployment(ctx, d.ID)
	if err != nil || after.PromotedFromID != nil {
		t.Errorf("after delete PromotedFromID = %v, %v", after.PromotedFromID, err)
	}
}
