package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/store"
)

type appBackupStatusResponse struct {
	Configured    bool              `json:"configured"`
	LastSuccessAt *time.Time        `json:"last_success_at"`
	Warning       bool              `json:"warning"`
	LastRun       *appBackupRunJSON `json:"last_run"`
}

type backupOverviewProblems struct {
	Problems     []backupProblemJSON `json:"problems"`
	SystemFailed bool                `json:"system_failed"`
}

// recordRun stores a finished run with its targets.
func recordRun(t *testing.T, st *store.Store, at time.Time, ok bool, msg string, targets ...store.BackupTarget) {
	t.Helper()
	ctx := context.Background()
	run, err := st.StartBackupRun(ctx, store.BackupTriggerScheduled, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishBackupRunWithTargets(ctx, run.ID, ok, msg, at.Add(time.Minute), targets); err != nil {
		t.Fatal(err)
	}
}

// An app's backups tab shows its own failures, not those of other apps, and
// the app list hears about every app with a problem the user may see.
func TestBackupProblemsStayWithTheirApp(t *testing.T) {
	f := newRoleFixture(t)
	f.srv.backups = &fakeBackups{store: f.srv.store}
	now := f.clk.Now()

	recordRun(t, f.srv.store, now.Add(-time.Hour), false, "shop dump db: exit 1 (from shop)",
		store.BackupTarget{Kind: "system", Status: store.BackupTargetFailed, Error: "VACUUM INTO failed"},
		store.BackupTarget{App: "blog", Kind: "volume", Target: "uploads", Status: store.BackupTargetSucceeded},
		store.BackupTarget{App: "shop", Kind: "dump", Target: "db", Status: store.BackupTargetFailed, Error: "exit 1 (from shop)"},
	)

	blog := decode[appBackupStatusResponse](t, f.as[viewerID].do(http.MethodGet, "/api/apps/blog/backups/status", "", nil))
	if blog.LastRun == nil || blog.LastRun.Status != "succeeded" || len(blog.LastRun.Targets) != 1 ||
		blog.LastRun.Targets[0].Target != "uploads" || blog.LastRun.Error != "" || blog.Warning || blog.LastSuccessAt == nil {
		t.Errorf("blog status = %+v", blog)
	}
	shop := decode[appBackupStatusResponse](t, f.owner.do(http.MethodGet, "/api/apps/shop/backups/status", "", nil))
	if shop.LastRun == nil || shop.LastRun.Status != "failed" || len(shop.LastRun.Targets) != 1 ||
		shop.LastRun.Targets[0].Error != "exit 1 (from shop)" || !shop.Warning {
		t.Errorf("shop status = %+v", shop)
	}

	owner := decode[backupOverviewProblems](t, f.owner.do(http.MethodGet, "/api/backups", "", nil))
	if len(owner.Problems) != 1 || owner.Problems[0].App != "shop" || owner.Problems[0].Status != "failed" ||
		owner.Problems[0].WholeRun || !owner.SystemFailed {
		t.Errorf("owner overview = %+v", owner)
	}
	// A member of blog only hears about blog, and not about the system.
	viewer := decode[backupOverviewProblems](t, f.as[viewerID].do(http.MethodGet, "/api/backups", "", nil))
	if len(viewer.Problems) != 0 || viewer.SystemFailed {
		t.Errorf("viewer overview = %+v", viewer)
	}
	stranger := decode[backupOverviewProblems](t, f.as[strangerID].do(http.MethodGet, "/api/backups", "", nil))
	if len(stranger.Problems) != 1 || stranger.Problems[0].App != "shop" {
		t.Errorf("shop member overview = %+v", stranger)
	}

	// A run that failed before backing anything up concerns every app; only
	// the owner reads its message.
	recordRun(t, f.srv.store, now, false, "open the repository: unreachable")
	blog = decode[appBackupStatusResponse](t, f.as[viewerID].do(http.MethodGet, "/api/apps/blog/backups/status", "", nil))
	if blog.LastRun == nil || blog.LastRun.Status != "failed" || !blog.LastRun.WholeRun || blog.LastRun.Error != "" {
		t.Errorf("blog status after a failed run = %+v", blog.LastRun)
	}
	blogOwner := decode[appBackupStatusResponse](t, f.owner.do(http.MethodGet, "/api/apps/blog/backups/status", "", nil))
	if blogOwner.LastRun == nil || blogOwner.LastRun.Error != "open the repository: unreachable" {
		t.Errorf("owner's blog status = %+v", blogOwner.LastRun)
	}
	owner = decode[backupOverviewProblems](t, f.owner.do(http.MethodGet, "/api/backups", "", nil))
	if len(owner.Problems) != 2 || !owner.Problems[0].WholeRun || !owner.Problems[1].WholeRun {
		t.Errorf("owner overview after a failed run = %+v", owner)
	}
}

func TestAppBackupStatusOfANewApp(t *testing.T) {
	f := newRoleFixture(t)
	f.srv.backups = &fakeBackups{store: f.srv.store}
	got := decode[appBackupStatusResponse](t, f.owner.do(http.MethodGet, "/api/apps/blog/backups/status", "", nil))
	if !got.Configured || got.LastRun != nil || got.Warning || got.LastSuccessAt != nil {
		t.Errorf("status = %+v", got)
	}
}
