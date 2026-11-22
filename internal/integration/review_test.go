//go:build integration

package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/review"
	"github.com/tricknotes/koroku/internal/store"
)

// reviewApp is postgresApp with a web service that shows the variables
// koroku provides.
var reviewApp = strings.Replace(postgresApp,
	`command: ["sh", "-c", "mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 8080 -h /www"]`,
	`command: ["sh", "-c", "mkdir -p /www && printf 'url=%s\nhost=%s\npr=%s\n' \"$$KOROKU_APP_URL\" \"$$KOROKU_APP_HOST\" \"$$KOROKU_PR_NUMBER\" > /www/index.html && exec httpd -f -p 8080 -h /www"]`, 1)

// TestReviewAppGetsItsURLAndTheParentsData deploys a review app with real
// containers (ARCHITECTURE 9.9): its containers see their own URL and the
// pull request, and once the first deployment succeeded the review watcher
// copies the parent's database into it and runs the post-load command.
// The pull request itself (GitHub) is covered by the API tests; here the
// review app is made in the store from a local repository.
func TestReviewAppGetsItsURLAndTheParentsData(t *testing.T) {
	t.Parallel()
	if !strings.Contains(reviewApp, "KOROKU_PR_NUMBER") {
		t.Fatal("the review app's compose file was not derived from postgresApp")
	}
	e := newEnv(t)
	ctx := context.Background()
	repo := newRepo(t, "")
	repo.write("compose.yaml", reviewApp)
	sha := repo.commit("Review app")
	vars := map[string]string{
		"POSTGRES_USER": "app", "POSTGRES_DB": "app",
		"POSTGRES_PASSWORD": "review-test-password-1", "PGPASSWORD": "review-test-password-1",
	}

	parentSlug := e.slug("rvparent")
	e.createApp(parentSlug, repo.url())
	e.setEnv(parentSlug, vars)
	if status := e.call(http.MethodPatch, "/api/apps/"+parentSlug, map[string]any{
		"dump_hook_service": "db", "dump_hook_command": `pg_dump -Fc -U "$POSTGRES_USER" "$POSTGRES_DB"`,
		"load_hook_service": "db", "load_hook_command": `pg_restore --clean --if-exists --no-owner -U "$POSTGRES_USER" -d "$POSTGRES_DB"`,
		"post_load_service": "worker", "post_load_command": `psql -h db -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "UPDATE users SET email = 'user' || id || '@example.invalid'"`,
	}, nil); status != http.StatusOK {
		t.Fatalf("patch parent: %d", status)
	}
	if d := e.deploy(parentSlug, map[string]string{"sha": sha}); d.Status != "succeeded" {
		t.Fatalf("deploy parent: %s %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	psql(t, parentSlug, "CREATE TABLE users (id int PRIMARY KEY, email text); "+
		"INSERT INTO users VALUES (1, 'alice@example.com'), (2, 'bob@example.com')")

	parent, err := e.db.GetApp(ctx, parentSlug)
	if err != nil {
		t.Fatal(err)
	}
	reviewSlug := e.slug("rvpr")
	in := parent
	in.Slug = reviewSlug
	if _, err := e.db.CreateReviewApp(ctx, store.NewReviewApp{App: in, Review: store.ReviewApp{
		ParentAppID: parent.ID, PRNumber: 12, PRTitle: "Add search", HeadBranch: "main",
		DataCopy: store.DataCopyWaiting,
	}}, parent.HostPort, parent.HostPort+portSpan, time.Now()); err != nil {
		t.Fatal(err)
	}
	e.setEnv(reviewSlug, vars)

	watcher := review.New(review.Watcher{Store: e.db, Copier: e.engine})
	// Nothing happens before the first deployment.
	watcher.StartCopies(ctx)
	if copies, _ := e.db.ListDBCopies(ctx, mustApp(t, e, reviewSlug).ID, 5); len(copies) != 0 {
		t.Fatalf("copied before the deployment: %+v", copies)
	}

	d := e.deploy(reviewSlug, map[string]string{"sha": sha})
	if d.Status != "succeeded" {
		t.Fatalf("deploy review app: %s %s\n%s", d.Status, d.Error, e.logText(d.ID))
	}
	reviewApp := mustApp(t, e, reviewSlug)
	got := page(t, reviewApp.HostPort)
	if got["url"] != "https://"+reviewSlug+".dev.example.com" || got["host"] != reviewSlug+".dev.example.com" || got["pr"] != "12" {
		t.Errorf("page = %v", got)
	}

	watcher.StartCopies(ctx)
	r, err := e.db.GetReviewApp(ctx, reviewApp.ID)
	if err != nil || r.DataCopy != store.DataCopyStarted {
		t.Fatalf("review = %+v, %v", r, err)
	}
	var c store.DBCopy
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		copies, err := e.db.ListDBCopies(ctx, reviewApp.ID, 5)
		if err != nil || len(copies) != 1 {
			t.Fatalf("copies = %+v, %v", copies, err)
		}
		if c = copies[0]; c.Status != store.CopyRunning {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	log := e.copyLog(c.ID)
	if c.Status != store.CopySucceeded || c.BackupSnapshotID != "" {
		t.Fatalf("copy = %+v\n%s", c, log)
	}
	if !strings.Contains(log, "was just created; there is no data to back up first") {
		t.Errorf("log:\n%s", log)
	}
	if got := psql(t, reviewSlug, "SELECT id || ' ' || email FROM users ORDER BY id"); got != "1 user1@example.invalid\n2 user2@example.invalid" {
		t.Errorf("review app rows = %q\n%s", got, log)
	}
}

func mustApp(t *testing.T, e *env, slug string) store.App {
	t.Helper()
	app, err := e.db.GetApp(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	return app
}
