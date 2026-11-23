package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 18, 3, 0, 0, 0, time.UTC)

func newApp(slug string) App {
	return App{
		Slug:            slug,
		RepoFullName:    "octo/" + slug,
		DefaultBranch:   "main",
		WebService:      "web",
		WebPort:         3000,
		HealthcheckPath: "/",
	}
}

func mustCreateApp(t *testing.T, s *Store, slug string) App {
	t.Helper()
	app, err := s.CreateApp(context.Background(), newApp(slug), 20000, 20999, testNow)
	if err != nil {
		t.Fatalf("CreateApp(%s): %v", slug, err)
	}
	return app
}

func TestCreateAndGetApp(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	in := newApp("blog")
	in.ServiceMemoryLimits = map[string]string{"db": "1g"}
	in.Environment = EnvironmentProduction
	created, err := s.CreateApp(ctx, in, 20000, 20999, testNow)
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if created.ID == 0 || created.HostPort != 20000 {
		t.Errorf("created = %+v", created)
	}
	if created.DesiredState != DesiredRunning || created.Environment != EnvironmentProduction {
		t.Errorf("defaults not applied: %+v", created)
	}
	if !created.CreatedAt.Equal(testNow) || created.CurrentDeploymentID != nil {
		t.Errorf("unexpected metadata: %+v", created)
	}

	got, err := s.GetApp(ctx, "blog")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.ServiceMemoryLimits["db"] != "1g" || got.RepoFullName != "octo/blog" {
		t.Errorf("GetApp = %+v", got)
	}
	if got.CloneURL() != "https://github.com/octo/blog.git" || got.ProjectName() != "koroku-blog" {
		t.Errorf("CloneURL = %q, ProjectName = %q", got.CloneURL(), got.ProjectName())
	}
	byID, err := s.GetAppByID(ctx, created.ID)
	if err != nil || byID.Slug != "blog" {
		t.Errorf("GetAppByID = %+v, %v", byID, err)
	}
	if _, err := s.GetApp(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetApp(missing) err = %v", err)
	}
}

