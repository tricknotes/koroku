package store

import (
	"context"
	"testing"
	"time"
)

func TestBackupRuns(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if last, err := s.LastSuccessfulBackup(ctx); err != nil || last != nil {
		t.Fatalf("LastSuccessfulBackup on an empty table = %v, %v", last, err)
	}

	failed, err := s.StartBackupRun(ctx, BackupTriggerScheduled, testNow)
	if err != nil {
		t.Fatalf("StartBackupRun: %v", err)
	}
	if err := s.FinishBackupRun(ctx, failed.ID, false, "restic: repository unreachable", testNow.Add(time.Minute)); err != nil {
		t.Fatalf("FinishBackupRun: %v", err)
	}
	if err := s.FinishBackupRun(ctx, failed.ID, true, "", testNow); err == nil {
		t.Error("a finished run must not be finished again")
	}

	ok, _ := s.StartBackupRun(ctx, BackupTriggerManual, testNow.Add(time.Hour))
	done := testNow.Add(time.Hour + 5*time.Minute)
	if err := s.FinishBackupRun(ctx, ok.ID, true, "", done); err != nil {
		t.Fatal(err)
	}
	// A pre-deploy backup covers one app only and does not count as "the" backup.
	pre, _ := s.StartBackupRun(ctx, BackupTriggerPreDeploy, testNow.Add(2*time.Hour))
	_ = s.FinishBackupRun(ctx, pre.ID, true, "", testNow.Add(3*time.Hour))

	last, err := s.LastSuccessfulBackup(ctx)
	if err != nil || last == nil || !last.Equal(done) {
		t.Errorf("LastSuccessfulBackup = %v, %v, want %v", last, err, done)
	}

	stale, _ := s.StartBackupRun(ctx, BackupTriggerScheduled, testNow.Add(4*time.Hour))
	if err := s.FailInterruptedBackupRuns(ctx, testNow.Add(5*time.Hour)); err != nil {
		t.Fatalf("FailInterruptedBackupRuns: %v", err)
	}
	runs, err := s.ListBackupRuns(ctx, 10)
	if err != nil || len(runs) != 4 {
		t.Fatalf("ListBackupRuns = %d runs, %v", len(runs), err)
	}
	if runs[0].ID != stale.ID || runs[0].Status != BackupFailed || runs[0].FinishedAt == nil {
		t.Errorf("interrupted run = %+v", runs[0])
	}
	if runs[3].ID != failed.ID || runs[3].Error != "restic: repository unreachable" || runs[3].Trigger != BackupTriggerScheduled {
		t.Errorf("oldest run = %+v", runs[3])
	}
	if limited, _ := s.ListBackupRuns(ctx, 2); len(limited) != 2 {
		t.Errorf("limit ignored: %d", len(limited))
	}
	if _, err := s.StartBackupRun(ctx, "hourly", testNow); err == nil {
		t.Error("an unknown trigger must be rejected")
	}
}

func TestAppBackupSettings(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	if app.BackupExcludeVolumes == nil || len(app.BackupExcludeVolumes) != 0 || app.BackupBeforeDeploy {
		t.Errorf("defaults = %+v", app)
	}
	app.BackupExcludeVolumes = []string{"pgdata"}
	app.DumpHookService, app.DumpHookCommand = "db", "pg_dump -U postgres app"
	app.BackupBeforeDeploy = true
	if err := s.UpdateApp(ctx, app, testNow); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	got, _ := s.GetApp(ctx, "blog")
	if len(got.BackupExcludeVolumes) != 1 || got.BackupExcludeVolumes[0] != "pgdata" ||
		got.DumpHookService != "db" || got.DumpHookCommand != "pg_dump -U postgres app" || !got.BackupBeforeDeploy {
		t.Errorf("app = %+v", got)
	}
	active, err := s.HasActiveDeployment(ctx, app.ID)
	if err != nil || active {
		t.Errorf("HasActiveDeployment = %v, %v", active, err)
	}
	d := enqueue(t, s, app.ID, "aaa")
	if active, _ := s.HasActiveDeployment(ctx, app.ID); !active {
		t.Error("a queued deployment is active")
	}
	_ = s.StartDeployment(ctx, d.ID, testNow)
	if active, _ := s.HasActiveDeployment(ctx, app.ID); !active {
		t.Error("a running deployment is active")
	}
	_ = s.FinishDeployment(ctx, d.ID, StatusFailed, "x", testNow)
	if active, _ := s.HasActiveDeployment(ctx, app.ID); active {
		t.Error("a finished deployment is not active")
	}
}
