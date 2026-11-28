//go:build integration

package pitr_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/clock"
	"github.com/tricknotes/koroku/internal/deploy"
	"github.com/tricknotes/koroku/internal/gitrepo"
	"github.com/tricknotes/koroku/internal/pitr"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
	"github.com/tricknotes/koroku/internal/system"
)

const restoreApp = `services:
  web:
    image: busybox:1.37
    stop_signal: SIGKILL
    command: ["sh", "-c", "mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 8080 -h /www"]
  db:
    image: postgres:17.10-alpine
    environment:
      POSTGRES_USER: app
      POSTGRES_DB: app
      POSTGRES_PASSWORD: pitr-restore-password-1
    volumes:
      - pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -h 127.0.0.1 -U app -d app"]
      interval: 1s
      timeout: 5s
      retries: 60
volumes:
  pgdata:
`

// snapshots reads the snapshots of point-in-time recovery from a backup
// manager, as cmd/koroku does.
type snapshots struct{ m *backup.Manager }

func (s snapshots) app(app store.App) backup.App {
	return backup.App{Slug: app.Slug, Project: app.ProjectName()}
}

func (s snapshots) WriteBase(ctx context.Context, app store.App, id string, w io.Writer) error {
	return s.m.WriteStream(ctx, s.app(app), backup.KindPITRBase, id, w)
}

func (s snapshots) WriteWAL(ctx context.Context, app store.App, id string, w io.Writer) error {
	return s.m.WriteStream(ctx, s.app(app), backup.KindWAL, id, w)
}

type hooks struct{ svc *pitr.Service }