// The ID of an app's repository is recorded once: the first reading fills
// an empty repo_id and no later one replaces it (ARCHITECTURE 5).
func TestRecordAppRepoIDFillsOnlyAnEmptyID(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	if app.RepoID != nil {
		t.Fatalf("a new app has repo_id %d", *app.RepoID)
	}

	if got, err := s.RecordAppRepoID(ctx, app.ID, 1296269); err != nil || got != 1296269 {
		t.Fatalf("first recording = %d, %v", got, err)
	}
	if got, err := s.RecordAppRepoID(ctx, app.ID, 555); err != nil || got != 1296269 {
		t.Errorf("second recording = %d, %v; want the first ID kept", got, err)
	}
	stored, err := s.GetApp(ctx, "blog")
	if err != nil || stored.RepoID == nil || *stored.RepoID != 1296269 {
		t.Errorf("stored repo_id = %v, %v", stored.RepoID, err)
	}
	// Nothing else of the app changes, the time of its last edit included.
	if !stored.UpdatedAt.Equal(app.UpdatedAt) || stored.RepoFullName != app.RepoFullName {
		t.Errorf("the recording changed the app: %+v", stored)
	}

	// An ID set with the app is left alone as well.
	picked := newApp("shop")
	id := int64(42)
	picked.RepoID = &id
	created, err := s.CreateApp(ctx, picked, 20000, 20999, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.RecordAppRepoID(ctx, created.ID, 43); err != nil || got != 42 {
		t.Errorf("recording over a picked ID = %d, %v", got, err)
	}

	if _, err := s.RecordAppRepoID(ctx, 9999, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing app: err = %v", err)
	}
}

func TestCloneURLPrefersExplicitURL(t *testing.T) {
	a := App{RepoFullName: "octo/blog", RepoURL: "https://example.com/git/blog.git"}
	if a.CloneURL() != "https://example.com/git/blog.git" {
		t.Errorf("CloneURL = %q", a.CloneURL())
	}
	if (App{}).CloneURL() != "" {
		t.Error("an app without a repository has no clone URL")
	}
}

func TestCreateAppRejectsDuplicateSlug(t *testing.T) {
	s := openTest(t)
	mustCreateApp(t, s, "blog")
	_, err := s.CreateApp(context.Background(), newApp("blog"), 20000, 20999, testNow)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestCreateAppWithEnv(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	var sealedFor int64
	created, err := s.CreateAppWithEnv(ctx, newApp("blog"), 20000, 20999, testNow, func(id int64) ([]EnvVar, error) {
		sealedFor = id
		return []EnvVar{{Key: "RAILS_ENV", ValueEncrypted: []byte("sealed-1")}, {Key: "SECRET_KEY_BASE", ValueEncrypted: []byte("sealed-2")}}, nil
	})
	if err != nil {
		t.Fatalf("CreateAppWithEnv: %v", err)
	}
	if sealedFor != created.ID {
		t.Errorf("env was sealed for app %d, created %d", sealedFor, created.ID)
	}
	vars, err := s.ListEnvVars(ctx, created.ID)
	if err != nil || len(vars) != 2 || vars[0].Key != "RAILS_ENV" || string(vars[1].ValueEncrypted) != "sealed-2" {
		t.Errorf("ListEnvVars = %+v, %v", vars, err)
	}

	// A failure leaves neither the app nor any variable behind.
	boom := errors.New("sealing failed")
	_, err = s.CreateAppWithEnv(ctx, newApp("shop"), 20000, 20999, testNow, func(int64) ([]EnvVar, error) {
		return nil, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the sealing error", err)
	}
	if _, err := s.GetApp(ctx, "shop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the app was created anyway: %v", err)
	}
	_, err = s.CreateAppWithEnv(ctx, newApp("shop"), 20000, 20999, testNow, func(int64) ([]EnvVar, error) {
		return []EnvVar{{Key: "A", ValueEncrypted: []byte("x")}, {Key: "A", ValueEncrypted: []byte("y")}}, nil
	})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate keys: err = %v, want ErrConflict", err)
	}
	if _, err := s.GetApp(ctx, "shop"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the app was created despite the duplicate: %v", err)
	}
}

func TestPortAllocation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	a := mustCreateApp(t, s, "a")
	b := mustCreateApp(t, s, "b")
	c := mustCreateApp(t, s, "c")
	if a.HostPort != 20000 || b.HostPort != 20001 || c.HostPort != 20002 {
		t.Fatalf("ports = %d, %d, %d", a.HostPort, b.HostPort, c.HostPort)
	}

	// A freed port is reused before the range grows.
	if err := s.DeleteApp(ctx, b.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	d := mustCreateApp(t, s, "d")
	if d.HostPort != 20001 {
		t.Errorf("reused port = %d, want 20001", d.HostPort)
	}
}

func TestPortAllocationExhausted(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, slug := range []string{"a", "b"} {
		if _, err := s.CreateApp(ctx, newApp(slug), 20000, 20001, testNow); err != nil {
			t.Fatalf("CreateApp(%s): %v", slug, err)
		}
	}
	_, err := s.CreateApp(ctx, newApp("c"), 20000, 20001, testNow)
	if !errors.Is(err, ErrNoFreePort) {
		t.Fatalf("err = %v, want ErrNoFreePort", err)
	}
	if _, err := s.GetApp(ctx, "c"); !errors.Is(err, ErrNotFound) {
		t.Error("a failed creation must not leave a row behind")
	}
}

func TestListAndUpdateApp(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	mustCreateApp(t, s, "zeta")
	app := mustCreateApp(t, s, "alpha")

	apps, err := s.ListApps(ctx)
	if err != nil {
		t.Fatalf("ListApps: %v", err)
	}
	if len(apps) != 2 || apps[0].Slug != "alpha" || apps[1].Slug != "zeta" {
		t.Errorf("ListApps order = %+v", apps)
	}

	later := testNow.Add(time.Hour)
	app.WebPort = 8080
	app.HealthcheckPath = "/up"
	app.RelaxPolicy = true
	app.MemoryLimit = "1g"
	app.ServiceMemoryLimits = map[string]string{"worker": "256m"}
	app.Slug = "renamed" // must be ignored
	app.HostPort = 1     // must be ignored
	if err := s.UpdateApp(ctx, app, later); err != nil {
		t.Fatalf("UpdateApp: %v", err)
	}
	got, err := s.GetApp(ctx, "alpha")
	if err != nil {
		t.Fatalf("GetApp: %v", err)
	}
	if got.WebPort != 8080 || got.HealthcheckPath != "/up" || !got.RelaxPolicy || got.MemoryLimit != "1g" {
		t.Errorf("update not applied: %+v", got)
	}
	if got.ServiceMemoryLimits["worker"] != "256m" || got.HostPort != 20001 || !got.UpdatedAt.Equal(later) {
		t.Errorf("unexpected fields: %+v", got)
	}

	if err := s.SetAppDesiredState(ctx, got.ID, DesiredStopped, later); err != nil {
		t.Fatalf("SetAppDesiredState: %v", err)
	}
	if got, _ := s.GetApp(ctx, "alpha"); got.DesiredState != DesiredStopped {
		t.Errorf("DesiredState = %q", got.DesiredState)
	}
	if err := s.UpdateApp(ctx, App{ID: 999}, later); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateApp(missing) err = %v", err)
	}
}

