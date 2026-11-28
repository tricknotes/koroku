package pitr

import (
	"context"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/store"
)

func TestWALNames(t *testing.T) {
	for name, want := range map[string]bool{
		"000000010000000000000003":                 true,
		"00000002.history":                         true,
		"000000010000000000000003.00000028.backup": true,
		"000000010000000000000003.partial":         true,
		"000000010000000000000003.tmp":             false,
		"lost+found":                               false,
		"000000010000000000000003; rm -rf /":       false,
	} {
		if got := walName.MatchString(name); got != want {
			t.Errorf("walName(%q) = %v, want %v", name, got, want)
		}
	}
}

type fakeRepo struct{}

func (fakeRepo) BackupStream(context.Context, backup.Stream) (string, error) { return "", nil }

// The status reaches back to the oldest base backup kept, but not across
// a change of the major version, and shows how far the WAL was shipped.
func TestStatus(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	app, err := st.CreateApp(ctx, store.App{Slug: "blog", RepoFullName: "octo/blog", DefaultBranch: "main", WebService: "web", WebPort: 3000, HealthcheckPath: "/"}, 20000, 20999, now)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(Options{Store: st, Repo: fakeRepo{}, Clock: clock.NewFake(now)})

	if got, _ := svc.Status(ctx, app); got.State != StateOff {
		t.Errorf("off: %+v", got)
	}
	app.PITRService = "db"
	if got, _ := svc.Status(ctx, app); got.State != StateWaiting {
		t.Errorf("before the preparation: %+v", got)
	}
	if got, _ := New(Options{Store: st, Clock: clock.NewFake(now)}).Status(ctx, app); got.State != StateUnavailable {
		t.Errorf("without backups: %+v", got)
	}

	if err := st.PutPITRState(ctx, store.PITRState{AppID: app.ID, Service: "db", Status: store.PITRReady, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	base := func(ago time.Duration) {
		t.Helper()
		at := now.Add(-ago)
		if _, err := st.AddPITRBase(ctx, store.PITRBase{AppID: app.ID, Service: "db", SnapshotID: "s", StartedAt: at, FinishedAt: at, StartSegment: "000000010000000000000001", Timeline: 1}); err != nil {
			t.Fatal(err)
		}
	}
	base(10 * 24 * time.Hour) // past the snapshots' retention
	base(5 * 24 * time.Hour)
	base(24 * time.Hour)
	if err := st.AddPITRWALBatch(ctx, store.PITRWALBatch{AppID: app.ID, Service: "db", SnapshotID: "w", FirstSegment: "a", LastSegment: "b", FileCount: 2, LastModifiedAt: now.Add(-time.Hour), CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Status(ctx, app)
	if err != nil || got.State != StateReady || got.EarliestAt == nil || !got.EarliestAt.Equal(now.Add(-5*24*time.Hour)) || !got.ArchivedUntil.Equal(now.Add(-time.Hour)) {
		t.Fatalf("status = %+v, %v", got, err)
	}

	// A new major version two days ago: only the base backup since counts.
	if err := st.OpenPITRGap(ctx, app.ID, "db", store.PITRGapVersionChanged, now.Add(-9*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ClosePITRGaps(ctx, app.ID, store.PITRGapVersionChanged, now.Add(-2*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ = svc.Status(ctx, app)
	if got.EarliestAt == nil || !got.EarliestAt.Equal(now.Add(-24*time.Hour)) || len(got.Gaps) != 1 {
		t.Errorf("after a version change: %+v", got)
	}
}
