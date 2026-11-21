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

func TestBackupOutcomesPerApp(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	blog, err := s.CreateApp(ctx, newApp("blog"), 20000, 20999, testNow)
	if err != nil {
		t.Fatal(err)
	}
	shop, err := s.CreateApp(ctx, newApp("shop"), 20000, 20999, testNow)
	if err != nil {
		t.Fatal(err)
	}
	run := func(at time.Time, ok bool, msg string, targets ...BackupTarget) int64 {
		t.Helper()
		r, err := s.StartBackupRun(ctx, BackupTriggerScheduled, at)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishBackupRunWithTargets(ctx, r.ID, ok, msg, at.Add(time.Minute), targets); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}

	// A run recorded the old way says nothing per app.
	old, _ := s.StartBackupRun(ctx, BackupTriggerScheduled, testNow)
	_ = s.FinishBackupRun(ctx, old.ID, false, "old failure", testNow.Add(time.Minute))
	if got, err := s.LatestAppBackupOutcome(ctx, blog.ID); err != nil || got != nil {
		t.Fatalf("before any recorded run: %+v, %v", got, err)
	}

	// shop's dump fails; blog is fine and does not see shop's error.
	first := run(testNow.Add(time.Hour), false, "shop dump db: exit 1",
		BackupTarget{Kind: "system", Status: BackupTargetSucceeded},
		BackupTarget{App: "blog", Kind: "volume", Target: "uploads", Status: BackupTargetWarning, Error: "1 file unreadable"},
		BackupTarget{App: "shop", Kind: "dump", Target: "db", Status: BackupTargetFailed, Error: "exit 1"},
		BackupTarget{App: "gone", Kind: "dump", Target: "db", Status: BackupTargetFailed, Error: "deleted app"},
	)
	got, err := s.LatestAppBackupOutcome(ctx, blog.ID)
	if err != nil || got == nil || got.Run.ID != first || len(got.Targets) != 1 ||
		got.Targets[0].Status != BackupTargetWarning || got.Targets[0].Target != "uploads" {
		t.Fatalf("blog = %+v, %v", got, err)
	}
	got, _ = s.LatestAppBackupOutcome(ctx, shop.ID)
	if got == nil || len(got.Targets) != 1 || got.Targets[0].Status != BackupTargetFailed || got.Targets[0].Error != "exit 1" {
		t.Fatalf("shop = %+v", got)
	}
	if last, _ := s.LastAppBackupSuccess(ctx, blog.ID); last == nil {
		t.Error("a warning still counts as stored")
	}
	if last, _ := s.LastAppBackupSuccess(ctx, shop.ID); last != nil {
		t.Errorf("shop never succeeded, got %v", last)
	}
	sys, _ := s.LatestSystemBackupOutcome(ctx)
	if sys == nil || sys.Run.ID != first || len(sys.Targets) != 1 || sys.Targets[0].Kind != "system" {
		t.Fatalf("system = %+v", sys)
	}

	// A run that backs up only blog (before a deployment) is blog's latest,
	// not shop's.
	pre := run(testNow.Add(2*time.Hour), true, "",
		BackupTarget{App: "blog", Kind: "volume", Target: "uploads", Status: BackupTargetSucceeded})
	if got, _ := s.LatestAppBackupOutcome(ctx, blog.ID); got == nil || got.Run.ID != pre {
		t.Errorf("blog latest = %+v", got)
	}
	if got, _ := s.LatestAppBackupOutcome(ctx, shop.ID); got == nil || got.Run.ID != first {
		t.Errorf("shop latest = %+v", got)
	}

	// A run that failed before backing anything up concerns every app.
	whole := run(testNow.Add(3*time.Hour), false, "open the repository: unreachable")
	for _, app := range []App{blog, shop} {
		got, _ := s.LatestAppBackupOutcome(ctx, app.ID)
		if got == nil || got.Run.ID != whole || len(got.Targets) != 0 || got.Run.Error == "" {
			t.Errorf("%s latest = %+v", app.Slug, got)
		}
	}
	if sys, _ := s.LatestSystemBackupOutcome(ctx); sys == nil || sys.Run.ID != whole {
		t.Errorf("system latest = %+v", sys)
	}

	// Deleting an app takes its rows along.
	if err := s.DeleteApp(ctx, shop.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_run_targets WHERE app_id = ?`, shop.ID).Scan(&n)
	if n != 0 {
		t.Errorf("rows of a deleted app = %d", n)
	}
}