func TestDeleteAppCascades(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	if err := s.ReplaceEnvVars(ctx, app.ID, []EnvVar{{Key: "A", ValueEncrypted: []byte("x")}}); err != nil {
		t.Fatalf("ReplaceEnvVars: %v", err)
	}
	if _, _, err := s.EnqueueDeployment(ctx, app.ID, "main", "abc", "", testNow); err != nil {
		t.Fatalf("EnqueueDeployment: %v", err)
	}
	if err := s.DeleteApp(ctx, app.ID); err != nil {
		t.Fatalf("DeleteApp: %v", err)
	}
	for _, table := range []string{"env_vars", "deployments"} {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s rows = %d after deleting the app", table, n)
		}
	}
	if err := s.DeleteApp(ctx, app.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete err = %v", err)
	}
}

func TestReplaceEnvVars(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	app := mustCreateApp(t, s, "blog")
	other := mustCreateApp(t, s, "other")

	first := []EnvVar{{Key: "B", ValueEncrypted: []byte("2")}, {Key: "A", ValueEncrypted: []byte("1")}}
	if err := s.ReplaceEnvVars(ctx, app.ID, first); err != nil {
		t.Fatalf("ReplaceEnvVars: %v", err)
	}
	if err := s.ReplaceEnvVars(ctx, other.ID, []EnvVar{{Key: "A", ValueEncrypted: []byte("other")}}); err != nil {
		t.Fatalf("ReplaceEnvVars(other): %v", err)
	}
	vars, err := s.ListEnvVars(ctx, app.ID)
	if err != nil {
		t.Fatalf("ListEnvVars: %v", err)
	}
	if len(vars) != 2 || vars[0].Key != "A" || string(vars[1].ValueEncrypted) != "2" {
		t.Errorf("vars = %+v", vars)
	}

	if err := s.ReplaceEnvVars(ctx, app.ID, []EnvVar{{Key: "C", ValueEncrypted: []byte("3")}}); err != nil {
		t.Fatalf("second ReplaceEnvVars: %v", err)
	}
	vars, _ = s.ListEnvVars(ctx, app.ID)
	if len(vars) != 1 || vars[0].Key != "C" {
		t.Errorf("replacement must drop old keys: %+v", vars)
	}
	if vars, _ := s.ListEnvVars(ctx, other.ID); len(vars) != 1 {
		t.Errorf("other app's vars were touched: %+v", vars)
	}

	// A duplicate key fails as a whole and keeps the previous set.
	dup := []EnvVar{{Key: "X", ValueEncrypted: []byte("1")}, {Key: "X", ValueEncrypted: []byte("2")}}
	if err := s.ReplaceEnvVars(ctx, app.ID, dup); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate key err = %v", err)
	}
	vars, _ = s.ListEnvVars(ctx, app.ID)
	if len(vars) != 1 || vars[0].Key != "C" {
		t.Errorf("failed replacement must roll back: %+v", vars)
	}
}
