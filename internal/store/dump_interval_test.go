package store

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// Migration 34 rebuilds backup_runs and backup_run_targets. The runs and
// targets recorded before survive it, and the targets still go with their
// run.
func TestDumpIntervalMigrationKeepsTheBackupHistory(t *testing.T) {
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(ON)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	s := &Store{db: db}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	var later []migration
	for _, m := range migrations {
		if m.version >= 34 {
			later = append(later, m)
			continue
		}
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("migration %s: %v", m.name, err)
		}
	}

	// The apps table gains a column in this migration too, so the target
	// recorded before it is the system's.
	run, err := s.StartBackupRun(ctx, BackupTriggerScheduled, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO backup_run_targets (run_id, app_id, kind, target, status, error) VALUES (?, NULL, 'system', '', 'failed', 'exit 1')`,
		run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE backup_runs SET status = 'failed', finished_at = ?, targets_recorded = 1 WHERE id = ?`,
		toMillis(testNow.Add(time.Minute)), run.ID); err != nil {
		t.Fatal(err)
	}

	for _, m := range later {
		if err := s.applyMigration(ctx, m); err != nil {
			t.Fatalf("migration %s: %v", m.name, err)
		}
	}

	got, err := s.LatestSystemBackupOutcome(ctx)
	if err != nil || got == nil || got.Run.ID != run.ID || len(got.Targets) != 1 || got.Targets[0].Error != "exit 1" {
		t.Fatalf("outcome after the migration = %+v, %v", got, err)
	}
	var parent string
	if err := db.QueryRowContext(ctx, `SELECT "table" FROM pragma_foreign_key_list('backup_run_targets') WHERE "from" = 'run_id'`).Scan(&parent); err != nil || parent != "backup_runs" {
		t.Errorf("backup_run_targets.run_id references %q, %v", parent, err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM backup_runs WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_run_targets`).Scan(&left); err != nil || left != 0 {
		t.Errorf("targets left after their run was deleted: %d, %v", left, err)
	}
}

func TestDumpIntervalSetting(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	if app.DumpIntervalHours != DefaultDumpIntervalHours {
		t.Fatalf("default interval = %d", app.DumpIntervalHours)
	}
	app.DumpIntervalHours = 6
	if err := s.UpdateApp(ctx, app, testNow); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetApp(ctx, "blog"); got.DumpIntervalHours != 6 {
		t.Errorf("interval = %d, want 6", got.DumpIntervalHours)
	}
	app.DumpIntervalHours = 5
	if err := s.UpdateApp(ctx, app, testNow); err == nil {
		t.Error("an interval that does not divide 24 was stored")
	}
}

// An interval run takes dumps only. It does not become "the" latest run of
// the app, whose volumes it did not back up, but its dump results replace
// the older ones in the app's outcome (ARCHITECTURE 10.8).
func TestIntervalDumpsJoinTheAppOutcome(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	blog := mustCreateApp(t, s, "blog")
	run := func(trigger string, at time.Time, ok bool, targets ...BackupTarget) int64 {
		t.Helper()
		r, err := s.StartBackupRun(ctx, trigger, at)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishBackupRunWithTargets(ctx, r.ID, ok, "", at.Add(time.Minute), targets); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}

	// Only interval runs so far: their dump is the outcome.
	first := run(BackupTriggerInterval, testNow, true,
		BackupTarget{App: "blog", Kind: "dump", Target: "db", Status: BackupTargetSucceeded, Duration: 2 * time.Second})
	got, err := s.LatestAppBackupOutcome(ctx, blog.ID)
	if err != nil || got == nil || got.Run.ID != first || len(got.Targets) != 1 || got.Targets[0].Duration != 2*time.Second {
		t.Fatalf("outcome of an interval run only = %+v, %v", got, err)
	}

	daily := run(BackupTriggerScheduled, testNow.Add(time.Hour), false,
		BackupTarget{App: "blog", Kind: "volume", Target: "uploads", Status: BackupTargetFailed, Error: "disk"},
		BackupTarget{App: "blog", Kind: "dump", Target: "db", Status: BackupTargetSucceeded})
	later := testNow.Add(2 * time.Hour)
	run(BackupTriggerInterval, later, false,
		BackupTarget{App: "blog", Kind: "dump", Target: "db", Status: BackupTargetFailed, Error: "exit 1", Duration: time.Minute})

	got, err = s.LatestAppBackupOutcome(ctx, blog.ID)
	if err != nil || got == nil || got.Run.ID != daily || len(got.Targets) != 2 {
		t.Fatalf("outcome = %+v, %v", got, err)
	}
	for _, target := range got.Targets {
		switch target.Kind {
		case "volume":
			if target.Status != BackupTargetFailed || target.FinishedAt == nil || !target.FinishedAt.Equal(testNow.Add(time.Hour+time.Minute)) {
				t.Errorf("volume = %+v", target)
			}
		case "dump":
			if target.Status != BackupTargetFailed || target.Error != "exit 1" || !target.FinishedAt.Equal(later.Add(time.Minute)) {
				t.Errorf("dump = %+v, want the interval run's", target)
			}
		}
	}

	// The time of the last full success and the runs list leave interval
	// runs out; the dump's own times count them.
	if last, err := s.LastAppBackupSuccess(ctx, blog.ID); err != nil || last != nil {
		t.Errorf("LastAppBackupSuccess = %v, %v; the only full success is no interval run", last, err)
	}
	runs, err := s.ListBackupRuns(ctx, 10)
	if err != nil || len(runs) != 1 || runs[0].ID != daily {
		t.Errorf("ListBackupRuns = %+v, %v", runs, err)
	}
	dump, err := s.LatestAppDump(ctx, blog.ID)
	if err != nil || dump == nil || dump.Status != BackupTargetFailed || dump.Duration != time.Minute {
		t.Errorf("LatestAppDump = %+v, %v", dump, err)
	}
	if ok, err := s.LastAppDumpSuccess(ctx, blog.ID); err != nil || ok == nil || !ok.Equal(testNow.Add(time.Hour+time.Minute)) {
		t.Errorf("LastAppDumpSuccess = %v, %v", ok, err)
	}
}
