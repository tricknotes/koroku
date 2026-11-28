package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/store"
)

func TestStartDBRestore(t *testing.T) {
	h, copier, srv := newCopyServer(t)
	const id = "9fceb02d0ae598e95dc970b74767f19372d61af8"
	rec := request(t, h, http.MethodPost, "/api/apps/blog-staging/db-restores", `{"snapshot_id":"`+id+`"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	c := decode[dbCopyJSON](t, rec)
	if c.Kind != store.CopyKindRestore || c.SourceSnapshotID != id || c.SourceApp != "blog-staging" || c.TargetApp != "blog-staging" {
		t.Errorf("restore = %+v", c)
	}
	if len(copier.requests) != 1 || !copier.requests[0].Restore || copier.requests[0].SnapshotID != id {
		t.Errorf("requests = %+v", copier.requests)
	}
	entries, err := srv.store.ListActivity(context.Background(), copier.requests[0].Target.ID, 0, 10)
	if err != nil || len(entries) == 0 || entries[0].Action != actDBRestored {
		t.Errorf("activity = %+v, %v", entries, err)
	}

	// Production: the slug confirms it.
	for body, want := range map[string]int{
		`{"snapshot_id":"` + id + `"}`:                  http.StatusUnprocessableEntity,
		`{"snapshot_id":"` + id + `","confirm":"x"}`:    http.StatusUnprocessableEntity,
		`{"snapshot_id":"` + id + `","confirm":"blog"}`: http.StatusAccepted,
	} {
		if rec := request(t, h, http.MethodPost, "/api/apps/blog/db-restores", body); rec.Code != want {
			t.Errorf("production %s: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := request(t, h, http.MethodPost, "/api/apps/blog-staging/db-restores", `{"snapshot_id":"latest"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad ID: %d %s", rec.Code, rec.Body)
	}
}

func TestStartDBRestoreRefusals(t *testing.T) {
	h, copier, _ := newCopyServer(t)
	for err, code := range map[error]string{
		deploy.ErrDumpNotFound:         "snapshot_not_found",
		deploy.ErrDumpOfOtherService:   "snapshot_of_other_service",
		deploy.ErrRestoreNeedsDumpHook: "dump_hook_missing",
		deploy.ErrNoLoadHook:           "target_load_hook_missing",
		deploy.ErrBusy:                 "busy",
	} {
		copier.err = err
		rec := request(t, h, http.MethodPost, "/api/apps/blog-staging/db-restores", `{"snapshot_id":"4bba301e5d2c7a90"}`)
		if errorCode(t, rec) != code {
			t.Errorf("%v: %d %s", err, rec.Code, rec.Body)
		}
	}
}

func TestDumpIntervalSettingAndWarnings(t *testing.T) {
	srv := newTestServer(t, func(o *Options) { o.Backups = &fakeBackups{store: o.Store} })
	h := srv.Handler()
	createApp(t, h, "blog-staging")
	ctx := context.Background()
	if app := decode[appJSON](t, request(t, h, http.MethodGet, "/api/apps/blog-staging", "")); app.DumpIntervalHours != 24 {
		t.Errorf("default interval = %d", app.DumpIntervalHours)
	}
	if rec := request(t, h, http.MethodPatch, "/api/apps/blog-staging", `{"dump_interval_hours":5}`); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "dump_interval_hours") {
		t.Errorf("interval 5: %d %s", rec.Code, rec.Body)
	}
	rec := request(t, h, http.MethodPatch, "/api/apps/blog-staging",
		`{"dump_interval_hours":1,"dump_hook_service":"db","dump_hook_command":"pg_dump app"}`)
	if rec.Code != http.StatusOK || decode[appJSON](t, rec).DumpIntervalHours != 1 {
		t.Fatalf("interval 1: %d %s", rec.Code, rec.Body)
	}

	app, err := srv.store.GetApp(ctx, "blog-staging")
	if err != nil {
		t.Fatal(err)
	}
	app.DesiredState = store.DesiredRunning
	if err := srv.store.UpdateApp(ctx, app, srv.clock.Now()); err != nil {
		t.Fatal(err)
	}
	status := func() map[string]any {
		t.Helper()
		return decode[map[string]any](t, request(t, h, http.MethodGet, "/api/apps/blog-staging/backups/status", ""))
	}
	run := func(at time.Time, target store.BackupTarget) {
		t.Helper()
		r, err := srv.store.StartBackupRun(ctx, store.BackupTriggerInterval, at)
		if err != nil {
			t.Fatal(err)
		}
		target.App = "blog-staging"
		if err := srv.store.FinishBackupRunWithTargets(ctx, r.ID, target.Status == store.BackupTargetSucceeded, "", at, []store.BackupTarget{target}); err != nil {
			t.Fatal(err)
		}
	}
	// No dump yet: nothing to be late with.
	if st := status(); st["dump_warning"] != false || st["dump_slow"] != false {
		t.Errorf("before any dump: %v", st)
	}
	now := srv.clock.Now()
	run(now.Add(-3*time.Hour), store.BackupTarget{Kind: "dump", Target: "db", Status: store.BackupTargetSucceeded, Duration: 40 * time.Minute})
	run(now.Add(-time.Hour), store.BackupTarget{Kind: "dump", Target: "db", Status: store.BackupTargetFailed, Error: "exit 1"})
	st := status()
	if st["dump_warning"] != true {
		t.Errorf("three hours since the last stored hourly dump: %v", st)
	}
	if st["dump_slow"] != false {
		t.Errorf("the last dump was not measured: %v", st)
	}
	run(now, store.BackupTarget{Kind: "dump", Target: "db", Status: store.BackupTargetSucceeded, Duration: 31 * time.Minute})
	if st := status(); st["dump_warning"] != false || st["dump_slow"] != true {
		t.Errorf("a stored dump that took 31 minutes of an hour: %v", st)
	}
	lastRun, _ := status()["last_run"].(map[string]any)
	targets, _ := lastRun["targets"].([]any)
	if len(targets) != 1 || targets[0].(map[string]any)["duration_ms"] != float64(31*60*1000) {
		t.Errorf("last_run = %v", lastRun)
	}
}
