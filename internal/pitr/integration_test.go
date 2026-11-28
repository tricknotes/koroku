//go:build integration

package pitr

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tricknotes/koroku/internal/backup"
	"github.com/tricknotes/koroku/internal/runner"
	"github.com/tricknotes/koroku/internal/secrets"
	"github.com/tricknotes/koroku/internal/store"
)

// directEngine runs everything at once: the app has no other operation.
type directEngine struct{}

func (directEngine) WithAppLock(ctx context.Context, _ int64, fn func(context.Context) error) error {
	return fn(ctx)
}

func (directEngine) WithServiceRunning(ctx context.Context, _ int64, _ string, fn func(context.Context) error) error {
	return fn(ctx)
}

func (directEngine) OutputMasker(context.Context, store.App) (*secrets.Masker, error) {
	return secrets.NewMasker(), nil
}

const itCompose = `services:
  db:
    image: postgres:17.10-alpine
    environment:
      POSTGRES_USER: app
      POSTGRES_DB: app
      POSTGRES_PASSWORD: pitr-test-password-1
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

type site struct {
	t       *testing.T
	ctx     context.Context
	dir     string
	walDir  string
	project string
	svc     *Service
	st      *store.Store
	app     store.App
}

func randomHex(t *testing.T) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// newSite starts a PostgreSQL service as koroku runs it, with the archive
// directory mounted, and a Service on a local restic repository.
func newSite(t *testing.T) *site {
	t.Helper()
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic is not on PATH")
	}
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose is not available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	// The archive directory is bind-mounted by the host's Docker daemon:
	// it must be at a path the host sees, which /tmp of the development
	// container is not. data/ of the checkout is.
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Fatalf("find the checkout: %v", err)
	}
	base := filepath.Join(strings.TrimSpace(string(root)), "data", "it")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	shared, err := os.MkdirTemp(base, "pitr-")
	if err != nil {
		t.Fatal(err)
	}
	s := &site{t: t, ctx: ctx, dir: shared, walDir: filepath.Join(shared, "wal"), project: "koroku-pitrit" + randomHex(t)}
	if err := os.MkdirAll(s.walDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "compose.yaml"), []byte(itCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "wal.yaml"), []byte("services:\n  db:\n    volumes:\n      - type: bind\n        source: "+s.walDir+"\n        target: /koroku/wal\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("docker", "compose", "-p", s.project, "down", "--volumes", "--timeout", "1").Run()
		// The files belong to the container's postgres user.
		_ = exec.Command("docker", "run", "--rm", "-v", base+":/base", "alpine", "rm", "-rf", "/base/"+filepath.Base(shared)).Run()
	})
	s.up(true)

	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app, err := st.CreateApp(ctx, store.App{
		Slug: strings.TrimPrefix(s.project, "koroku-"), RepoFullName: "octo/pitr", DefaultBranch: "main",
		WebService: "web", WebPort: 3000, HealthcheckPath: "/",
	}, 20000, 20999, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	app.PITRService = "db"
	app.DesiredState = store.DesiredRunning
	if err := st.UpdateApp(ctx, app, time.Now()); err != nil {
		t.Fatal(err)
	}
	s.st, s.app = st, app

	repoDir := t.TempDir()
	password := filepath.Join(repoDir, "restic.key")
	if err := os.WriteFile(password, []byte("pitr-test\n"), 0o600); err != nil {
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
	s.svc = New(Options{Store: st, Repo: manager, Engine: directEngine{}, Runner: runner.Exec{}})
	return s
}

// up starts the service, with or without the archive directory.
func (s *site) up(mounted bool) {
	s.t.Helper()
	args := []string{"compose", "-p", s.project, "-f", filepath.Join(s.dir, "compose.yaml")}
	if mounted {
		args = append(args, "-f", filepath.Join(s.dir, "wal.yaml"))
	}
	args = append(args, "up", "-d", "--wait", "--force-recreate")
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		s.t.Fatalf("docker compose up: %v\n%s", err, out)
	}
}

func (s *site) psql(query string) string {
	s.t.Helper()
	out, err := exec.Command("docker", "compose", "-p", s.project, "exec", "-T", "db",
		"psql", "-U", "app", "-d", "app", "-tA", "-c", query).CombinedOutput()
	if err != nil {
		s.t.Fatalf("psql %q: %v\n%s", query, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (s *site) waiting() []string {
	s.t.Helper()
	out, err := exec.Command("docker", "compose", "-p", s.project, "exec", "-T", "db", "ls", "/koroku/wal").CombinedOutput()
	if err != nil {
		s.t.Fatalf("ls: %v\n%s", err, out)
	}
	return strings.Fields(string(out))
}

// TestIntegrationArchiving prepares a PostgreSQL service, takes its base
// backup, ships its WAL, and records the period its release does not
// mount the archive directory (ARCHITECTURE 10.9).
func TestIntegrationArchiving(t *testing.T) {
	s := newSite(t)
	if err := s.svc.Prepare(s.ctx, s.app); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	st, err := s.st.GetPITRState(s.ctx, s.app.ID)
	if err != nil || st.Status != store.PITRReady || st.DataDirectory != "/var/lib/postgresql/data" || st.ServerVersionNum/10000 != 17 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if got := s.psql("SHOW archive_mode"); got != "on" {
		t.Errorf("archive_mode = %q", got)
	}
	bases, err := s.st.ListPITRBases(s.ctx, s.app.ID, time.Time{})
	if err != nil || len(bases) != 1 || bases[0].SnapshotID == "" || bases[0].Timeline != 1 || len(bases[0].StartSegment) != 24 {
		t.Fatalf("bases = %+v, %v", bases, err)
	}
	// Preparing again changes nothing and restarts nothing.
	started := s.psql("SELECT pg_postmaster_start_time()")
	if err := s.svc.Prepare(s.ctx, s.app); err != nil {
		t.Fatalf("Prepare again: %v", err)
	}
	if got := s.psql("SELECT pg_postmaster_start_time()"); got != started {
		t.Errorf("the second preparation restarted the database")
	}

	s.psql("CREATE TABLE t (v text); INSERT INTO t VALUES ('a')")
	s.psql("SELECT pg_switch_wal()")
	deadline := time.Now().Add(30 * time.Second)
	for len(s.waiting()) == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if len(s.waiting()) == 0 {
		t.Fatal("no WAL reached the archive directory")
	}
	if err := s.svc.Archive(s.ctx, s.app); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	batches, err := s.st.ListPITRWALBatches(s.ctx, s.app.ID, time.Time{})
	if err != nil || len(batches) != 1 || batches[0].FileCount == 0 {
		t.Fatalf("batches = %+v, %v", batches, err)
	}
	for _, name := range s.waiting() {
		if !strings.HasSuffix(name, ".tmp") {
			t.Errorf("%s stayed in the archive directory after it was shipped", name)
		}
	}
	status, err := s.svc.Status(s.ctx, s.app)
	if err != nil || status.State != StateReady || status.EarliestAt == nil || status.ArchivedUntil == nil {
		t.Fatalf("status = %+v, %v", status, err)
	}

	// A release without the mount: the database keeps running, and the
	// period is recorded.
	s.up(false)
	s.psql("INSERT INTO t VALUES ('b'); SELECT pg_switch_wal()")
	if err := s.svc.Archive(s.ctx, s.app); err != nil {
		t.Fatalf("Archive without the mount: %v", err)
	}
	status, _ = s.svc.Status(s.ctx, s.app)
	if len(status.Gaps) != 1 || status.Gaps[0].Reason != store.PITRGapMountMissing || status.Gaps[0].EndedAt != nil || status.Warning == "" {
		t.Fatalf("status without the mount = %+v", status)
	}
	if got := s.psql("SELECT failed_count FROM pg_stat_archiver"); got != "0" {
		t.Errorf("archiving failed without the mount: failed_count = %s", got)
	}

	s.up(true)
	if err := s.svc.Archive(s.ctx, s.app); err != nil {
		t.Fatalf("Archive with the mount again: %v", err)
	}
	status, _ = s.svc.Status(s.ctx, s.app)
	if len(status.Gaps) != 1 || status.Gaps[0].EndedAt == nil || status.Warning != "" {
		t.Errorf("status with the mount again = %+v", status)
	}
	s.svc.takeBases(s.ctx)
	if bases, _ := s.st.ListPITRBases(s.ctx, s.app.ID, time.Time{}); len(bases) != 2 {
		t.Errorf("%d base backups; the mount coming back asks for a new one", len(bases))
	}
}
