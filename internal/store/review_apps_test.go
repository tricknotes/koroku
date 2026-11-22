package store

import (
	"context"
	"errors"
	"testing"
)

func newReview(t *testing.T, s *Store, parent App, slug string, pr int) App {
	t.Helper()
	app := newApp(slug)
	created, err := s.CreateReviewApp(context.Background(), NewReviewApp{
		App:    app,
		Review: ReviewApp{ParentAppID: parent.ID, PRNumber: pr, PRTitle: "Add search", HeadBranch: "search", DataCopy: DataCopyWaiting},
		Env: func(appID int64) ([]EnvVar, error) {
			return []EnvVar{{Key: "SECRET", ValueEncrypted: []byte{byte(appID)}}}, nil
		},
		Registry: func(appID int64) (*RegistryCredential, error) {
			return &RegistryCredential{Registry: "ghcr.io", Username: "bot", TokenEncrypted: []byte{byte(appID)}}, nil
		},
	}, 20000, 20999, testNow)
	if err != nil {
		t.Fatalf("CreateReviewApp(%s): %v", slug, err)
	}
	return created
}

func TestCreateReviewApp(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	parent := mustCreateApp(t, s, "shop-staging")
	review := newReview(t, s, parent, "shop-staging-pr-12", 12)

	got, err := s.GetReviewApp(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ParentAppID != parent.ID || got.PRNumber != 12 || got.PRState != PRStateOpen ||
		got.DataCopy != DataCopyWaiting || got.PRCheckedAt == nil || got.HeadBranch != "search" {
		t.Errorf("GetReviewApp = %+v", got)
	}
	vars, err := s.ListEnvVars(ctx, review.ID)
	if err != nil || len(vars) != 1 || vars[0].ValueEncrypted[0] != byte(review.ID) {
		t.Errorf("env vars = %+v, %v", vars, err)
	}
	cred, err := s.GetRegistryCredential(ctx, review.ID)
	if err != nil || cred.Username != "bot" || cred.TokenEncrypted[0] != byte(review.ID) {
		t.Errorf("registry credential = %+v, %v", cred, err)
	}
	if _, err := s.GetReviewApp(ctx, parent.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("parent is no review app: err = %v", err)
	}

	// One review app per pull request; the whole creation is undone.
	_, err = s.CreateReviewApp(ctx, NewReviewApp{
		App:    newApp("shop-staging-pr-12-2"),
		Review: ReviewApp{ParentAppID: parent.ID, PRNumber: 12},
	}, 20000, 20999, testNow)
	if !errors.Is(err, ErrReviewAppExists) {
		t.Errorf("second review app of the PR: err = %v", err)
	}
	if _, err := s.GetApp(ctx, "shop-staging-pr-12-2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("the refused app was kept: %v", err)
	}

	newReview(t, s, parent, "shop-staging-pr-15", 15)
	list, err := s.ListReviewApps(ctx, parent.ID)
	if err != nil || len(list) != 2 || list[0].PRNumber != 15 {
		t.Errorf("ListReviewApps = %+v, %v", list, err)
	}

	if err := s.SetReviewDataCopy(ctx, review.ID, DataCopyFailed, "the parent is stopped"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateReviewPullRequest(ctx, review.ID, "Add search v2", "search", PRStateMerged, testNow); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetReviewApp(ctx, review.ID)
	if got.DataCopy != DataCopyFailed || got.DataCopyDetail != "the parent is stopped" ||
		got.PRState != PRStateMerged || got.PRTitle != "Add search v2" {
		t.Errorf("after updates = %+v", got)
	}

	// The parent cannot go while it has review apps; the review app can.
	if err := s.DeleteApp(ctx, parent.ID); err == nil {
		t.Error("deleting the parent of review apps succeeded")
	}
	if err := s.DeleteApp(ctx, review.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetReviewApp(ctx, review.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("review row after deleting the app: %v", err)
	}
}

func TestSummarizeReviewApps(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	parent := mustCreateApp(t, s, "shop-staging")
	if got, err := s.SummarizeReviewApps(ctx, parent.ID); err != nil || got != (ReviewSummary{}) {
		t.Fatalf("no review apps: %+v, %v", got, err)
	}

	// Deployed fine.
	ok := newReview(t, s, parent, "shop-staging-pr-1", 1)
	d, _, err := s.EnqueueDeployment(ctx, ok.ID, "search", "a", "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.StartDeployment(ctx, d.ID, testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDeployment(ctx, d.ID, StatusSucceeded, "", testNow); err != nil {
		t.Fatal(err)
	}
	// The latest deployment failed.
	broken := newReview(t, s, parent, "shop-staging-pr-2", 2)
	d, _, _ = s.EnqueueDeployment(ctx, broken.ID, "b", "b", "", testNow)
	_ = s.StartDeployment(ctx, d.ID, testNow)
	if err := s.FinishDeployment(ctx, d.ID, StatusFailed, "boom", testNow); err != nil {
		t.Fatal(err)
	}
	// Still deploying.
	queued := newReview(t, s, parent, "shop-staging-pr-3", 3)
	if _, _, err := s.EnqueueDeployment(ctx, queued.ID, "c", "c", "", testNow); err != nil {
		t.Fatal(err)
	}
	// The copy of the parent's data could not start.
	noCopy := newReview(t, s, parent, "shop-staging-pr-4", 4)
	if err := s.SetReviewDataCopy(ctx, noCopy.ID, DataCopyFailed, "source_not_running"); err != nil {
		t.Fatal(err)
	}
	// The copy ran and failed, then a copy from elsewhere succeeded: only
	// copies from the parent count.
	copyFailed := newReview(t, s, parent, "shop-staging-pr-5", 5)
	c, err := s.StartDBCopy(ctx, copyFailed, parent, CopySourceLive, testNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.FinishDBCopy(ctx, c.ID, false, "load failed", 0, testNow); err != nil {
		t.Fatal(err)
	}
	if status, err := s.LatestCopyStatus(ctx, copyFailed.ID, parent.ID); err != nil || status != CopyFailed {
		t.Errorf("LatestCopyStatus = %q, %v", status, err)
	}
	if status, err := s.LatestCopyStatus(ctx, ok.ID, parent.ID); err != nil || status != "" {
		t.Errorf("LatestCopyStatus without copies = %q, %v", status, err)
	}

	// Another parent's review apps are not counted.
	other := mustCreateApp(t, s, "blog-staging")
	newReview(t, s, other, "blog-staging-pr-1", 1)

	got, err := s.SummarizeReviewApps(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := (ReviewSummary{Count: 5, Failed: 3, Deploying: 1}); got != want {
		t.Errorf("SummarizeReviewApps = %+v, want %+v", got, want)
	}
}

func TestReviewAppsAllowedMigration(t *testing.T) {
	ctx := context.Background()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	parent := mustCreateApp(t, s, "shop-staging")
	plain := mustCreateApp(t, s, "blog-staging")
	newReview(t, s, parent, "shop-staging-pr-1", 1)
	// Roll back to before migration 27 with the review app in place.
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE apps DROP COLUMN review_apps_allowed;
		DELETE FROM schema_migrations WHERE version = 27`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		if m.version == 27 {
			if err := s.applyMigration(ctx, m); err != nil {
				t.Fatalf("apply %s: %v", m.name, err)
			}
		}
	}
	for slug, want := range map[string]bool{"shop-staging": true, "blog-staging": false, "shop-staging-pr-1": false} {
		app, err := s.GetApp(ctx, slug)
		if err != nil {
			t.Fatal(err)
		}
		if app.ReviewAppsAllowed != want {
			t.Errorf("%s: review apps allowed = %v, want %v", slug, app.ReviewAppsAllowed, want)
		}
	}

	// The setting is saved like the others.
	plain.ReviewAppsAllowed = true
	if err := s.UpdateApp(ctx, plain, testNow); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetApp(ctx, "blog-staging"); !got.ReviewAppsAllowed {
		t.Error("review_apps_allowed was not saved")
	}
}

func TestReviewAppRolesFollowTheParent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	parent := mustCreateApp(t, s, "shop-staging")
	other := mustCreateApp(t, s, "blog")
	review := newReview(t, s, parent, "shop-staging-pr-3", 3)
	if err := s.AddMember(ctx, Member{GitHubUserID: 7, Login: "mia", AddedAt: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAppRole(ctx, parent.ID, 7, RoleDeploy); err != nil {
		t.Fatal(err)
	}
	if role, err := s.AppRole(ctx, review.ID, 7); err != nil || role != RoleDeploy {
		t.Errorf("role on the review app = %q, %v", role, err)
	}
	if role, _ := s.AppRole(ctx, other.ID, 7); role != "" {
		t.Errorf("role on another app = %q", role)
	}
	roles, err := s.RolesOf(ctx, 7)
	if err != nil || len(roles) != 2 || roles[review.ID] != RoleDeploy || roles[parent.ID] != RoleDeploy {
		t.Errorf("RolesOf = %v, %v", roles, err)
	}
	if err := s.SetAppRole(ctx, review.ID, 7, RoleManage); !errors.Is(err, ErrRoleFollowsParent) {
		t.Errorf("setting a role on a review app: err = %v", err)
	}
	// Taking the parent's role away closes the review app at once.
	if err := s.SetAppRole(ctx, parent.ID, 7, ""); err != nil {
		t.Fatal(err)
	}
	if role, _ := s.AppRole(ctx, review.ID, 7); role != "" {
		t.Errorf("role on the review app after removal = %q", role)
	}
}
