package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestAutoDeploySettingsRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	in := newApp("blog")
	in.AutoDeploy, in.AutoDeployBranch = true, "release"
	app, err := s.CreateApp(ctx, in, 20000, 20999, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if !app.AutoDeploy || app.EffectiveAutoDeployBranch() != "release" {
		t.Errorf("created = %+v", app)
	}
	app.AutoDeployBranch = ""
	if err := s.UpdateApp(ctx, app, testNow); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetApp(ctx, "blog")
	if err != nil {
		t.Fatal(err)
	}
	if !got.AutoDeploy || got.EffectiveAutoDeployBranch() != "main" {
		t.Errorf("updated = %+v", got)
	}
	got.AutoDeployWorkflow = ".github/workflows/image.yml"
	if err := s.UpdateApp(ctx, got, testNow); err != nil {
		t.Fatal(err)
	}
	if again, err := s.GetApp(ctx, "blog"); err != nil || again.AutoDeployWorkflow != ".github/workflows/image.yml" {
		t.Errorf("workflow = %q, %v", again.AutoDeployWorkflow, err)
	}
}

func TestDeploymentTrigger(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	manual, _, err := s.EnqueueDeployment(ctx, app.ID, "main", "aaa", "m", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if manual.Trigger != TriggerManual || manual.Pusher != "" {
		t.Errorf("manual = %+v", manual)
	}
	pushed, superseded, err := s.EnqueueTriggered(ctx, app.ID, "main", "bbb", "m", nil,
		Origin{Trigger: TriggerPush, Pusher: "octo"}, testNow)
	if err != nil {
		t.Fatal(err)
	}
	// A push supersedes a queued deployment like any other.
	if pushed.Trigger != TriggerPush || pushed.Pusher != "octo" || !slices.Equal(superseded, []int64{manual.ID}) {
		t.Errorf("pushed = %+v, superseded = %v", pushed, superseded)
	}
	if _, _, err := s.EnqueueTriggered(ctx, app.ID, "main", "ccc", "m", nil, Origin{Trigger: "cron"}, testNow); err == nil {
		t.Error("an unknown trigger was accepted")
	}
}

func TestAutoDeployAppsReadsWithoutChanging(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "koroku.db")
	if apps, err := AutoDeployApps(ctx, path); err != nil || apps != nil {
		t.Fatalf("missing database: %v %v", apps, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the database was created: %v", err)
	}

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	for _, slug := range []string{"shop", "blog", "wiki"} {
		in := newApp(slug)
		in.AutoDeploy = slug != "wiki"
		if slug != "blog" {
			in.AutoDeployWorkflow = ".github/workflows/image.yml"
		}
		if _, err := s.CreateApp(ctx, in, 20000, 20999, testNow); err != nil {
			t.Fatal(err)
		}
	}
	apps, err := AutoDeployApps(ctx, path)
	if err != nil || !slices.Equal(apps, []string{"blog", "shop"}) {
		t.Errorf("apps = %v, %v", apps, err)
	}
	// wiki names a workflow but does not deploy on push.
	gated, err := WorkflowGatedApps(ctx, path)
	if err != nil || !slices.Equal(gated, []string{"shop"}) {
		t.Errorf("gated = %v, %v", gated, err)
	}
	_ = s.Close()

	// A database from before the feature has no such apps.
	oldPath := filepath.Join(t.TempDir(), "old.db")
	old, err := Open(ctx, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.DB().ExecContext(ctx, `ALTER TABLE apps DROP COLUMN auto_deploy`); err != nil {
		t.Fatal(err)
	}
	_ = old.Close()
	if apps, err := AutoDeployApps(ctx, oldPath); err != nil || apps != nil {
		t.Errorf("old database: %v %v", apps, err)
	}
	if apps, err := WorkflowGatedApps(ctx, oldPath); err != nil || apps != nil {
		t.Errorf("old database, gated: %v %v", apps, err)
	}
}