func (h *hooks) RequestBase(id int64) {
	if h.svc != nil {
		h.svc.RequestBase(id)
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestIntegrationRestoreToAPointInTime deploys a PostgreSQL app with
// point-in-time recovery, writes rows around a time, and restores the
// database to that time through the deploy engine (ARCHITECTURE 10.9):
// the rows written by then are there, the later ones are not, and WAL
// that never left the host counts too.
func TestIntegrationRestoreToAPointInTime(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not on PATH")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// The apps directory holds the archive directory the host's Docker
	// daemon mounts: under data/ of the checkout, which it sees.
	root := git(t, ".", "rev-parse", "--show-toplevel")
	base := filepath.Join(root, "data", "it")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, "pitr-restore-")
	if err != nil {
		t.Fatal(err)
	}
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	slug := "pitrr" + hex.EncodeToString(suffix)
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "-p", "koroku-"+slug, "down", "--volumes", "--timeout", "1").Run()
		_ = exec.Command("docker", "run", "--rm", "-v", base+":/base", "alpine", "rm", "-rf", "/base/"+filepath.Base(dir)).Run()
	})

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "compose.yaml"), []byte(restoreApp), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "--quiet", "--initial-branch", "main")
	git(t, repo, "add", "--all")
	git(t, repo, "commit", "--quiet", "-m", "PostgreSQL app")
	sha := git(t, repo, "rev-parse", "HEAD")

	st, err := store.Open(ctx, filepath.Join(dir, "koroku.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	key := make([]byte, secrets.KeySize)
	_, _ = rand.Read(key)
	box, _ := secrets.NewBox(key)

	repoDir := t.TempDir()
	password := filepath.Join(repoDir, "restic.key")
	if err := os.WriteFile(password, []byte("pitr-restore\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := backup.New(backup.Options{
		Repository: backup.Repository{Path: filepath.Join(repoDir, "repo"), PasswordFile: password, CacheDir: filepath.Join(repoDir, "cache")},
		Runner:     runner.Exec{},
		Apps:       func(context.Context) ([]backup.App, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &hooks{}
	run := runner.Exec{}
	engine := deploy.New(deploy.Options{
		Store: st, Runner: run, Clock: clock.System{}, Box: box,
		System:  system.Host{DataDir: dir, Runner: run},
		Fetcher: gitrepo.Fetcher{Runner: run, AllowFileURLs: true},
		AppsDir: filepath.Join(dir, "apps"), BaseDomain: "dev.example.com",
		DiskLimitPercent: 100, WaitTimeout: 90 * time.Second, HealthTimeout: 20 * time.Second,
		PITRSnapshots: snapshots{manager}, PITR: h, TrashDir: filepath.Join(dir, "restore-trash"),
	})
	go func() { _ = engine.Run(ctx) }()
	svc := pitr.New(pitr.Options{Store: st, Repo: manager, Engine: engine, Runner: run})
	h.svc = svc

	app, err := st.CreateApp(ctx, store.App{
		Slug: slug, RepoURL: "file://" + repo, DefaultBranch: "main", ComposePath: "compose.yaml",
		WebService: "web", WebPort: 8080, HealthcheckPath: "/",
	}, 28000, 28999, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	app.PITRService = "db"
	if err := st.UpdateApp(ctx, app, time.Now()); err != nil {
		t.Fatal(err)
	}
	d, err := engine.Enqueue(ctx, app, "main", sha, "PostgreSQL app")
	if err != nil {
		t.Fatal(err)
	}
	for d.Status == store.StatusQueued || d.Status == store.StatusRunning {
		time.Sleep(300 * time.Millisecond)
		if d, err = st.GetDeployment(ctx, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	if d.Status != store.StatusSucceeded {
		log, _ := os.ReadFile(engine.LogPath(slug, d.ID))
		t.Fatalf("deployment %s: %s\n%s", d.Status, d.Error, log)
	}
	if app, err = st.GetApp(ctx, slug); err != nil {
		t.Fatal(err)
	}
	if err := svc.Prepare(ctx, app); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	psql := func(query string) string {
		t.Helper()
		out, err := exec.Command("docker", "compose", "-p", "koroku-"+slug, "exec", "-T", "db",
			"psql", "-U", "app", "-d", "app", "-tA", "-c", query).CombinedOutput()
		if err != nil {
			t.Fatalf("psql %q: %v\n%s", query, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	psql("CREATE TABLE posts (title text); INSERT INTO posts VALUES ('first'); SELECT pg_switch_wal()")
	time.Sleep(2 * time.Second)
	if err := svc.Archive(ctx, app); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	// 'second' only reaches the host's archive directory and the data
	// directory's pg_wal; it is never shipped.
	psql("INSERT INTO posts VALUES ('second')")
	time.Sleep(1500 * time.Millisecond)
	target := time.Now().UTC()
	time.Sleep(1500 * time.Millisecond)
	// Right after the time: a row is deleted by mistake and another
	// written.
	psql("DELETE FROM posts WHERE title = 'first'; INSERT INTO posts VALUES ('third')")

	if _, err := engine.StartPITRRestore(ctx, app, time.Now().Add(time.Hour)); !errors.Is(err, pitr.ErrFutureTarget) {
		t.Errorf("a time in the future: %v", err)
	}
	if _, err := engine.StartPITRRestore(ctx, app, time.Now().Add(-time.Hour)); !errors.Is(err, pitr.ErrNoBase) {
		t.Errorf("a time before the base backup: %v", err)
	}

	r, err := engine.StartPITRRestore(ctx, app, target)
	if err != nil {
		t.Fatalf("StartPITRRestore: %v", err)
	}
	for r.Status == "running" {
		time.Sleep(500 * time.Millisecond)
		if r, err = st.GetPITRRestore(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	log, _ := os.ReadFile(engine.PITRRestoreLogPath(slug, r.ID))
	if r.Status != "succeeded" {
		t.Fatalf("restore %s: %s\n%s", r.Status, r.Error, log)
	}
	if got := psql("SELECT string_agg(title, ',' ORDER BY title) FROM posts"); got != "first,second" {
		t.Errorf("rows after the restore = %q\n%s", got, log)
	}
	if got := psql("SELECT timeline_id FROM pg_control_checkpoint()"); got != "2" {
		t.Errorf("timeline = %s", got)
	}
	if got := psql("SHOW restore_command"); got != "" {
		t.Errorf("restore_command left behind: %q", got)
	}
	out, err := exec.Command("docker", "compose", "-p", "koroku-"+slug, "ps", "--status", "running", "--format", "{{.Service}}").Output()
	if err != nil || !strings.Contains(string(out), "web") {
		t.Errorf("web does not run after the restore: %q, %v", out, err)
	}
	if app, _ = st.GetApp(ctx, slug); app.DesiredState != store.DesiredRunning {
		t.Errorf("desired state = %s", app.DesiredState)
	}
}
