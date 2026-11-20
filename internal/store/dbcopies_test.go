package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLoadHookSettingsRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog-staging")
	app.LoadHookService, app.LoadHookCommand = "db", `pg_restore -d "$POSTGRES_DB"`
	app.PostLoadService, app.PostLoadCommand = "web", "bin/rails db:anonymize"
	if err := s.UpdateApp(ctx, app, testNow); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(ctx, "blog-staging")
	if err != nil {
		t.Fatal(err)
	}
	if got.LoadHookService != "db" || got.LoadHookCommand != app.LoadHookCommand ||
		got.PostLoadService != "web" || got.PostLoadCommand != "bin/rails db:anonymize" {
		t.Errorf("got %+v", got)
	}
}

func TestDBCopies(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	source := mustCreateApp(t, s, "blog")
	target := mustCreateApp(t, s, "blog-staging")

	first, err := s.StartDBCopy(ctx, target, source, CopySourceLive, testNow)
	if err != nil {
		t.Fatalf("StartDBCopy: %v", err)
	}
	if first.Status != CopyRunning || first.SourceSlug != "blog" || first.SourceAppID == nil ||
		*first.SourceAppID != source.ID || first.FinishedAt != nil {
		t.Errorf("started copy = %+v", first)
	}
	if err := s.SetDBCopyBackup(ctx, first.ID, "4bba301e"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDBCopy(ctx, first.ID, true, "", 1234, testNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDBCopy(ctx, first.ID, false, "again", 0, testNow); err == nil {
		t.Error("a finished copy must not be finished again")
	}
	second, _ := s.StartDBCopy(ctx, target, source, CopySourceSnapshot, testNow.Add(time.Hour))

	list, err := s.ListDBCopies(ctx, target.ID, 10)
	if err != nil || len(list) != 2 || list[0].ID != second.ID {
		t.Fatalf("ListDBCopies = %+v, %v", list, err)
	}
	done := list[1]
	if done.Status != CopySucceeded || done.Bytes != 1234 || done.BackupSnapshotID != "4bba301e" ||
		done.FinishedAt == nil || !done.FinishedAt.Equal(testNow.Add(time.Minute)) {
		t.Errorf("finished copy = %+v", done)
	}
	if other, _ := s.ListDBCopies(ctx, source.ID, 10); len(other) != 0 {
		t.Errorf("copies are listed by target, got %+v", other)
	}

	interrupted, err := s.FailInterruptedDBCopies(ctx, testNow.Add(2*time.Hour))
	if err != nil || len(interrupted) != 1 || interrupted[0].ID != second.ID {
		t.Fatalf("FailInterruptedDBCopies = %+v, %v", interrupted, err)
	}
	if got, _ := s.GetDBCopy(ctx, second.ID); got.Status != CopyFailed || got.Error != CopyInterruptedError {
		t.Errorf("interrupted copy = %+v", got)
	}

	// Deleting the source keeps the record with its name; deleting the
	// target removes the history.
	if err := s.DeleteApp(ctx, source.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetDBCopy(ctx, first.ID); got.SourceAppID != nil || got.SourceSlug != "blog" {
		t.Errorf("after deleting the source: %+v", got)
	}
	if err := s.DeleteApp(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDBCopy(ctx, first.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("after deleting the target: %v", err)
	}
}
